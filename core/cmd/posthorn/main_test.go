package main

import (
	"context"
	"github.com/craigmccaskill/posthorn/ingress"
	"github.com/craigmccaskill/posthorn/transport"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/craigmccaskill/posthorn/config"
	"github.com/craigmccaskill/posthorn/metrics"
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

[[smtp_listeners]]
name = "tenant-b"
listen = "localhost:0"
require_tls = false
allowed_senders = ["*@b.example"]

[[smtp_listeners.smtp_users]]
username = "b"
password = "pb"

[smtp_listeners.transport]
type = "resend"

[smtp_listeners.transport.settings]
api_key = "kb"
`

func TestRunValidate_MultipleListeners_OK(t *testing.T) {
	path := writeConfig(t, twoListenersTOML)
	if err := runValidate([]string{"--config", path}); err != nil {
		t.Errorf("runValidate: %v", err)
	}
}

func TestRunValidate_MultipleListeners_SemanticErrorNamesListener(t *testing.T) {
	// Second listener leaves require_tls at its default (true) with no
	// cert: the SMTP-level check must fail and name that listener.
	cfg := strings.Replace(twoListenersTOML, "listen = \"localhost:0\"\nrequire_tls = false\n", "listen = \"localhost:0\"\n", 1)
	path := writeConfig(t, cfg)
	err := runValidate([]string{"--config", path})
	if err == nil || !strings.Contains(err.Error(), "smtp_listeners[1] (tenant-b)") {
		t.Errorf("want an error naming smtp_listeners[1] (tenant-b), got %v", err)
	}
}

func TestBuildSMTPIngresses_TwoListenersStartAndStop(t *testing.T) {
	// FR96: every declared listener is built, registered under its own
	// name for queued replays, started, and drained.
	cfg, err := config.Load(writeConfig(t, twoListenersTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	transports := map[string]transport.Transport{}
	ings, err := buildSMTPIngresses(cfg, buildLogger(cfg.Logging), nil, nil, transports)
	if err != nil {
		t.Fatalf("buildSMTPIngresses: %v", err)
	}
	if len(ings) != 2 {
		t.Fatalf("ingresses = %d, want 2", len(ings))
	}
	for _, name := range []string{"tenant-a", "tenant-b"} {
		if _, ok := transports[name]; !ok {
			t.Errorf("transports missing %q (keys: %v)", name, keys(transports))
		}
	}
	if _, ok := transports["smtp_listener"]; ok {
		t.Error("default label must not be registered when listeners are named")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, len(ings))
	for _, ing := range ings {
		go func(ing ingress.Ingress) { errCh <- ing.Start(ctx) }(ing)
	}
	// Give both accept loops a moment to bind, then drain.
	time.Sleep(100 * time.Millisecond)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	for _, ing := range ings {
		if err := ing.Stop(stopCtx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}
	for range ings {
		if err := <-errCh; err != nil {
			t.Errorf("Start returned error: %v", err)
		}
	}
}

func keys(m map[string]transport.Transport) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
