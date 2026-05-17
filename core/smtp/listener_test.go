package smtp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/craigmccaskill/posthorn/config"
	"github.com/craigmccaskill/posthorn/transport"
)

// --- Mock transport for SMTP tests ---

type mockTransport struct {
	mu     sync.Mutex
	sent   []transport.Message
	result transport.SendResult
	err    error
}

func (m *mockTransport) Send(_ context.Context, msg transport.Message) (transport.SendResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, msg)
	return m.result, m.err
}

func (m *mockTransport) Sent() []transport.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]transport.Message, len(m.sent))
	copy(out, m.sent)
	return out
}

// --- Fixture: listener + raw textproto client ---

type smtpFixture struct {
	t        *testing.T
	listener *Listener
	mt       *mockTransport
	addr     string
	ctx      context.Context
	cancel   context.CancelFunc
}

func startListener(t *testing.T, cfg ListenerConfig) *smtpFixture {
	t.Helper()
	if cfg.MaxMessageSize == "" {
		cfg.MaxMessageSize = "1MB"
	}
	mt := &mockTransport{}
	maxBody := int64(1 << 20) // 1MB for tests
	l, err := New(cfg, mt, maxBody, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	go func() {
		// We can't easily test Start because it blocks; manually
		// bind a listener and call handleConnection.
		_ = ctx // suppress unused
		close(started)
	}()
	<-started

	// Bind ourselves to a random port and intercept.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	l.mu.Lock()
	l.listener = ln
	l.mu.Unlock()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			l.wg.Add(1)
			go func(c net.Conn) {
				defer l.wg.Done()
				l.handleConnection(c)
			}(conn)
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = ln.Close()
		l.wg.Wait()
	})
	return &smtpFixture{
		t:        t,
		listener: l,
		mt:       mt,
		addr:     ln.Addr().String(),
		ctx:      ctx,
		cancel:   cancel,
	}
}

// dial connects to the listener and reads the greeting. Returns the
// textproto.Conn for further commands. Caller closes via the cleanup
// function.
func (f *smtpFixture) dial() *textproto.Conn {
	f.t.Helper()
	conn, err := net.Dial("tcp", f.addr)
	if err != nil {
		f.t.Fatalf("dial: %v", err)
	}
	tp := textproto.NewConn(conn)
	f.t.Cleanup(func() { _ = tp.Close() })
	// Read greeting.
	if _, _, err := tp.ReadResponse(220); err != nil {
		f.t.Fatalf("read greeting: %v", err)
	}
	return tp
}

// expect reads a response and asserts the code matches.
func expect(t *testing.T, tp *textproto.Conn, wantCode int) string {
	t.Helper()
	code, msg, err := tp.ReadResponse(wantCode)
	if err != nil {
		t.Fatalf("expected %d, got code=%d msg=%q err=%v", wantCode, code, msg, err)
	}
	return msg
}

// expectCode reads a response without enforcing a particular code, returning
// the actual code + message.
func expectCode(t *testing.T, tp *textproto.Conn) (int, string) {
	t.Helper()
	code, msg, err := tp.ReadResponse(0)
	if err != nil {
		// textproto returns "ReadResponse: <ErrorResponse>" — extract code from err.
		if er, ok := err.(*textproto.Error); ok {
			return er.Code, er.Msg
		}
		t.Fatalf("read response: %v", err)
	}
	return code, msg
}

// --- Config / Validate tests ---

func TestListenerConfig_RequiresListen(t *testing.T) {
	c := ListenerConfig{AllowedSenders: []string{"*"}, Transport: config.TransportConfig{Type: "postmark", Settings: map[string]any{"api_key": "k"}}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Errorf("expected listen-required error, got: %v", err)
	}
}

func TestListenerConfig_RequiresSenderAllowlist(t *testing.T) {
	c := ListenerConfig{
		Listen:                  ":2525",
		AllowedSenders:          nil,
		MaxRecipientsPerSession: 10,
		Transport:               config.TransportConfig{Type: "postmark", Settings: map[string]any{"api_key": "k"}},
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "allowed_senders") {
		t.Errorf("expected allowed_senders error, got: %v", err)
	}
}

func TestListenerConfig_AuthSMTPRequiresUsers(t *testing.T) {
	c := ListenerConfig{
		Listen:         ":2525",
		AllowedSenders: []string{"*@example.com"},
		AuthRequired:   AuthSMTP,
		Transport:      config.TransportConfig{Type: "postmark", Settings: map[string]any{"api_key": "k"}},
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "smtp_users") {
		t.Errorf("expected smtp_users-required error, got: %v", err)
	}
}

func TestListenerConfig_ClientCertRequiresCA(t *testing.T) {
	c := ListenerConfig{
		Listen:         ":2525",
		AllowedSenders: []string{"*"},
		AuthRequired:   AuthClientCert,
		TLSCert:        "/dev/null",
		TLSKey:         "/dev/null",
		Transport:      config.TransportConfig{Type: "postmark", Settings: map[string]any{"api_key": "k"}},
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "client_cert_ca") {
		t.Errorf("expected client_cert_ca-required error, got: %v", err)
	}
}

func TestListenerConfig_RequireTLSRequiresCertKey(t *testing.T) {
	c := ListenerConfig{
		Listen:         ":2525",
		AllowedSenders: []string{"*"},
		RequireTLS:     true,
		AuthRequired:   AuthSMTP,
		SMTPUsers:      []User{{Username: "u", Password: "p"}},
		Transport:      config.TransportConfig{Type: "postmark", Settings: map[string]any{"api_key": "k"}},
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "tls_cert") {
		t.Errorf("expected tls_cert-required error, got: %v", err)
	}
}

// --- Wire-level tests ---

func baseTestConfig() ListenerConfig {
	return ListenerConfig{
		Listen:                  "127.0.0.1:0",
		RequireTLS:              false, // tests use plain text + manual AUTH PLAIN
		AuthRequired:            AuthSMTP,
		SMTPUsers:               []User{{Username: "user", Password: "pass"}},
		AllowedSenders:          []string{"*@example.com"},
		MaxRecipientsPerSession: 5,
		MaxMessageSize:          "1MB",
		Transport: config.TransportConfig{
			Type:     "postmark",
			Settings: map[string]any{"api_key": "k"},
		},
	}
}

func authPlainCreds(user, pass string) string {
	raw := "\x00" + user + "\x00" + pass
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

func TestSMTP_HappyPath_DeliversMessage(t *testing.T) {
	f := startListener(t, baseTestConfig())
	tp := f.dial()

	_ = tp.PrintfLine("EHLO client.test")
	expectMultiline(t, tp, 250)

	_ = tp.PrintfLine("AUTH PLAIN %s", authPlainCreds("user", "pass"))
	expect(t, tp, 235)

	_ = tp.PrintfLine("MAIL FROM:<noreply@example.com>")
	expect(t, tp, 250)

	_ = tp.PrintfLine("RCPT TO:<alice@somewhere.com>")
	expect(t, tp, 250)

	_ = tp.PrintfLine("DATA")
	expect(t, tp, 354)

	dataBody := "From: noreply@example.com\r\n" +
		"Subject: Hello\r\n" +
		"\r\n" +
		"Body text.\r\n"
	_ = tp.PrintfLine("%s.", dataBody)
	expect(t, tp, 250)

	_ = tp.PrintfLine("QUIT")
	expect(t, tp, 221)

	// Give the goroutine a moment to land the Send call.
	waitForSend(t, f.mt, 1, 500*time.Millisecond)

	sent := f.mt.Sent()
	if len(sent) != 1 {
		t.Fatalf("transport.Send count = %d, want 1", len(sent))
	}
	if sent[0].From != "noreply@example.com" {
		t.Errorf("From = %q, want %q", sent[0].From, "noreply@example.com")
	}
	if len(sent[0].To) != 1 || sent[0].To[0] != "alice@somewhere.com" {
		t.Errorf("To = %v, want [alice@somewhere.com]", sent[0].To)
	}
	if sent[0].Subject != "Hello" {
		t.Errorf("Subject = %q", sent[0].Subject)
	}
	if !strings.Contains(sent[0].BodyText, "Body text.") {
		t.Errorf("BodyText missing %q: %s", "Body text.", sent[0].BodyText)
	}
}

func TestSMTP_AuthRequiredBeforeMail(t *testing.T) {
	f := startListener(t, baseTestConfig())
	tp := f.dial()
	_ = tp.PrintfLine("EHLO c")
	expectMultiline(t, tp, 250)
	_ = tp.PrintfLine("MAIL FROM:<a@example.com>")
	code, _ := expectCode(t, tp)
	if code != 530 {
		t.Errorf("MAIL before AUTH got %d, want 530", code)
	}
}

func TestSMTP_WrongPassword_Rejects(t *testing.T) {
	f := startListener(t, baseTestConfig())
	tp := f.dial()
	_ = tp.PrintfLine("EHLO c")
	expectMultiline(t, tp, 250)
	_ = tp.PrintfLine("AUTH PLAIN %s", authPlainCreds("user", "wrong"))
	code, _ := expectCode(t, tp)
	if code != 535 {
		t.Errorf("AUTH with wrong password got %d, want 535", code)
	}
}

func TestSMTP_SenderNotInAllowlist_550(t *testing.T) {
	cfg := baseTestConfig()
	cfg.AllowedSenders = []string{"noreply@example.com"} // exact match only
	f := startListener(t, cfg)
	tp := f.dial()
	_ = tp.PrintfLine("EHLO c")
	expectMultiline(t, tp, 250)
	_ = tp.PrintfLine("AUTH PLAIN %s", authPlainCreds("user", "pass"))
	expect(t, tp, 235)
	_ = tp.PrintfLine("MAIL FROM:<other@example.com>")
	code, _ := expectCode(t, tp)
	if code != 550 {
		t.Errorf("MAIL with disallowed sender got %d, want 550", code)
	}
}

func TestSMTP_RecipientCap_452(t *testing.T) {
	cfg := baseTestConfig()
	cfg.MaxRecipientsPerSession = 2
	f := startListener(t, cfg)
	tp := f.dial()
	_ = tp.PrintfLine("EHLO c")
	expectMultiline(t, tp, 250)
	_ = tp.PrintfLine("AUTH PLAIN %s", authPlainCreds("user", "pass"))
	expect(t, tp, 235)
	_ = tp.PrintfLine("MAIL FROM:<a@example.com>")
	expect(t, tp, 250)
	_ = tp.PrintfLine("RCPT TO:<r1@x.com>")
	expect(t, tp, 250)
	_ = tp.PrintfLine("RCPT TO:<r2@x.com>")
	expect(t, tp, 250)
	_ = tp.PrintfLine("RCPT TO:<r3@x.com>")
	code, _ := expectCode(t, tp)
	if code != 452 {
		t.Errorf("RCPT beyond cap got %d, want 452", code)
	}
}

func TestSMTP_RecipientAllowlist_550(t *testing.T) {
	cfg := baseTestConfig()
	cfg.AllowedRecipients = []string{"*@yourdomain.com"}
	f := startListener(t, cfg)
	tp := f.dial()
	_ = tp.PrintfLine("EHLO c")
	expectMultiline(t, tp, 250)
	_ = tp.PrintfLine("AUTH PLAIN %s", authPlainCreds("user", "pass"))
	expect(t, tp, 235)
	_ = tp.PrintfLine("MAIL FROM:<a@example.com>")
	expect(t, tp, 250)
	_ = tp.PrintfLine("RCPT TO:<eve@attacker.com>")
	code, _ := expectCode(t, tp)
	if code != 550 {
		t.Errorf("RCPT to disallowed got %d, want 550", code)
	}
}

func TestSMTP_RequireTLS_BlocksAuthBeforeSTARTTLS(t *testing.T) {
	cfg := baseTestConfig()
	cfg.RequireTLS = true
	certPath, keyPath := writeSelfSignedCert(t)
	cfg.TLSCert = certPath
	cfg.TLSKey = keyPath
	f := startListener(t, cfg)
	tp := f.dial()
	_ = tp.PrintfLine("EHLO c")
	expectMultiline(t, tp, 250)
	_ = tp.PrintfLine("AUTH PLAIN %s", authPlainCreds("user", "pass"))
	code, _ := expectCode(t, tp)
	if code != 530 {
		t.Errorf("AUTH before STARTTLS got %d, want 530", code)
	}
}

func TestSMTP_MessageSizeCap_552(t *testing.T) {
	cfg := baseTestConfig()
	// Construct listener with tiny max body.
	mt := &mockTransport{}
	l, err := New(cfg, mt, 100, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	l.mu.Lock()
	l.listener = ln
	l.mu.Unlock()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			l.wg.Add(1)
			go func(c net.Conn) {
				defer l.wg.Done()
				l.handleConnection(c)
			}(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); l.wg.Wait() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	tp := textproto.NewConn(conn)
	_, _, _ = tp.ReadResponse(220)

	_ = tp.PrintfLine("EHLO c")
	expectMultiline(t, tp, 250)
	_ = tp.PrintfLine("AUTH PLAIN %s", authPlainCreds("user", "pass"))
	expect(t, tp, 235)
	_ = tp.PrintfLine("MAIL FROM:<a@example.com>")
	expect(t, tp, 250)
	_ = tp.PrintfLine("RCPT TO:<x@x.com>")
	expect(t, tp, 250)
	_ = tp.PrintfLine("DATA")
	expect(t, tp, 354)

	bigBody := "From: a@example.com\r\nSubject: S\r\n\r\n" + strings.Repeat("X", 500)
	_ = tp.PrintfLine("%s.", bigBody)
	code, _ := expectCode(t, tp)
	if code != 552 {
		t.Errorf("oversized DATA got %d, want 552", code)
	}
}

// --- MIME parsing tests (unit) ---

func TestParseMIMEToMessage_PlainText(t *testing.T) {
	data := []byte("From: a@example.com\r\nSubject: Hello\r\n\r\nBody text.\r\n")
	msg, err := parseMIMEToMessage(data, "a@example.com", []string{"r@example.com"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if msg.From != "a@example.com" {
		t.Errorf("From = %q", msg.From)
	}
	if msg.Subject != "Hello" {
		t.Errorf("Subject = %q", msg.Subject)
	}
	if !strings.Contains(msg.BodyText, "Body text.") {
		t.Errorf("BodyText = %q", msg.BodyText)
	}
	if len(msg.To) != 1 || msg.To[0] != "r@example.com" {
		t.Errorf("To = %v (must come from envelope, not MIME)", msg.To)
	}
}

func TestParseMIMEToMessage_RecipientsFromEnvelopeNotMIME_NFR22(t *testing.T) {
	// Malicious MIME tries to add Bcc: header. The envelope is the
	// source of truth; the MIME Bcc must be ignored.
	data := []byte(
		"From: attacker@evil.com\r\n" +
			"To: visible@example.com\r\n" +
			"Bcc: victim@target.com\r\n" +
			"Subject: hi\r\n" +
			"\r\n" +
			"body\r\n")
	msg, err := parseMIMEToMessage(data, "attacker@evil.com", []string{"intended@example.com"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(msg.To) != 1 || msg.To[0] != "intended@example.com" {
		t.Errorf("To = %v, want [intended@example.com] only (Bcc/To from MIME must be ignored)", msg.To)
	}
}

func TestParseMIMEToMessage_MultipartPrefersTextPlain(t *testing.T) {
	boundary := "BOUNDARY"
	data := []byte(
		"From: a@example.com\r\n" +
			"Subject: Multi\r\n" +
			"Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n" +
			"\r\n" +
			"--" + boundary + "\r\n" +
			"Content-Type: text/plain; charset=utf-8\r\n" +
			"\r\n" +
			"plain version\r\n" +
			"--" + boundary + "\r\n" +
			"Content-Type: text/html; charset=utf-8\r\n" +
			"\r\n" +
			"<p>html version</p>\r\n" +
			"--" + boundary + "--\r\n")
	msg, err := parseMIMEToMessage(data, "a@example.com", []string{"r@example.com"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.Contains(msg.BodyText, "plain version") {
		t.Errorf("BodyText should prefer text/plain: %q", msg.BodyText)
	}
	if strings.Contains(msg.BodyText, "<p>") {
		t.Errorf("BodyText leaked HTML: %q", msg.BodyText)
	}
}

func TestParseMIMEToMessage_HTMLOnly_Rejected(t *testing.T) {
	data := []byte(
		"From: a@example.com\r\n" +
			"Subject: HTML\r\n" +
			"Content-Type: text/html; charset=utf-8\r\n" +
			"\r\n" +
			"<p>only html</p>\r\n")
	_, err := parseMIMEToMessage(data, "a@example.com", []string{"r@example.com"})
	if err == nil {
		t.Error("HTML-only message accepted; expected rejection")
	}
}

func TestParseMIMEToMessage_RFC2047EncodedSubject(t *testing.T) {
	// Base64-encoded "café" subject.
	data := []byte("From: a@example.com\r\nSubject: =?UTF-8?B?Y2Fmw6k=?=\r\n\r\nbody\r\n")
	msg, err := parseMIMEToMessage(data, "a@example.com", []string{"r@example.com"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if msg.Subject != "café" {
		t.Errorf("Subject = %q, want %q (RFC 2047 decoded)", msg.Subject, "café")
	}
}

// --- Allowlist matching tests ---

func TestMatchesAllowlist(t *testing.T) {
	tests := []struct {
		addr      string
		allowlist []string
		want      bool
	}{
		{"a@example.com", []string{"a@example.com"}, true},
		{"a@example.com", []string{"b@example.com"}, false},
		{"a@example.com", []string{"*@example.com"}, true},
		{"a@example.com", []string{"*@other.com"}, false},
		{"a@example.com", []string{"*"}, true},
		{"a@example.com", []string{"*@other.com", "*@example.com"}, true},
	}
	for _, tt := range tests {
		got := matchesAllowlist(tt.addr, tt.allowlist)
		if got != tt.want {
			t.Errorf("matchesAllowlist(%q, %v) = %v, want %v", tt.addr, tt.allowlist, got, tt.want)
		}
	}
}

// --- Helpers ---

func expectMultiline(t *testing.T, tp *textproto.Conn, wantCode int) {
	t.Helper()
	// Read continuation lines until the final one.
	for {
		line, err := tp.ReadLine()
		if err != nil {
			t.Fatalf("read multiline: %v", err)
		}
		if len(line) < 4 {
			t.Fatalf("malformed reply: %q", line)
		}
		var code int
		fmt.Sscanf(line[:3], "%d", &code)
		if code != wantCode {
			t.Fatalf("got code %d, want %d (line %q)", code, wantCode, line)
		}
		if line[3] == ' ' {
			return // final line of multiline reply
		}
		// line[3] == '-' means more lines follow.
	}
}

func waitForSend(t *testing.T, mt *mockTransport, count int, deadline time.Duration) {
	t.Helper()
	start := time.Now()
	for time.Since(start) < deadline {
		if len(mt.Sent()) >= count {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("transport.Send not called within %v (got %d, want %d)", deadline, len(mt.Sent()), count)
}

// writeSelfSignedCert generates a self-signed certificate at test time
// and writes it to two temp files. Used by tests that need STARTTLS
// without requiring on-disk test fixtures.
func writeSelfSignedCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "posthorn-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	cf, _ := os.Create(certPath)
	_ = pem.Encode(cf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	_ = cf.Close()
	kf, _ := os.Create(keyPath)
	_ = pem.Encode(kf, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	_ = kf.Close()
	return certPath, keyPath
}

// Ensure the TLS package is used so the import isn't dead in builds
// where only some tests run.
var _ = tls.VersionTLS12
