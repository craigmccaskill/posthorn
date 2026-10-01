package smtp

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/textproto"
	"strings"
	"time"

	uuid "github.com/google/uuid"

	"github.com/craigmccaskill/posthorn/storage"
	"github.com/craigmccaskill/posthorn/transport"
)

// sendTimeout is the hard upper bound on a single DATA submission's
// outbound send, including any retries — the same FR22 bound the HTTP
// gateway applies via its requestTimeout. Declared as a var so tests
// can override; production never mutates it.
var sendTimeout = 10 * time.Second

// session is the per-connection SMTP state machine.
//
// State transitions (RFC 5321-shaped, simplified to what Posthorn needs):
//
//	greeted    → TLS pending | AUTH | MAIL FROM ([require_tls=false] only)
//	TLS pending → established (after STARTTLS handshake)
//	established → AUTH | MAIL FROM ([if auth not required]) | client-cert-authed
//	authed     → MAIL FROM
//	in trx    → RCPT TO | DATA | RSET
//	in data   → 250 OK (after `.`) | reset on error
//
// Invalid command sequences return 503 5.5.1 "Bad sequence of commands"
// without leaving the current state.
type session struct {
	l      *Listener
	conn   net.Conn
	tp     *textproto.Conn
	logger *slog.Logger
	id     string

	// Negotiated state.
	tlsActive     bool
	authedUser    string // empty until auth completes
	authedViaCert bool

	// Per-transaction state. Resets on RSET, MAIL FROM, or after
	// successful DATA.
	mailFrom    string
	rcptTo      []string
	transaction bool
}

func newSession(conn net.Conn, l *Listener) *session {
	id := uuid.NewString()
	logger := l.logger.With(
		slog.String("session_id", id),
		slog.String("ingress", "smtp"),
		slog.String("remote_addr", conn.RemoteAddr().String()),
	)
	return &session{
		l:      l,
		conn:   conn,
		tp:     textproto.NewConn(conn),
		logger: logger,
		id:     id,
	}
}

// run executes the state machine until the client QUITs or the
// connection closes / times out.
func (s *session) run() {
	s.logger.Info("smtp_session_open",
		slog.String("tls", "no"),
	)

	if err := s.writeReply(220, "posthorn ready"); err != nil {
		return
	}

	for {
		if err := s.conn.SetReadDeadline(s.l.idleDeadline(time.Now())); err != nil {
			return
		}
		line, err := s.tp.ReadLine()
		if err != nil {
			return
		}
		cmd, arg := splitCommand(line)
		switch strings.ToUpper(cmd) {
		case "EHLO":
			s.handleEHLO(arg)
		case "HELO":
			s.handleHELO(arg)
		case "STARTTLS":
			if s.handleSTARTTLS() {
				continue
			}
		case "AUTH":
			if s.handleAUTH(arg) {
				// Brute-force budget exhausted (#50); 421 already sent.
				s.logger.Info("smtp_session_close", slog.String("reason", "auth_rate_limited"))
				return
			}
		case "MAIL":
			s.handleMAIL(arg)
		case "RCPT":
			s.handleRCPT(arg)
		case "DATA":
			s.handleDATA()
		case "RSET":
			s.resetTransaction()
			_ = s.writeReply(250, "2.0.0 OK")
		case "NOOP":
			_ = s.writeReply(250, "2.0.0 OK")
		case "QUIT":
			_ = s.writeReply(221, "2.0.0 Bye")
			s.logger.Info("smtp_session_close", slog.String("reason", "quit"))
			return
		default:
			_ = s.writeReply(500, "5.5.1 Command unrecognized")
		}
	}
}

func (s *session) writeReply(code int, msg string) error {
	return s.tp.PrintfLine("%d %s", code, msg)
}

func (s *session) writeMultilineReply(code int, lines []string) error {
	for i, line := range lines {
		sep := "-"
		if i == len(lines)-1 {
			sep = " "
		}
		if err := s.tp.PrintfLine("%d%s%s", code, sep, line); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) handleEHLO(_ string) {
	lines := []string{"posthorn"}
	if s.l.tlsConfig != nil && !s.tlsActive {
		lines = append(lines, "STARTTLS")
	}
	if !s.authRequiredAndMissing() || s.canAdvertiseAuth() {
		// Advertise AUTH PLAIN when the operator's config has SMTP
		// users (regardless of TLS state — stdlib PlainAuth will
		// refuse to send creds in plaintext, which is fine).
		if s.canAdvertiseAuth() {
			lines = append(lines, "AUTH PLAIN")
		}
	}
	if s.l.maxBody > 0 {
		lines = append(lines, fmt.Sprintf("SIZE %d", s.l.maxBody))
	}
	_ = s.writeMultilineReply(250, lines)
}

func (s *session) handleHELO(_ string) {
	_ = s.writeReply(250, "posthorn")
}

func (s *session) canAdvertiseAuth() bool {
	mode := s.l.cfg.EffectiveAuthMode()
	return mode == AuthSMTP || mode == AuthEither
}

func (s *session) authRequiredAndMissing() bool {
	mode := s.l.cfg.EffectiveAuthMode()
	switch mode {
	case AuthNone:
		// No auth required — the sender allowlist + recipient cap are
		// the only gates. Only safe on private networks where network
		// access already implies trust (Docker bridge, loopback).
		return false
	case AuthSMTP:
		return s.authedUser == ""
	case AuthClientCert:
		return !s.authedViaCert
	}
	// AuthEither: missing if neither path succeeded.
	return s.authedUser == "" && !s.authedViaCert
}

// handleSTARTTLS upgrades the connection. Returns true if the upgrade
// happened (caller should continue the loop with the new conn); false
// on error or unsupported config (writeReply already done).
func (s *session) handleSTARTTLS() bool {
	if s.l.tlsConfig == nil {
		_ = s.writeReply(502, "5.5.1 STARTTLS not supported")
		return false
	}
	if s.tlsActive {
		_ = s.writeReply(503, "5.5.1 TLS already active")
		return false
	}
	if err := s.writeReply(220, "2.0.0 Ready to start TLS"); err != nil {
		return false
	}
	tlsConn := tls.Server(s.conn, s.l.tlsConfig)
	if err := tlsConn.HandshakeContext(context.Background()); err != nil {
		s.logger.Info("smtp_tls_handshake_failed", slog.String("error", err.Error()))
		return false
	}
	s.conn = tlsConn
	s.tp = textproto.NewConn(tlsConn)
	s.tlsActive = true
	// Reset session state per RFC: STARTTLS forgets any EHLO state.
	s.resetTransaction()
	s.authedUser = ""

	// If the client presented a verified cert and we're in cert-auth
	// mode, mark the session authed.
	if len(tlsConn.ConnectionState().VerifiedChains) > 0 {
		s.authedViaCert = true
	}
	s.logger.Info("smtp_tls_established")
	return true
}

// handleAUTH processes an AUTH command. Returns true when the session
// must close because the caller's AUTH-failure budget is exhausted
// (#50) — the 421 reply has already been written.
func (s *session) handleAUTH(arg string) (closeSession bool) {
	if s.l.cfg.RequireTLS && !s.tlsActive {
		_ = s.writeReply(530, "5.7.0 Must issue STARTTLS first")
		return false
	}
	if !s.canAdvertiseAuth() {
		_ = s.writeReply(502, "5.5.1 AUTH not supported")
		return false
	}
	// Accept only AUTH PLAIN for v1.0.
	const plainPrefix = "PLAIN"
	if len(arg) < len(plainPrefix) || !strings.EqualFold(arg[:len(plainPrefix)], plainPrefix) {
		_ = s.writeReply(504, "5.5.4 Auth mechanism not supported (PLAIN only)")
		return false
	}
	credsB64 := strings.TrimSpace(arg[len(plainPrefix):])
	if credsB64 == "" {
		_ = s.writeReply(334, "")
		// Next line should be the base64 credentials.
		line, err := s.tp.ReadLine()
		if err != nil {
			return false
		}
		credsB64 = line
	}
	raw, err := base64.StdEncoding.DecodeString(credsB64)
	if err != nil {
		return s.authFailed("5.7.8 Malformed AUTH credentials", "")
	}
	// PLAIN format: \x00<user>\x00<pass>
	parts := bytes.SplitN(raw, []byte{0}, 3)
	if len(parts) != 3 {
		return s.authFailed("5.7.8 Malformed AUTH credentials", "")
	}
	user := string(parts[1])
	pass := string(parts[2])
	if !s.verifyUser(user, pass) {
		return s.authFailed("5.7.8 Authentication failed", user)
	}
	s.authedUser = user
	_ = s.writeReply(235, "2.7.0 Authentication successful")
	s.logger.Info("smtp_auth_ok", slog.String("user", user))
	return false
}

// authFailed replies to a failed AUTH attempt, counting it against the
// per-IP brute-force budget (#50). Malformed credentials count the same
// as wrong ones — scanners produce both. When the budget is exhausted
// the reply is a 421 and the caller must close the session.
func (s *session) authFailed(msg535, user string) (closeSession bool) {
	if user != "" {
		s.logger.Info("smtp_auth_failed", slog.String("user", user))
	} else {
		s.logger.Info("smtp_auth_failed", slog.String("reason", "malformed_credentials"))
	}
	if s.l.authFailureExceeded(remoteIPOf(s.conn)) {
		_ = s.writeReply(421, "4.7.0 Too many failed AUTH attempts, closing connection")
		s.logger.Info("smtp_auth_rate_limited")
		return true
	}
	_ = s.writeReply(535, msg535)
	return false
}

func (s *session) verifyUser(user, pass string) bool {
	for _, u := range s.l.cfg.SMTPUsers {
		// Constant-time comparison so we don't leak which usernames
		// exist via timing.
		userMatch := subtle.ConstantTimeCompare([]byte(u.Username), []byte(user)) == 1
		passMatch := subtle.ConstantTimeCompare([]byte(u.Password), []byte(pass)) == 1
		if userMatch && passMatch {
			return true
		}
	}
	return false
}

func (s *session) handleMAIL(arg string) {
	if s.l.cfg.RequireTLS && !s.tlsActive {
		_ = s.writeReply(530, "5.7.0 Must issue STARTTLS first")
		return
	}
	if s.authRequiredAndMissing() {
		_ = s.writeReply(530, "5.7.0 Authentication required")
		return
	}
	addr := extractEnvelopeAddress(arg, "FROM:")
	if addr == "" {
		_ = s.writeReply(501, "5.5.4 Syntax: MAIL FROM:<address>")
		return
	}
	if !matchesAllowlist(addr, s.l.cfg.AllowedSenders) {
		_ = s.writeReply(550, "5.7.1 Sender not authorized")
		s.logger.Info("smtp_sender_rejected", slog.String("from", addr))
		return
	}
	s.resetTransaction()
	s.mailFrom = addr
	s.transaction = true
	_ = s.writeReply(250, "2.1.0 OK")
}

func (s *session) handleRCPT(arg string) {
	if !s.transaction {
		_ = s.writeReply(503, "5.5.1 MAIL FROM required first")
		return
	}
	addr := extractEnvelopeAddress(arg, "TO:")
	if addr == "" {
		_ = s.writeReply(501, "5.5.4 Syntax: RCPT TO:<address>")
		return
	}
	cap := s.l.cfg.EffectiveMaxRecipients()
	if cap > 0 && len(s.rcptTo) >= cap {
		_ = s.writeReply(452, "4.5.3 Too many recipients")
		return
	}
	if len(s.l.cfg.AllowedRecipients) > 0 && !matchesAllowlist(addr, s.l.cfg.AllowedRecipients) {
		_ = s.writeReply(550, "5.7.1 Recipient not authorized")
		s.logger.Info("smtp_recipient_rejected", slog.String("to", addr))
		return
	}
	s.rcptTo = append(s.rcptTo, addr)
	_ = s.writeReply(250, "2.1.5 OK")
}

func (s *session) handleDATA() {
	if !s.transaction {
		_ = s.writeReply(503, "5.5.1 MAIL FROM required first")
		return
	}
	if len(s.rcptTo) == 0 {
		_ = s.writeReply(503, "5.5.1 RCPT TO required first")
		return
	}
	if err := s.writeReply(354, "End data with <CR><LF>.<CR><LF>"); err != nil {
		return
	}
	// Bounded read; reject DATA exceeding the configured cap.
	r := s.tp.DotReader()
	buf := &bytes.Buffer{}
	limited := io.LimitReader(r, s.l.maxBody+1) // +1 so we detect overflow
	n, err := io.Copy(buf, limited)
	if err != nil {
		_ = s.writeReply(451, "4.3.0 Read error")
		s.resetTransaction()
		return
	}
	if s.l.maxBody > 0 && n > s.l.maxBody {
		_ = s.writeReply(552, "5.3.4 Message too big")
		s.resetTransaction()
		// Drain any remaining data so the protocol state stays valid.
		_, _ = io.Copy(io.Discard, r)
		return
	}

	msg, err := parseMIMEToMessage(buf.Bytes(), s.mailFrom, s.rcptTo)
	if err != nil {
		_ = s.writeReply(550, "5.6.0 Malformed message: "+err.Error())
		s.resetTransaction()
		return
	}

	// Hand off to the outbound transport. SendWithRetry applies the
	// FR19-22 retry policy (one retry on transient/429), shared with
	// the HTTP ingress per ADR-12's ingress-agnostic egress (issue
	// #59); previously the SMTP path called Send directly and returned
	// an immediate 451 on errors the HTTP path would have retried.
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	submissionID := uuid.NewString()
	msg.SubmissionID = submissionID // FR89; Fields stays nil for SMTP

	// FR86: drop suppressed recipients before the send. All suppressed
	// → 250 (terminally handled; a 5xx would make the client retry mail
	// that must never send) with nothing delivered.
	var suppressedCount int
	msg.To, suppressedCount = s.filterSuppressed(msg.To)
	if suppressedCount > 0 && s.l.recorder != nil {
		s.l.recorder.Suppressed(s.l.name, suppressedCount)
	}
	if len(msg.To) == 0 {
		s.recordSuppressed(submissionID, msg)
		_ = s.writeReply(250, "2.1.5 OK suppressed as "+submissionID)
		s.logger.Info("smtp_submission_suppressed",
			slog.String("submission_id", submissionID),
			slog.Int("recipients", suppressedCount),
		)
		s.resetTransaction()
		return
	}

	// FR77: persist pre-send (status "sending") so a crash mid-send is
	// recoverable (NFR28). Best-effort; degraded storage never blocks.
	persisted, queueable := s.recordSubmission(submissionID, msg)

	sendStart := time.Now()
	result, sendErr := transport.SendWithRetry(ctx, s.l.transport, msg, s.logger)
	if sendErr != nil {
		// FR78: with storage available, transient exhaustion queues and
		// the relay OWNS the retry — reply 250, because a 451 would make
		// the client retry a send Posthorn is already going to retry,
		// double-sending when both succeed.
		if queueable && transport.IsRetryable(sendErr) {
			if qerr := s.l.gate.Store().Enqueue(submissionID, time.Now()); qerr != nil {
				s.l.gate.ReportError(qerr)
			} else {
				if s.l.recorder != nil {
					s.l.recorder.Queued(s.l.name, s.l.cfg.Transport.Type)
				}
				s.logger.Warn("smtp_submission_queued",
					slog.String("submission_id", submissionID),
					slog.String("error", sendErr.Error()),
				)
				_ = s.writeReply(250, "2.0.0 OK accepted for retry as "+submissionID)
				s.resetTransaction()
				return
			}
		}
		if persisted {
			if serr := s.l.gate.Store().MarkStatus(submissionID, storage.StatusFailed, sendErr.Error()); serr != nil {
				s.l.gate.ReportError(serr)
			}
		}
		s.recordSendFailed(sendErr)
		s.logger.Error("smtp_submission_failed",
			slog.String("submission_id", submissionID),
			slog.String("error", sendErr.Error()),
		)
		_ = s.writeReply(451, "4.0.0 Upstream transport failed")
		s.resetTransaction()
		return
	}
	if persisted {
		if serr := s.l.gate.Store().MarkSent(submissionID, result.MessageID, time.Now()); serr != nil {
			s.l.gate.ReportError(serr)
		}
	}
	// Record the outcome before acknowledging it. Once the client holds
	// the reply, the send must already be visible on /metrics and in the
	// log; replying first left a window where a scrape taken on the 250
	// showed nothing (the TestSMTP_RecorderWired flake, #43). The queued
	// and failed paths above follow the same order.
	s.recordSendOk(time.Since(sendStart))
	s.logger.Info("smtp_submission_sent",
		slog.String("submission_id", submissionID),
		slog.String("transport_message_id", result.MessageID),
		slog.Int64("size_bytes", n),
	)
	_ = s.writeReply(250, "2.0.0 OK queued as "+submissionID)
	s.resetTransaction()
}

// recordSubmission persists the pre-send row for an SMTP submission
// (FR77). SMTP has no form fields and no strip_client_ip knob; the
// session's remote IP is recorded, matching the NFR23 session log.
func (s *session) recordSubmission(id string, msg transport.Message) (persisted, queueable bool) {
	g := s.l.gate
	if g == nil || !g.Healthy() {
		return false, false
	}
	host, _, err := net.SplitHostPort(s.conn.RemoteAddr().String())
	if err != nil {
		host = s.conn.RemoteAddr().String()
	}
	sub := storage.Submission{
		ID:        id,
		Endpoint:  s.l.name,
		Transport: s.l.cfg.Transport.Type,
		From:      msg.From,
		ToAddrs:   msg.To,
		ReplyTo:   msg.ReplyTo,
		Subject:   msg.Subject,
		BodyText:  msg.BodyText,
		BodyHTML:  msg.BodyHTML,
		ClientIP:  host,
		Status:    storage.StatusSending,
		CreatedAt: time.Now(),
	}
	if err := g.Store().RecordSubmission(sub); err != nil {
		g.ReportError(err)
		return false, false
	}
	return true, true
}

// filterSuppressed drops suppressed recipients (FR86). Without storage
// (or degraded) everything passes — v1.x untouched; lookup errors fail
// open per-recipient (mail first, NFR27).
func (s *session) filterSuppressed(to []string) (remaining []string, suppressed int) {
	g := s.l.gate
	if g == nil || !g.Healthy() {
		return to, 0
	}
	remaining = make([]string, 0, len(to))
	for _, addr := range to {
		_, ok, err := g.Store().SuppressionFor(addr)
		if err != nil {
			g.ReportError(err)
			remaining = append(remaining, addr)
			continue
		}
		if ok {
			suppressed++
			continue
		}
		remaining = append(remaining, addr)
	}
	return remaining, suppressed
}

// recordSuppressed logs an all-recipients-suppressed submission.
func (s *session) recordSuppressed(id string, msg transport.Message) {
	g := s.l.gate
	if g == nil || !g.Healthy() {
		return
	}
	err := g.Store().RecordSubmission(storage.Submission{
		ID:        id,
		Endpoint:  s.l.name,
		Transport: s.l.cfg.Transport.Type,
		From:      msg.From,
		Subject:   msg.Subject,
		Status:    storage.StatusSuppressed,
		CreatedAt: time.Now(),
	})
	if err != nil {
		g.ReportError(err)
	}
}

func (s *session) recordSendOk(latency time.Duration) {
	if s.l.recorder == nil {
		return
	}
	// The endpoint label is the listener's name ("smtp_listener" when it
	// has none), so operators can split inbound-via-HTTP from
	// inbound-via-SMTP, and one listener from another, in metrics.
	s.l.recorder.Sent(s.l.name, s.l.cfg.Transport.Type, latency)
}

func (s *session) recordSendFailed(err error) {
	if s.l.recorder == nil {
		return
	}
	cls := "unknown"
	var te *transport.TransportError
	if errors.As(err, &te) {
		cls = te.Class.String()
	}
	s.l.recorder.Failed(s.l.name, s.l.cfg.Transport.Type, cls)
}

func (s *session) resetTransaction() {
	s.mailFrom = ""
	s.rcptTo = nil
	s.transaction = false
}

// splitCommand returns ("EHLO", "client.host") for "EHLO client.host"
// and ("QUIT", "") for "QUIT".
func splitCommand(line string) (cmd, arg string) {
	idx := strings.IndexByte(line, ' ')
	if idx < 0 {
		return line, ""
	}
	return line[:idx], line[idx+1:]
}

// extractEnvelopeAddress parses the address portion of a MAIL FROM or
// RCPT TO argument. Examples:
//
//	"FROM:<noreply@example.com>" → "noreply@example.com"
//	"TO:<a@b.com> SIZE=1024"     → "a@b.com"
//
// prefix is the expected SMTP prefix ("FROM:" or "TO:"); case-
// insensitive. Returns "" on parse failure.
func extractEnvelopeAddress(arg, prefix string) string {
	if len(arg) < len(prefix) || !strings.EqualFold(arg[:len(prefix)], prefix) {
		return ""
	}
	rest := strings.TrimSpace(arg[len(prefix):])
	if !strings.HasPrefix(rest, "<") {
		return ""
	}
	end := strings.IndexByte(rest, '>')
	if end < 0 {
		return ""
	}
	return rest[1:end]
}

// matchesAllowlist returns true if addr matches one of the entries. An
// entry can be an exact address ("noreply@example.com") or a domain
// wildcard ("*@example.com"). Star ("*") matches anything.
func matchesAllowlist(addr string, allowlist []string) bool {
	for _, entry := range allowlist {
		if entry == "*" {
			return true
		}
		if entry == addr {
			return true
		}
		if strings.HasPrefix(entry, "*@") {
			domain := entry[2:]
			if at := strings.IndexByte(addr, '@'); at >= 0 && addr[at+1:] == domain {
				return true
			}
		}
	}
	return false
}
