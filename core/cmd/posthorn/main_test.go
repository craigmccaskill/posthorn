package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/craigmccaskill/posthorn/config"
	"github.com/craigmccaskill/posthorn/ingress"
	"github.com/craigmccaskill/posthorn/metrics"
	"github.com/craigmccaskill/posthorn/smtp"
	"github.com/craigmccaskill/posthorn/storage"
	"github.com/craigmccaskill/posthorn/transport"
)

// --- buildTransport ---

func TestBuildTransport_Postmark(t *testing.T) {
	tp, err := buildTransport(config.TransportConfig{
		Type:     "postmark",
		Settings: map[string]any{"api_key": "test-key"},
	})
	if err != nil {
		t.Fatalf("buildTransport: %v", err)
	}
	if tp == nil {
		t.Fatal("nil transport with nil error")
	}
}

func TestBuildTransport_UnknownType(t *testing.T) {
	_, err := buildTransport(config.TransportConfig{Type: "nonsense-transport"})
	if err == nil {
		t.Fatal("expected error for unknown transport type")
	}
	if !strings.Contains(err.Error(), "unknown transport type") {
		t.Errorf("error: %v", err)
	}
}

func TestBuildTransport_PostmarkMissingAPIKey(t *testing.T) {
	_, err := buildTransport(config.TransportConfig{
		Type:     "postmark",
		Settings: map[string]any{},
	})
	if err == nil {
		t.Fatal("expected error for missing api_key")
	}
}

// --- buildLogger ---

func TestBuildLogger_DefaultLevel(t *testing.T) {
	logger := buildLogger(config.LoggingConfig{})
	if logger == nil {
		t.Fatal("nil logger")
	}
}

func TestBuildLogger_LevelAccepted(t *testing.T) {
	for _, lvl := range []string{"debug", "info", "warn", "error", ""} {
		t.Run(lvl, func(t *testing.T) {
			logger := buildLogger(config.LoggingConfig{Level: lvl})
			if logger == nil {
				t.Fatal("nil logger")
			}
		})
	}
}

// --- runValidate ---

const validTOML = `
[[endpoints]]
path = "/api/contact"
to = ["craig@example.com"]
from = "noreply@example.com"
subject = "Contact"
body = "Body"

[endpoints.transport]
type = "postmark"

[endpoints.transport.settings]
api_key = "test-key"
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestRunValidate_Valid(t *testing.T) {
	path := writeConfig(t, validTOML)
	if err := runValidate([]string{"--config", path}); err != nil {
		t.Errorf("runValidate: %v", err)
	}
}

func TestRunValidate_SMTPListener_SemanticError(t *testing.T) {
	// Structurally valid (config.Load passes) but the SMTP-level checks
	// fail: require_tls defaults to true with no tls_cert. Before #58,
	// runValidate skipped these and reported OK; now it must fail.
	cfg := validTOML + `
[smtp_listener]
listen          = "127.0.0.1:2525"
allowed_senders = ["*@example.com"]

[[smtp_listener.smtp_users]]
username = "u"
password = "p"

[smtp_listener.transport]
type = "postmark"

[smtp_listener.transport.settings]
api_key = "k"
`
	path := writeConfig(t, cfg)
	err := runValidate([]string{"--config", path})
	if err == nil || !strings.Contains(err.Error(), "smtp_listener") {
		t.Errorf("want an smtp_listener validation error, got %v", err)
	}
}

func TestRunValidate_FileNotFound(t *testing.T) {
	err := runValidate([]string{"--config", "/no/such/file.toml"})
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestRunValidate_InvalidConfig(t *testing.T) {
	// Missing required field "from" should surface as a config error.
	bad := strings.Replace(validTOML, `from = "noreply@example.com"`, "", 1)
	path := writeConfig(t, bad)
	err := runValidate([]string{"--config", path})
	if err == nil {
		t.Fatal("expected error for invalid config")
	}
}

func TestRunValidate_TemplateParseError(t *testing.T) {
	// Body with unclosed action — config.Load passes (it doesn't parse
	// templates), but gateway.New surfaces the template parse error.
	bad := strings.Replace(validTOML, `body = "Body"`, `body = "Bad: {{.x"`, 1)
	path := writeConfig(t, bad)
	err := runValidate([]string{"--config", path})
	if err == nil {
		t.Fatal("expected error for unparseable template")
	}
}

// --- buildMux ---

// muxRegRec builds the mux with a fresh shared registry + recorder, the
// way runServe now wires them (#57). Tests scrape /metrics through the
// mux, which uses this registry.
func muxRegRec(cfg *config.Config) (*http.ServeMux, *metrics.Registry, *metrics.Recorder, error) {
	reg := metrics.New()
	rec := metrics.NewRecorder(reg)
	mux, _, err := buildMux(cfg, buildLogger(config.LoggingConfig{}), reg, rec, nil)
	return mux, reg, rec, err
}

func TestBuildMux_RoutesEndpointsCorrectly(t *testing.T) {
	cfg := &config.Config{
		Endpoints: []config.EndpointConfig{
			{
				Path:    "/api/contact",
				To:      []string{"to@example.com"},
				From:    "from@example.com",
				Subject: "S",
				Body:    "B",
				Transport: config.TransportConfig{
					Type:     "postmark",
					Settings: map[string]any{"api_key": "k"},
				},
			},
		},
	}
	mux, _, _, err := muxRegRec(cfg)
	if err != nil {
		t.Fatalf("buildMux: %v", err)
	}

	// Configured path is reachable.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/contact", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(rec, req)
	// The transport is real (Postmark client pointing at the public API), so
	// a synchronous Send will fail (test-key is invalid). But the mux at
	// least routed the request — we assert we got SOMETHING back, not 404.
	// Not 404 = mux routed correctly.
	if rec.Code == http.StatusNotFound {
		t.Errorf("configured path /api/contact returned 404; mux did not route")
	}
}

func TestBuildMux_UnconfiguredPath_404(t *testing.T) {
	cfg := &config.Config{
		Endpoints: []config.EndpointConfig{
			{
				Path:    "/api/contact",
				To:      []string{"to@example.com"},
				From:    "from@example.com",
				Subject: "S",
				Body:    "B",
				Transport: config.TransportConfig{
					Type:     "postmark",
					Settings: map[string]any{"api_key": "k"},
				},
			},
		},
	}
	mux, _, _, err := muxRegRec(cfg)
	if err != nil {
		t.Fatalf("buildMux: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/unconfigured", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for unconfigured path", rec.Code)
	}
}

func TestBuildMux_BadTransport_PropagatesError(t *testing.T) {
	cfg := &config.Config{
		Endpoints: []config.EndpointConfig{
			{
				Path:    "/api/x",
				To:      []string{"to@example.com"},
				From:    "from@example.com",
				Subject: "S",
				Body:    "B",
				Transport: config.TransportConfig{
					Type: "nonexistent",
				},
			},
		},
	}
	_, _, _, err := muxRegRec(cfg)
	if err == nil {
		t.Fatal("expected error for bad transport")
	}
}

// TestBuildMux_HealthzRegistered pins FR54: /healthz returns 200 OK with
// body "ok" regardless of endpoint configuration.
func TestBuildMux_HealthzRegistered(t *testing.T) {
	cfg := &config.Config{
		Endpoints: []config.EndpointConfig{
			{
				Path:    "/api/contact",
				To:      []string{"to@example.com"},
				From:    "from@example.com",
				Subject: "S", Body: "B",
				Transport: config.TransportConfig{
					Type:     "postmark",
					Settings: map[string]any{"api_key": "k"},
				},
			},
		},
	}
	mux, _, _, err := muxRegRec(cfg)
	if err != nil {
		t.Fatalf("buildMux: %v", err)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("/healthz body = %q, want %q", rec.Body.String(), "ok")
	}
}

// TestBuildMux_MetricsRegistered pins FR55: /metrics returns the
// Prometheus exposition format. The body should at least contain the
// metric family names registered by NewRecorder.
func TestBuildMux_MetricsRegistered(t *testing.T) {
	cfg := &config.Config{
		Endpoints: []config.EndpointConfig{
			{
				Path:    "/api/contact",
				To:      []string{"to@example.com"},
				From:    "from@example.com",
				Subject: "S", Body: "B",
				Transport: config.TransportConfig{
					Type:     "postmark",
					Settings: map[string]any{"api_key": "k"},
				},
			},
		},
	}
	mux, _, _, err := muxRegRec(cfg)
	if err != nil {
		t.Fatalf("buildMux: %v", err)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/metrics status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// Each registered metric appears in HELP/TYPE lines even before
	// any observations are recorded.
	for _, want := range []string{
		"# TYPE posthorn_submissions_received_total counter",
		"# TYPE posthorn_submissions_sent_total counter",
		"# TYPE posthorn_submissions_failed_total counter",
		"# TYPE posthorn_send_latency_seconds histogram",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in /metrics body:\n%s", want, body)
		}
	}
	// Content-Type should be Prometheus exposition.
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("/metrics Content-Type = %q, want text/plain prefix", rec.Header().Get("Content-Type"))
	}
}

// TestBuildMux_MetricsObservedAfterRequest confirms the mux-wired
// Recorder records observations when traffic actually flows through a
// gateway handler. Sends a request to /api/contact (will fail at
// transport since the Postmark key is fake, hitting submission_failed),
// then asserts the failure shows up in /metrics.
func TestBuildMux_MetricsObservedAfterRequest(t *testing.T) {
	cfg := &config.Config{
		Endpoints: []config.EndpointConfig{
			{
				Path:    "/api/contact",
				To:      []string{"to@example.com"},
				From:    "from@example.com",
				Subject: "S", Body: "B",
				Required: []string{"name"},
				Transport: config.TransportConfig{
					Type:     "postmark",
					Settings: map[string]any{"api_key": "k"},
				},
			},
		},
	}
	mux, _, _, err := muxRegRec(cfg)
	if err != nil {
		t.Fatalf("buildMux: %v", err)
	}

	// Send a request that will fail validation (no required `name`).
	// validation_failed is recorded; submission_received isn't (we
	// haven't passed validation).
	req := httptest.NewRequest(http.MethodPost, "/api/contact", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(httptest.NewRecorder(), req)

	// Now scrape /metrics.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `posthorn_validation_failed_total{endpoint="/api/contact"} 1`) {
		t.Errorf("validation_failed counter not incremented: %s", body)
	}
}

// --- Block F, Stories 19.2 / 19.3: multiple listeners in one process ---

// twoListenersTOML declares two listeners with different providers. The
// two %s verbs are each transport's base_url, so a test can point them
// at stub servers; empty means the provider's real URL (never dialed by
// the validate tests).
const twoListenersTOML = validTOML + `
[[smtp_listeners]]
name = "tenant-a"
listen = "127.0.0.1:0"
require_tls = false
allowed_senders = ["*@a.example"]

[[smtp_listeners.smtp_users]]
username = "a"
password = "pa"

[smtp_listeners.transport]
type = "postmark"

[smtp_listeners.transport.settings]
api_key = "ka"
base_url = "%s"

[[smtp_listeners]]
name = "tenant-b"
listen = "127.0.0.1:0"
require_tls = false
allowed_senders = ["*@b.example"]

[[smtp_listeners.smtp_users]]
username = "b"
password = "pb"

[smtp_listeners.transport]
type = "resend"

[smtp_listeners.transport.settings]
api_key = "kb"
base_url = "%s"
`

func TestRunValidate_MultipleListeners_OK(t *testing.T) {
	path := writeConfig(t, fmt.Sprintf(twoListenersTOML, "", ""))
	if err := runValidate([]string{"--config", path}); err != nil {
		t.Errorf("runValidate: %v", err)
	}
}

func TestRunValidate_MultipleListeners_SemanticErrorNamesListener(t *testing.T) {
	// Second listener leaves require_tls at its default (true) with no
	// cert: the SMTP-level check must fail and name that listener and the
	// key the way the operator wrote them. "smtp_listener.tls_cert" would
	// point at a table this config doesn't have.
	cfg := fmt.Sprintf(twoListenersTOML, "", "")
	const second = "allowed_senders = [\"*@b.example\"]"
	i := strings.Index(cfg, second)
	if i < 0 {
		t.Fatal("fixture changed: second listener not found")
	}
	head, tail := cfg[:i], cfg[i:]
	j := strings.LastIndex(head, "require_tls = false\n")
	cfg = head[:j] + head[j+len("require_tls = false\n"):] + tail

	err := runValidate([]string{"--config", writeConfig(t, cfg)})
	if err == nil {
		t.Fatal("expected a validation error")
	}
	if want := "smtp_listeners[1] (tenant-b): tls_cert: required"; !strings.Contains(err.Error(), want) {
		t.Errorf("error should contain %q, got %v", want, err)
	}
	if strings.Contains(err.Error(), "smtp_listener.") {
		t.Errorf("error names the single-table key in an array-form config: %v", err)
	}
}

// providerStub stands in for a provider's HTTP API. It records, per
// request, the credential header it was sent and the JSON body, so a
// test can tell which listener's transport made the call.
type providerStub struct {
	srv *httptest.Server

	mu   sync.Mutex
	hits []providerHit
}

type providerHit struct {
	credential string // X-Postmark-Server-Token or Authorization
	body       map[string]any
}

func newProviderStub(t *testing.T, reply string) *providerStub {
	t.Helper()
	p := &providerStub{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		hit := providerHit{credential: r.Header.Get("X-Postmark-Server-Token")}
		if hit.credential == "" {
			hit.credential = r.Header.Get("Authorization")
		}
		_ = json.Unmarshal(raw, &hit.body)
		p.mu.Lock()
		p.hits = append(p.hits, hit)
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *providerStub) Hits() []providerHit {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]providerHit(nil), p.hits...)
}

// boundAddr waits for a listener started with a ":0" address to bind
// and returns where. No sleep: it polls the listener itself.
func boundAddr(t *testing.T, ing ingress.Ingress) string {
	t.Helper()
	l, ok := ing.(*smtp.Listener)
	if !ok {
		t.Fatalf("ingress is %T, want *smtp.Listener", ing)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if a := l.Addr(); a != nil {
			return a.String()
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("listener %s did not bind within 5s", ing.Name())
	return ""
}

// smtpSubmit runs one SMTP transaction and returns the reply code of
// the step that ended it: the AUTH or MAIL rejection, or the code
// answering the end of DATA. headerLines are extra header lines for the
// message.
func smtpSubmit(t *testing.T, addr, user, pass, from, to string, headerLines ...string) int {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	tp := textproto.NewConn(conn)
	defer func() { _ = tp.Close() }()

	read := func() int {
		code, _, err := tp.ReadResponse(0)
		if err != nil {
			t.Fatalf("read reply: %v", err)
		}
		return code
	}
	step := func(want int, format string, args ...any) (code int, ok bool) {
		if err := tp.PrintfLine(format, args...); err != nil {
			t.Fatalf("write: %v", err)
		}
		code = read()
		return code, code == want
	}

	if code := read(); code != 220 {
		t.Fatalf("greeting = %d", code)
	}
	if code, ok := step(250, "EHLO client.test"); !ok {
		t.Fatalf("EHLO = %d", code)
	}
	creds := base64.StdEncoding.EncodeToString([]byte("\x00" + user + "\x00" + pass))
	if code, ok := step(235, "AUTH PLAIN %s", creds); !ok {
		return code
	}
	if code, ok := step(250, "MAIL FROM:<%s>", from); !ok {
		return code
	}
	if code, ok := step(250, "RCPT TO:<%s>", to); !ok {
		return code
	}
	if code, ok := step(354, "DATA"); !ok {
		return code
	}
	headers := "Subject: Hi\r\n"
	for _, line := range headerLines {
		headers += line + "\r\n"
	}
	code, _ := step(250, "%s", headers+"\r\nBody.\r\n.")
	return code
}

// TestSMTPListeners_EndToEnd_EachListenerUsesItsOwnTransport is the
// Story 19.3 end-to-end test (FR96): two listeners loaded from TOML,
// built and started the way `serve` does it, each given a real SMTP
// submission. Mail accepted on tenant-a must leave through tenant-a's
// provider account and never tenant-b's, and the other way round.
func TestSMTPListeners_EndToEnd_EachListenerUsesItsOwnTransport(t *testing.T) {
	postmark := newProviderStub(t, `{"MessageID":"pm-1"}`)
	resend := newProviderStub(t, `{"id":"re-1"}`)

	cfg, err := config.Load(writeConfig(t, fmt.Sprintf(twoListenersTOML, postmark.srv.URL, resend.srv.URL)))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	transports := map[string]transport.Transport{}
	ings, err := buildSMTPIngresses(cfg, buildLogger(config.LoggingConfig{Level: "error"}), nil, nil, transports)
	if err != nil {
		t.Fatalf("buildSMTPIngresses: %v", err)
	}
	if len(ings) != 2 {
		t.Fatalf("ingresses = %d, want 2", len(ings))
	}
	// Queued replays resolve a transport by listener name.
	for _, name := range []string{"tenant-a", "tenant-b"} {
		if _, ok := transports[name]; !ok {
			t.Errorf("transports missing %q", name)
		}
	}
	if _, ok := transports[config.DefaultSMTPListenerName]; ok {
		t.Error("default name must not be registered when every listener is named")
	}

	startErr := make(chan error, len(ings))
	for _, ing := range ings {
		go func(ing ingress.Ingress) { startErr <- ing.Start(context.Background()) }(ing)
	}
	addrA, addrB := boundAddr(t, ings[0]), boundAddr(t, ings[1])

	// Tenant A's submission goes to Postmark with tenant A's token.
	if code := smtpSubmit(t, addrA, "a", "pa", "noreply@a.example", "alice@example.org"); code != 250 {
		t.Fatalf("tenant-a submission = %d, want 250", code)
	}
	if hits := postmark.Hits(); len(hits) != 1 || hits[0].credential != "ka" || hits[0].body["From"] != "noreply@a.example" {
		t.Fatalf("postmark stub hits = %+v, want one send from tenant-a with its own token", hits)
	}
	if hits := resend.Hits(); len(hits) != 0 {
		t.Fatalf("tenant-a mail reached tenant-b's provider: %+v", hits)
	}

	// Tenant B's goes to Resend with tenant B's key.
	if code := smtpSubmit(t, addrB, "b", "pb", "noreply@b.example", "bob@example.org"); code != 250 {
		t.Fatalf("tenant-b submission = %d, want 250", code)
	}
	if hits := resend.Hits(); len(hits) != 1 || hits[0].credential != "Bearer kb" || hits[0].body["from"] != "noreply@b.example" {
		t.Fatalf("resend stub hits = %+v, want one send from tenant-b with its own key", hits)
	}
	if hits := postmark.Hits(); len(hits) != 1 {
		t.Fatalf("tenant-b mail reached tenant-a's provider: %+v", hits)
	}

	// Per-listener credentials and allowlists: tenant A's login is not
	// valid on tenant B's listener, and tenant B can't send as tenant A.
	if code := smtpSubmit(t, addrB, "a", "pa", "noreply@a.example", "x@example.org"); code != 535 {
		t.Errorf("tenant-a credentials on tenant-b's listener = %d, want 535", code)
	}
	if code := smtpSubmit(t, addrB, "b", "pb", "noreply@a.example", "x@example.org"); code != 550 {
		t.Errorf("tenant-a sender on tenant-b's listener = %d, want 550", code)
	}
	if a, b := len(postmark.Hits()), len(resend.Hits()); a != 1 || b != 1 {
		t.Errorf("rejected submissions reached a provider: postmark=%d resend=%d", a, b)
	}

	// Shutdown drains both.
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := stopIngresses(stopCtx, ings); err != nil {
		t.Errorf("stopIngresses: %v", err)
	}
	for range ings {
		if err := <-startErr; err != nil {
			t.Errorf("Start returned error: %v", err)
		}
	}
}

// stubIngress is an ingress whose Stop either returns at once or blocks
// until the shutdown deadline, like a listener holding an idle
// connection.
type stubIngress struct {
	name       string
	blocks     bool
	stopCalled chan struct{}
}

func (s *stubIngress) Name() string                { return s.name }
func (s *stubIngress) Start(context.Context) error { return nil }
func (s *stubIngress) Stop(ctx context.Context) error {
	close(s.stopCalled)
	if !s.blocks {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

// TestStopIngresses_StopsAllAtOnce: one ingress that can't drain must
// not hold up the others. Stopped in sequence, the second would keep
// accepting mail until the first gave up, then get no time of its own.
func TestStopIngresses_StopsAllAtOnce(t *testing.T) {
	slow := &stubIngress{name: `smtp "tenant-a"`, blocks: true, stopCalled: make(chan struct{})}
	fast := &stubIngress{name: `smtp "tenant-b"`, stopCalled: make(chan struct{})}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- stopIngresses(ctx, []ingress.Ingress{slow, fast}) }()

	select {
	case <-fast.stopCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("second ingress was not stopped while the first was still draining")
	}

	cancel() // the shutdown deadline
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want the deadline error", err)
		}
		if !strings.Contains(err.Error(), `smtp "tenant-a" ingress graceful shutdown`) {
			t.Errorf("error should name the ingress that timed out: %v", err)
		}
		if strings.Contains(err.Error(), "tenant-b") {
			t.Errorf("error names an ingress that stopped cleanly: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stopIngresses did not return after the deadline")
	}
}

// TestCheckUnnamedListenerQueue covers the upgrade path that naming a
// listener opens: rows queued under the unnamed listener's key must not
// be silently dead-lettered once no listener carries that name.
func TestCheckUnnamedListenerQueue(t *testing.T) {
	queuedStore := func(t *testing.T, endpoint string) *storage.Store {
		t.Helper()
		store, err := storage.Open(storage.Config{InMemory: true})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		if endpoint == "" {
			return store
		}
		now := time.Now()
		if err := store.RecordSubmission(storage.Submission{
			ID: "sub-1", Endpoint: endpoint, Status: storage.StatusSending, CreatedAt: now,
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
		if err := store.Enqueue("sub-1", now); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		return store
	}
	named := func(names ...string) (*config.Config, map[string]transport.Transport) {
		cfg := &config.Config{}
		transports := map[string]transport.Transport{}
		for _, n := range names {
			cfg.SMTPListeners = append(cfg.SMTPListeners, config.SMTPListenerConfig{Name: n})
			transports[n] = nil
		}
		return cfg, transports
	}
	logger := buildLogger(config.LoggingConfig{Level: "error"})

	t.Run("queued under the old key, no listener has it: refuse", func(t *testing.T) {
		cfg, transports := named("tenant-a", "tenant-b")
		err := checkUnnamedListenerQueue(cfg, queuedStore(t, config.DefaultSMTPListenerName), transports, logger)
		if err == nil {
			t.Fatal("expected startup to be refused")
		}
		for _, w := range []string{"1 queued submission(s)", `name = "smtp_listener"`} {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("error should contain %q: %v", w, err)
			}
		}
	})
	t.Run("a listener keeps the old name: start", func(t *testing.T) {
		cfg, transports := named(config.DefaultSMTPListenerName, "tenant-b")
		if err := checkUnnamedListenerQueue(cfg, queuedStore(t, config.DefaultSMTPListenerName), transports, logger); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
	t.Run("nothing queued under the old key: start", func(t *testing.T) {
		cfg, transports := named("tenant-a", "tenant-b")
		if err := checkUnnamedListenerQueue(cfg, queuedStore(t, ""), transports, logger); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if err := checkUnnamedListenerQueue(cfg, queuedStore(t, "/api/contact"), transports, logger); err != nil {
			t.Errorf("rows for another endpoint must not block startup: %v", err)
		}
	})
	t.Run("no listeners at all: existing dead-letter behavior, start", func(t *testing.T) {
		cfg, transports := named()
		if err := checkUnnamedListenerQueue(cfg, queuedStore(t, config.DefaultSMTPListenerName), transports, logger); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

// --- Block F, Story 20.1: passthrough_headers from TOML to the provider ---

// TestPassthroughHeaders_ConfigToProvider follows passthrough_headers
// the whole way: TOML, config.Load, buildSMTPIngresses, a real SMTP
// submission, and the provider request. The config tests stop at the
// parsed struct and the SMTP tests set the field by hand, so without
// this the one line that copies it from one to the other could be
// deleted with every test still green (the shape of the
// redirect_success bug and of #123).
func TestPassthroughHeaders_ConfigToProvider(t *testing.T) {
	postmark := newProviderStub(t, `{"MessageID":"pm-1"}`)
	resend := newProviderStub(t, `{"id":"re-1"}`)

	toml := fmt.Sprintf(twoListenersTOML, postmark.srv.URL, resend.srv.URL)
	// tenant-a opts in; tenant-b does not.
	const marker = "allowed_senders = [\"*@a.example\"]\n"
	if !strings.Contains(toml, marker) {
		t.Fatal("fixture changed: tenant-a block not found")
	}
	toml = strings.Replace(toml, marker, marker+"passthrough_headers = [\"list-unsubscribe\", \"List-Unsubscribe-Post\"]\n", 1)

	cfg, err := config.Load(writeConfig(t, toml))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	ings, err := buildSMTPIngresses(cfg, buildLogger(config.LoggingConfig{Level: "error"}), nil, nil, map[string]transport.Transport{})
	if err != nil {
		t.Fatalf("buildSMTPIngresses: %v", err)
	}
	startErr := make(chan error, len(ings))
	for _, ing := range ings {
		go func(ing ingress.Ingress) { startErr <- ing.Start(context.Background()) }(ing)
	}
	addrA, addrB := boundAddr(t, ings[0]), boundAddr(t, ings[1])
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = stopIngresses(ctx, ings)
	})

	lines := []string{
		"List-Unsubscribe: <https://lists.example/u/abc>",
		"List-Unsubscribe-Post: List-Unsubscribe=One-Click",
		"List-Id: Weekly <weekly.lists.example>", // not configured: stays behind
	}

	if code := smtpSubmit(t, addrA, "a", "pa", "noreply@a.example", "alice@example.org", lines...); code != 250 {
		t.Fatalf("tenant-a submission = %d, want 250", code)
	}
	hits := postmark.Hits()
	if len(hits) != 1 {
		t.Fatalf("postmark hits = %d, want 1", len(hits))
	}
	got, _ := json.Marshal(hits[0].body["Headers"])
	want := `[{"Name":"List-Unsubscribe","Value":"<https://lists.example/u/abc>"},{"Name":"List-Unsubscribe-Post","Value":"List-Unsubscribe=One-Click"}]`
	var gotAny, wantAny any
	_ = json.Unmarshal(got, &gotAny)
	_ = json.Unmarshal([]byte(want), &wantAny)
	if fmt.Sprint(gotAny) != fmt.Sprint(wantAny) {
		t.Errorf("provider request Headers = %s, want %s", got, want)
	}

	// A listener that didn't opt in carries none, as in v2.0 (FR97).
	if code := smtpSubmit(t, addrB, "b", "pb", "noreply@b.example", "bob@example.org", lines...); code != 250 {
		t.Fatalf("tenant-b submission = %d, want 250", code)
	}
	hitsB := resend.Hits()
	if len(hitsB) != 1 {
		t.Fatalf("resend hits = %d, want 1", len(hitsB))
	}
	if h, present := hitsB[0].body["headers"]; present {
		t.Errorf("tenant-b did not configure passthrough_headers but its request carries headers: %v", h)
	}
}

func TestRunValidate_PassthroughHeadersOnWebhookTransport_Refused(t *testing.T) {
	// FR99: a transport that can't carry headers fails validation instead
	// of dropping them at send time.
	cfg := `
[smtp_listener]
listen = "127.0.0.1:2525"
require_tls = false
allowed_senders = ["*@example.com"]
passthrough_headers = ["List-Unsubscribe"]

[[smtp_listener.smtp_users]]
username = "u"
password = "p"

[smtp_listener.transport]
type = "webhook"

[smtp_listener.transport.settings]
url = "https://hooks.example/posthorn"
secret = "0123456789abcdef0123456789abcdef"
`
	err := runValidate([]string{"--config", writeConfig(t, cfg)})
	if err == nil {
		t.Fatal("expected passthrough_headers on a webhook transport to be refused")
	}
	for _, w := range []string{"smtp_listener", "passthrough_headers", "webhook"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error should contain %q: %v", w, err)
		}
	}
	// The same listener without passthrough_headers is fine.
	ok := strings.Replace(cfg, "passthrough_headers = [\"List-Unsubscribe\"]\n", "", 1)
	if err := runValidate([]string{"--config", writeConfig(t, ok)}); err != nil {
		t.Errorf("webhook listener without passthrough_headers: %v", err)
	}
}
