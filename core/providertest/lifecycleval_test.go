// Hermetic tests for the lifecycle live-validation support code. These
// run in the default suite on every PR: the Postmark API client is
// exercised against a fake server, the generated config is loaded
// through the real config.Load, and the receiver's independent HMAC
// verification is checked against the documented FR84 contract. The
// tagged orchestration in lifecyclelive_test.go is the only part that
// needs a network.
package providertest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/craigmccaskill/posthorn/config"
)

func TestParseQuickTunnelURL(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
		ok   bool
	}{
		{
			"banner_line",
			"2026-09-09T10:00:00Z INF |  Your quick Tunnel has been created! Visit it at (it may take some time to be reachable):  |",
			"", false,
		},
		{
			"url_line",
			"2026-09-09T10:00:00Z INF |  https://tame-poem-example-quick.trycloudflare.com                                         |",
			"https://tame-poem-example-quick.trycloudflare.com", true,
		},
		{"bare_url", "https://a1-b2.trycloudflare.com", "https://a1-b2.trycloudflare.com", true},
		{"unrelated_url", "connecting to https://api.trycloudflare.example.com", "", false},
		{"empty", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseQuickTunnelURL(tt.line)
			if ok != tt.ok || got != tt.want {
				t.Errorf("parseQuickTunnelURL(%q) = (%q, %v), want (%q, %v)", tt.line, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestVerifyEventSignature(t *testing.T) {
	secret := "validation-webhook-secret"
	body := []byte(`{"event":"delivered","submission_id":"abc"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	good := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !verifyEventSignature(secret, body, good) {
		t.Error("valid signature rejected")
	}
	if verifyEventSignature(secret, body, "sha256=deadbeef") {
		t.Error("wrong signature accepted")
	}
	if verifyEventSignature(secret, append(body, ' '), good) {
		t.Error("tampered body accepted")
	}
	if verifyEventSignature("other-secret", body, good) {
		t.Error("wrong secret accepted")
	}
	if verifyEventSignature(secret, body, "") {
		t.Error("empty header accepted")
	}
}

// fakePostmarkAPI records requests so the tests can assert the exact
// wire shape the client produces.
type fakePostmarkAPI struct {
	mu       sync.Mutex
	requests []recordedRequest
	respond  func(w http.ResponseWriter, r *http.Request)
}

type recordedRequest struct {
	Method string
	Path   string
	Token  string
	Body   []byte
}

func (f *fakePostmarkAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Token:  r.Header.Get("X-Postmark-Server-Token"),
		Body:   body,
	})
	f.mu.Unlock()
	if f.respond != nil {
		f.respond(w, r)
		return
	}
	_, _ = w.Write([]byte(`{}`))
}

func (f *fakePostmarkAPI) last(t *testing.T) recordedRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("no request reached the fake Postmark API")
	}
	return f.requests[len(f.requests)-1]
}

func TestPostmarkServerAPI_CreateWebhook(t *testing.T) {
	fake := &fakePostmarkAPI{respond: func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ID": 4242}`))
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	api := &postmarkServerAPI{Token: "sentinel-server-token", BaseURL: srv.URL, Client: srv.Client()}
	id, err := api.CreateWebhook(context.Background(), postmarkWebhookRequest{
		URL:           "https://example.trycloudflare.com/events/postmark",
		MessageStream: "outbound",
		HTTPAuth:      postmarkWebhookAuth{Username: "u", Password: "p"},
		Triggers:      validationTriggers(),
	})
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	if id != 4242 {
		t.Errorf("ID = %d, want 4242", id)
	}

	req := fake.last(t)
	if req.Method != http.MethodPost || req.Path != "/webhooks" {
		t.Errorf("request = %s %s, want POST /webhooks", req.Method, req.Path)
	}
	if req.Token != "sentinel-server-token" {
		t.Errorf("token header = %q", req.Token)
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(req.Body, &sent); err != nil {
		t.Fatalf("request body not JSON: %v", err)
	}
	for _, key := range []string{"Url", "MessageStream", "HttpAuth", "Triggers"} {
		if _, ok := sent[key]; !ok {
			t.Errorf("request body missing %q key: %s", key, req.Body)
		}
	}
	if !bytes.Contains(req.Body, []byte(`"Delivery":{"Enabled":true}`)) {
		t.Errorf("Delivery trigger not enabled in body: %s", req.Body)
	}
}

func TestPostmarkServerAPI_ListAndDeleteWebhooks(t *testing.T) {
	fake := &fakePostmarkAPI{respond: func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"Webhooks":[{"ID":1,"Url":"https://old.trycloudflare.com/events/postmark"},{"ID":2,"Url":"https://apps.example.com/hook"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	api := &postmarkServerAPI{Token: "t", BaseURL: srv.URL, Client: srv.Client()}
	hooks, err := api.ListWebhooks(context.Background())
	if err != nil {
		t.Fatalf("ListWebhooks: %v", err)
	}
	if len(hooks) != 2 || hooks[0].ID != 1 || hooks[0].URL != "https://old.trycloudflare.com/events/postmark" {
		t.Errorf("unexpected webhooks: %+v", hooks)
	}

	if err := api.DeleteWebhook(context.Background(), 1); err != nil {
		t.Fatalf("DeleteWebhook: %v", err)
	}
	req := fake.last(t)
	if req.Method != http.MethodDelete || req.Path != "/webhooks/1" {
		t.Errorf("request = %s %s, want DELETE /webhooks/1", req.Method, req.Path)
	}
}

func TestPostmarkServerAPI_DeleteSuppressions(t *testing.T) {
	fake := &fakePostmarkAPI{}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	api := &postmarkServerAPI{Token: "t", BaseURL: srv.URL, Client: srv.Client()}
	if err := api.DeleteSuppressions(context.Background(), "outbound", []string{postmarkHardBounceAddress}); err != nil {
		t.Fatalf("DeleteSuppressions: %v", err)
	}
	req := fake.last(t)
	if req.Method != http.MethodPost || req.Path != "/message-streams/outbound/suppressions/delete" {
		t.Errorf("request = %s %s, want POST /message-streams/outbound/suppressions/delete", req.Method, req.Path)
	}
	want := `{"Suppressions":[{"EmailAddress":"` + postmarkHardBounceAddress + `"}]}`
	if strings.TrimSpace(string(req.Body)) != want {
		t.Errorf("body = %s, want %s", req.Body, want)
	}
}

func TestPostmarkServerAPI_ErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"ErrorCode":300,"Message":"Invalid webhook URL"}`))
	}))
	defer srv.Close()

	api := &postmarkServerAPI{Token: "t", BaseURL: srv.URL, Client: srv.Client()}
	_, err := api.CreateWebhook(context.Background(), postmarkWebhookRequest{})
	if err == nil {
		t.Fatal("expected error on 422")
	}
	if !strings.Contains(err.Error(), "422") || !strings.Contains(err.Error(), "Invalid webhook URL") {
		t.Errorf("error should carry status and provider message, got: %v", err)
	}
}

func TestEventReceiver(t *testing.T) {
	rec := newEventReceiver("shared-secret")
	srv := httptest.NewServer(rec)
	defer srv.Close()

	body := []byte(`{"event":"hard_bounce","submission_id":"sub-1","endpoint":"/validate/bounce","recipient":"hardbounce@bounce-testing.postmarkapp.com"}`)
	mac := hmac.New(sha256.New, []byte("shared-secret"))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	post := func(sig string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(body))
		req.Header.Set("X-Posthorn-Signature", sig)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("receiver answered %d, want 200", resp.StatusCode)
		}
	}

	post(sig)
	ev := <-rec.Events
	if !ev.SignatureOK {
		t.Error("valid signature reported as failed")
	}
	if ev.Event != "hard_bounce" || ev.SubmissionID != "sub-1" || ev.Endpoint != "/validate/bounce" {
		t.Errorf("event fields not parsed: %+v", ev)
	}

	post("sha256=0000")
	ev = <-rec.Events
	if ev.SignatureOK {
		t.Error("invalid signature reported as OK")
	}
}

// TestLifecycleValidationConfig proves the generated config is one the
// real loader accepts (storage + lifecycle + two webhook-bearing
// endpoints), that the env placeholder resolves, and that the server
// token never appears literally in the rendered TOML (NFR3 discipline:
// the only on-disk reference is the ${env...} placeholder).
func TestLifecycleValidationConfig(t *testing.T) {
	const sentinel = "sentinel-postmark-token-8a1"
	t.Setenv("POSTMARK_SERVER_TOKEN", sentinel)

	dir := t.TempDir()
	rendered := lifecycleValidationConfig(lifecycleValidationOpts{
		DBPath:        filepath.Join(dir, "validate.db"),
		BasicAuthUser: "lifecycle-user",
		BasicAuthPass: "lifecycle-pass-0123456789",
		From:          "validator@example.com",
		DeliveryTo:    "inbox@example.com",
		WebhookURL:    "http://127.0.0.1:9999/events",
		WebhookSecret: "webhook-secret-0123456789",
	})

	if strings.Contains(rendered, sentinel) {
		t.Fatal("rendered config contains the literal server token; it must stay an env placeholder")
	}

	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(rendered), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load rejected the generated config: %v\n---\n%s", err, rendered)
	}

	if cfg.Storage == nil || cfg.Lifecycle == nil {
		t.Fatal("storage or lifecycle block missing after load")
	}
	if len(cfg.Endpoints) != 2 {
		t.Fatalf("endpoints = %d, want 2", len(cfg.Endpoints))
	}
	if cfg.Endpoints[0].Path != "/validate/delivery" || cfg.Endpoints[1].Path != "/validate/bounce" {
		t.Errorf("endpoint paths = %q, %q", cfg.Endpoints[0].Path, cfg.Endpoints[1].Path)
	}
	if got := cfg.Endpoints[1].To[0]; got != postmarkHardBounceAddress {
		t.Errorf("bounce endpoint to = %q, want the Postmark blackhole address", got)
	}
	for i, ep := range cfg.Endpoints {
		if ep.WebhookURL == "" || ep.WebhookSecret == "" {
			t.Errorf("endpoints[%d]: webhook_url/webhook_secret not set", i)
		}
		if got := ep.Transport.Settings["api_key"]; got != sentinel {
			t.Errorf("endpoints[%d]: api_key env placeholder resolved to %v, want sentinel", i, got)
		}
	}
}
