package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// Outbound SMTP transport (FR50, ADR-17).
//
// Implements the Transport interface against an upstream SMTP relay
// (Mailgun's SMTP gateway, a self-hosted Postfix relay, Mailtrap, etc.)
// using stdlib `net/smtp` primitives — no third-party SMTP library
// dependency. STARTTLS-enabled by default; SMTP AUTH PLAIN.
//
// Each Send opens a fresh TCP+TLS connection, runs the full SMTP
// transaction (EHLO → STARTTLS → AUTH → MAIL → RCPT → DATA → QUIT),
// then closes. Connection reuse is a v2 optimization once we have
// async sending and persistent state.
//
// NFR1 enforcement: SMTP transport hand-writes the RFC 5322 message
// envelope (From, To, Subject, Reply-To headers) into the DATA section
// because there's no JSON struct to lean on. The structural defense is
// (a) pre-write validation that submitter-controlled header values
// contain no CR/LF (rejects header-injection attempts at Send time with
// ErrTerminal), (b) stdlib net/smtp's own validateLine check on MAIL FROM
// and RCPT TO (CRLF rejected at the protocol level), and (c) net/textproto
// dot-stuffing inside Data() which translates `\n` → `\r\n` and escapes
// lone `.` lines to `..` so body content can't smuggle headers via the
// data-end terminator.

const (
	// smtpRequestTimeout is the per-Send hard cap on the SMTP transaction.
	// Longer than HTTP transports because SMTP often has slower greeting
	// and DATA-acceptance round trips.
	smtpRequestTimeout = 30 * time.Second
)

// SMTPOutTransport implements Transport against an upstream SMTP relay.
type SMTPOutTransport struct {
	Host     string
	Port     int
	Username string
	Password string

	// RequireTLS gates whether STARTTLS is mandatory. Default true. When
	// true and the server doesn't advertise STARTTLS, Send fails with
	// ErrTerminal — a misconfiguration the operator must fix.
	RequireTLS bool

	// TLSInsecureSkipVerify disables certificate validation on STARTTLS.
	// Test-only escape hatch for self-signed certs in local development;
	// production operators must leave this false and use a real cert.
	TLSInsecureSkipVerify bool

	// dial is the connection factory. Production: default uses
	// net.Dialer.DialContext + smtp.NewClient. Tests inject a function
	// that returns a client connected to a fake SMTP server.
	dial func(ctx context.Context, addr string) (*smtp.Client, error)

	// now is injectable for deterministic Date headers in tests.
	now func() time.Time
}

// NewSMTPOutTransport constructs an outbound SMTP transport. host and port
// identify the upstream relay; username/password are the SMTP AUTH PLAIN
// credentials; requireTLS controls STARTTLS enforcement.
func NewSMTPOutTransport(host string, port int, username, password string, requireTLS bool) *SMTPOutTransport {
	return &SMTPOutTransport{
		Host:       host,
		Port:       port,
		Username:   username,
		Password:   password,
		RequireTLS: requireTLS,
		dial:       defaultSMTPDial,
		now:        time.Now,
	}
}

// defaultSMTPDial is the production dial: open TCP, wrap in smtp.Client.
func defaultSMTPDial(ctx context.Context, addr string) (*smtp.Client, error) {
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return c, nil
}

// Send implements Transport.
//
// Error class mapping:
//
//	connect/network/timeout         → ErrTransient
//	STARTTLS missing when required  → ErrTerminal (config bug, not transient)
//	AUTH rejected                   → ErrTerminal (bad creds)
//	MAIL/RCPT/DATA 4xx              → ErrTransient (greylisting, temporary)
//	MAIL/RCPT/DATA 5xx              → ErrTerminal (relay-rejected, bad address)
//	CRLF in header value            → ErrTerminal (NFR1 reject at our edge)
func (s *SMTPOutTransport) Send(ctx context.Context, msg Message) (SendResult, error) {
	// NFR1: reject CRLF in submitter-controlled header values before we
	// hand-write them into the DATA blob. Catches header-injection
	// payloads at our edge with a clear error.
	if err := validateNoHeaderCRLF(msg); err != nil {
		return SendResult{}, &TransportError{
			Class:   ErrTerminal,
			Cause:   err,
			Message: "smtp: header injection attempt rejected",
		}
	}

	// Establish per-request deadline. The operator's per-endpoint timeout
	// (FR22) is the outer bound; smtpRequestTimeout is the safety net for
	// unbounded protocol stalls.
	deadlineCtx, cancel := context.WithTimeout(ctx, smtpRequestTimeout)
	defer cancel()

	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	c, err := s.dial(deadlineCtx, addr)
	if err != nil {
		return SendResult{}, &TransportError{Class: ErrTransient, Cause: err, Message: "smtp: dial failed"}
	}
	defer func() { _ = c.Close() }()

	// EHLO/HELO — required before any other command. The hostname we
	// announce is informational; "localhost" is conventional for
	// outbound-relay clients.
	if err := c.Hello("localhost"); err != nil {
		return SendResult{}, classifySMTPError(err, "smtp: EHLO failed")
	}

	// STARTTLS upgrade. Enforced when RequireTLS is true — if the server
	// doesn't advertise the extension, we refuse to continue (treating
	// plaintext as an operator-side config error, not a transient issue).
	if s.RequireTLS {
		ok, _ := c.Extension("STARTTLS")
		if !ok {
			return SendResult{}, &TransportError{
				Class:   ErrTerminal,
				Message: "smtp: server does not advertise STARTTLS but require_tls=true",
			}
		}
		tlsConfig := &tls.Config{
			ServerName:         s.Host,
			InsecureSkipVerify: s.TLSInsecureSkipVerify,
		}
		if err := c.StartTLS(tlsConfig); err != nil {
			return SendResult{}, classifySMTPError(err, "smtp: STARTTLS upgrade failed")
		}
	}

	// AUTH PLAIN. stdlib's PlainAuth refuses to send credentials over an
	// unencrypted link unless the server explicitly advertises AUTH PLAIN,
	// and verifies the server hostname matches what we configured.
	if s.Username != "" || s.Password != "" {
		auth := smtp.PlainAuth("", s.Username, s.Password, s.Host)
		if err := c.Auth(auth); err != nil {
			// Auth errors are typically bad creds; classify as terminal.
			return SendResult{}, classifySMTPError(err, "smtp: AUTH failed")
		}
	}

	// Envelope: MAIL FROM uses the address-only portion (no display name).
	envFrom, err := envelopeAddress(msg.From)
	if err != nil {
		return SendResult{}, &TransportError{Class: ErrTerminal, Cause: err, Message: "smtp: invalid from address"}
	}
	if err := c.Mail(envFrom); err != nil {
		return SendResult{}, classifySMTPError(err, "smtp: MAIL FROM rejected")
	}
	for _, to := range msg.To {
		envTo, err := envelopeAddress(to)
		if err != nil {
			return SendResult{}, &TransportError{Class: ErrTerminal, Cause: err, Message: "smtp: invalid to address"}
		}
		if err := c.Rcpt(envTo); err != nil {
			return SendResult{}, classifySMTPError(err, "smtp: RCPT TO rejected")
		}
	}

	// DATA: write the full RFC 5322 message (headers + body). textproto's
	// DotWriter handles dot-stuffing and \n→\r\n translation automatically.
	w, err := c.Data()
	if err != nil {
		return SendResult{}, classifySMTPError(err, "smtp: DATA command rejected")
	}
	payload := s.buildRFC5322(msg)
	if _, err := w.Write(payload); err != nil {
		return SendResult{}, &TransportError{Class: ErrTransient, Cause: err, Message: "smtp: write payload failed"}
	}
	if err := w.Close(); err != nil {
		return SendResult{}, classifySMTPError(err, "smtp: DATA finalize failed")
	}

	// QUIT is best-effort. A failed QUIT after a successful DATA-close
	// doesn't mean the message wasn't accepted — the server already
	// returned 250 to the dot.
	_ = c.Quit()

	// MessageID: stdlib net/smtp doesn't expose the server's
	// "queued as <id>" response, so SendResult.MessageID stays empty for
	// SMTP — the operator must correlate via timestamp + recipient in the
	// relay's own logs.
	return SendResult{}, nil
}

// validateNoHeaderCRLF rejects any submitter-controlled header value that
// contains a CR or LF — the structural defense against header injection in
// the hand-written RFC 5322 DATA blob (NFR1).
func validateNoHeaderCRLF(msg Message) error {
	if strings.ContainsAny(msg.From, "\r\n") {
		return errors.New("from contains CR or LF")
	}
	if strings.ContainsAny(msg.ReplyTo, "\r\n") {
		return errors.New("reply-to contains CR or LF")
	}
	if strings.ContainsAny(msg.Subject, "\r\n") {
		return errors.New("subject contains CR or LF")
	}
	for i, to := range msg.To {
		if strings.ContainsAny(to, "\r\n") {
			return fmt.Errorf("to[%d] contains CR or LF", i)
		}
	}
	return nil
}

// envelopeAddress extracts the bare email portion of an RFC 5322 address
// for use in SMTP MAIL FROM / RCPT TO. The display name is dropped — the
// envelope address is just the addr-spec.
//
// Examples:
//
//	"craig@example.com"               → "craig@example.com"
//	"Craig <craig@example.com>"       → "craig@example.com"
//	"\"Craig Mc\" <craig@example.com>" → "craig@example.com"
func envelopeAddress(addr string) (string, error) {
	parsed, err := mail.ParseAddress(addr)
	if err != nil {
		return "", fmt.Errorf("parse address %q: %w", addr, err)
	}
	return parsed.Address, nil
}

// buildRFC5322 constructs the message body sent in DATA. Headers use
// CRLF line endings (RFC 5322); the body's line endings are converted
// by textproto.DotWriter inside Data().
func (s *SMTPOutTransport) buildRFC5322(msg Message) []byte {
	var buf bytes.Buffer
	writeHeader(&buf, "From", msg.From)
	writeHeader(&buf, "To", strings.Join(msg.To, ", "))
	if msg.ReplyTo != "" {
		writeHeader(&buf, "Reply-To", msg.ReplyTo)
	}
	writeHeader(&buf, "Subject", encodeMIMESubject(msg.Subject))
	writeHeader(&buf, "Date", s.now().UTC().Format(time.RFC1123Z))
	writeHeader(&buf, "MIME-Version", "1.0")
	writeHeader(&buf, "Content-Type", "text/plain; charset=\"utf-8\"")
	writeHeader(&buf, "Content-Transfer-Encoding", "8bit")
	buf.WriteString("\r\n")
	buf.WriteString(msg.BodyText)
	return buf.Bytes()
}

// writeHeader writes a single `Name: Value\r\n` header line. Caller has
// already CRLF-validated the value (validateNoHeaderCRLF).
func writeHeader(buf *bytes.Buffer, name, value string) {
	buf.WriteString(name)
	buf.WriteString(": ")
	buf.WriteString(value)
	buf.WriteString("\r\n")
}

// encodeMIMESubject applies RFC 2047 B-encoding when the subject contains
// non-ASCII bytes. ASCII-only subjects pass through unchanged. The
// encoder handles word-splitting per the 76-character soft limit.
func encodeMIMESubject(subject string) string {
	if !needsMIMEEncoding(subject) {
		return subject
	}
	return mime.BEncoding.Encode("utf-8", subject)
}

func needsMIMEEncoding(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return true
		}
	}
	return false
}

// classifySMTPError maps an error from a net/smtp.Client command to a
// TransportError with the right ErrorClass. SMTP protocol errors come
// back as *textproto.Error which exposes the response code directly.
//
// Mapping:
//
//	4xx codes  → ErrTransient (server-side temporary; greylisting, etc.)
//	5xx codes  → ErrTerminal  (relay-rejected, bad address, AUTH failure)
//	non-SMTP   → ErrTransient (network/timeout/EOF; retry once)
func classifySMTPError(err error, contextMsg string) *TransportError {
	if err == nil {
		return nil
	}
	var tperr *textproto.Error
	if errors.As(err, &tperr) {
		class := ErrTransient
		if tperr.Code >= 500 {
			class = ErrTerminal
		}
		return &TransportError{
			Class:   class,
			Status:  tperr.Code,
			Cause:   err,
			Message: contextMsg,
		}
	}
	// I/O error, connection drop, timeout — transient.
	return &TransportError{
		Class:   ErrTransient,
		Cause:   err,
		Message: contextMsg,
	}
}

var _ Transport = (*SMTPOutTransport)(nil)

// Registry registration.
func init() {
	Register(Registration{
		Type:     "smtp",
		Validate: validateSMTPOutSettings,
		Build:    buildSMTPOutFromSettings,
	})
}

func validateSMTPOutSettings(settings map[string]any) error {
	host, ok := settings["host"].(string)
	if !ok || host == "" {
		return fmt.Errorf("smtp transport requires settings.host")
	}
	// port can be int or int64 depending on TOML's parse; accept either.
	portRaw, ok := settings["port"]
	if !ok {
		return fmt.Errorf("smtp transport requires settings.port")
	}
	port, ok := coerceInt(portRaw)
	if !ok || port <= 0 || port > 65535 {
		return fmt.Errorf("smtp transport settings.port must be 1-65535, got %v", portRaw)
	}
	if username, ok := settings["username"].(string); !ok || username == "" {
		return fmt.Errorf("smtp transport requires settings.username")
	}
	if password, ok := settings["password"].(string); !ok || password == "" {
		return fmt.Errorf("smtp transport requires settings.password")
	}
	return nil
}

func buildSMTPOutFromSettings(settings map[string]any) (Transport, error) {
	host, _ := settings["host"].(string)
	port, _ := coerceInt(settings["port"])
	username, _ := settings["username"].(string)
	password, _ := settings["password"].(string)
	requireTLS := true
	if v, ok := settings["require_tls"].(bool); ok {
		requireTLS = v
	}
	insecureSkipVerify := false
	if v, ok := settings["tls_insecure_skip_verify"].(bool); ok {
		insecureSkipVerify = v
	}
	if host == "" || port <= 0 || username == "" || password == "" {
		return nil, fmt.Errorf("smtp: host, port, username, password all required")
	}
	tp := NewSMTPOutTransport(host, port, username, password, requireTLS)
	tp.TLSInsecureSkipVerify = insecureSkipVerify
	return tp, nil
}

// coerceInt handles the int / int64 / float64 polymorphism TOML decoders
// can produce depending on the value's representation in the file.
func coerceInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}
