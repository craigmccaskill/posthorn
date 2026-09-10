//go:build lifecyclelive

// Story 18.3: the live lifecycle validation. This is the release-gate
// check that the v2.0 lifecycle loop works against the real world — real
// Postmark webhook senders POSTing into a really-running posthorn binary
// — which the hermetic suite cannot prove (it fakes Postmark's side).
//
// It is deliberately runnable by anyone, anywhere, with no standing
// infrastructure: the posthorn under test runs locally from a generated
// config, and reachability for Postmark's webhooks comes from an
// anonymous cloudflared "quick tunnel" (no Cloudflare account, no
// credentials, ephemeral URL). The only secret is POSTMARK_SERVER_TOKEN.
// Absent → the test SKIPS, same convention as the -tags integration
// live tier.
//
// What one run proves, in order:
//  1. A real send through the built binary reaches Postmark (delivery leg).
//  2. Postmark's Delivery webhook lands on /events/postmark through the
//     tunnel, authenticates (FR82), correlates (FR83), and is forwarded
//     to the endpoint's webhook_url with a valid HMAC (FR84).
//  3. A send to Postmark's blackhole bounce address produces a real
//     hard-bounce event end-to-end the same way.
//  4. The hard bounce auto-inserts a suppression row (FR85) and a
//     follow-up send answers 200 {"status":"suppressed"} (FR86).
//  5. `posthorn suppressions list` shows the row (FR87).
//
// Run via `make validate-lifecycle`. Requires `cloudflared` on PATH.
package providertest

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	// posthornStartTimeout bounds waiting for /healthz on the local binary.
	posthornStartTimeout = 30 * time.Second
	// tunnelStartTimeout bounds cloudflared printing its quick-tunnel URL.
	tunnelStartTimeout = 90 * time.Second
	// tunnelReachableTimeout bounds the tunnel actually routing traffic —
	// cloudflared warns the URL "may take some time to be reachable".
	tunnelReachableTimeout = 2 * time.Minute
	// eventTimeout bounds each leg's wait for Postmark to deliver the
	// webhook. Delivery to a real mailbox plus webhook dispatch is
	// usually well under a minute; the ceiling absorbs provider lag.
	eventTimeout = 5 * time.Minute
	// suppressionTimeout bounds the follow-up-send-is-suppressed check.
	suppressionTimeout = 60 * time.Second
)

func TestLifecycleLive_PostmarkEndToEnd(t *testing.T) {
	token := os.Getenv("POSTMARK_SERVER_TOKEN")
	if token == "" {
		t.Skip("POSTMARK_SERVER_TOKEN not set — skipping live lifecycle validation")
	}
	from := os.Getenv("POSTHORN_TEST_FROM")
	deliveryTo := os.Getenv("POSTHORN_TEST_TO")
	if from == "" || deliveryTo == "" {
		t.Fatal("POSTHORN_TEST_FROM and POSTHORN_TEST_TO must be set: FROM must be a verified sender signature on the Postmark server, TO a real mailbox that accepts mail")
	}
	cloudflared, err := exec.LookPath("cloudflared")
	if err != nil {
		t.Fatal("cloudflared not found on PATH — install it (https://github.com/cloudflare/cloudflared/releases); the quick tunnel needs no account or credentials")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()

	// --- Build the binary under test.
	bin := filepath.Join(dir, "posthorn")
	build := exec.CommandContext(ctx, "go", "build", "-o", bin, "github.com/craigmccaskill/posthorn/cmd/posthorn")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	// --- Receiver: the stand-in operator app the events must reach.
	webhookSecret := randomToken(t)
	rec := newEventReceiver(webhookSecret)
	recSrv := httptest.NewServer(rec)
	defer recSrv.Close()

	// --- Config + posthorn under test.
	basicUser, basicPass := "lifecycle-validate", randomToken(t)
	cfgPath := filepath.Join(dir, "config.toml")
	cfg := lifecycleValidationConfig(lifecycleValidationOpts{
		DBPath:        filepath.Join(dir, "validate.db"),
		BasicAuthUser: basicUser,
		BasicAuthPass: basicPass,
		From:          from,
		DeliveryTo:    deliveryTo,
		WebhookURL:    recSrv.URL + "/events",
		WebhookSecret: webhookSecret,
	})
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	listen := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	posthornLog := startProcess(t, ctx, bin, "serve", "--config", cfgPath, "--listen", listen)
	waitHTTP(t, "http://"+listen+"/healthz", posthornStartTimeout, "posthorn /healthz")
	t.Logf("posthorn under test listening on %s", listen)

	// --- Quick tunnel: the ephemeral public URL Postmark will POST to.
	tunnelURL := startQuickTunnel(t, ctx, cloudflared, "http://"+listen)
	t.Logf("quick tunnel: %s", tunnelURL)
	waitHTTP(t, tunnelURL+"/healthz", tunnelReachableTimeout, "tunnel routing")

	// --- Postmark server hygiene + webhook registration.
	pm := &postmarkServerAPI{Token: token}
	cleanupStaleWebhooks(t, pm)
	// Postmark suppresses the blackhole address on its own list after
	// each bounce; clear it so this run's bounce send actually goes out.
	if err := pm.DeleteSuppressions(ctx, "outbound", []string{postmarkHardBounceAddress}); err != nil {
		t.Fatalf("clearing provider-side suppression of the blackhole address: %v", err)
	}
	hookID, err := pm.CreateWebhook(ctx, postmarkWebhookRequest{
		URL:           tunnelURL + "/events/postmark",
		MessageStream: "outbound",
		HTTPAuth:      postmarkWebhookAuth{Username: basicUser, Password: basicPass},
		Triggers:      validationTriggers(),
	})
	if err != nil {
		t.Fatalf("registering webhook on Postmark: %v", err)
	}
	t.Logf("registered Postmark webhook %d → %s/events/postmark", hookID, tunnelURL)
	defer func() {
		// Teardown uses a fresh context: the test context may be done.
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		if err := pm.DeleteWebhook(dctx, hookID); err != nil {
			t.Errorf("teardown: deleting webhook %d failed (next run's stale-webhook sweep will catch it): %v", hookID, err)
		}
	}()

	runID := randomToken(t)

	// --- Leg 1: delivery.
	subDelivery := submitForm(t, "http://"+listen+"/validate/delivery", runID)
	t.Logf("delivery-leg submission %s accepted; waiting for the delivered event", subDelivery)
	ev := waitForEvent(t, rec, "delivered", subDelivery)
	assertEvent(t, ev, "/validate/delivery")

	// --- Leg 2: hard bounce.
	subBounce := submitForm(t, "http://"+listen+"/validate/bounce", runID)
	t.Logf("bounce-leg submission %s accepted; waiting for the hard_bounce event", subBounce)
	ev = waitForEvent(t, rec, "hard_bounce", subBounce)
	assertEvent(t, ev, "/validate/bounce")

	// --- Leg 3: the suppression the bounce must have created (FR85/86).
	waitForSuppressedSend(t, "http://"+listen+"/validate/bounce", runID)

	// --- Leg 4: the CLI management surface (FR87).
	list := exec.CommandContext(ctx, bin, "suppressions", "list", "--config", cfgPath)
	out, err := list.CombinedOutput()
	if err != nil {
		t.Fatalf("posthorn suppressions list: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), postmarkHardBounceAddress) {
		t.Errorf("suppressions list does not show %s:\n%s", postmarkHardBounceAddress, out)
	}

	t.Logf("lifecycle validation passed: delivery + hard_bounce forwarded with valid signatures, suppression enforced, CLI lists the row")
	_ = posthornLog // referenced so the capture lives for the whole run
}

// randomToken returns 24 hex chars from crypto/rand — used for the
// basic-auth password, webhook secret (>=16 bytes per config
// validation), and run ID.
func randomToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// startProcess launches a subprocess wired into the test lifecycle: its
// combined output is captured (and dumped on failure), and it is killed
// via t.Cleanup. Returns the output buffer.
func startProcess(t *testing.T, ctx context.Context, name string, args ...string) *lockedBuffer {
	t.Helper()
	cmd := exec.CommandContext(ctx, name, args...)
	buf := &lockedBuffer{}
	cmd.Stdout = buf
	cmd.Stderr = buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("--- output of %s ---\n%s", filepath.Base(name), buf.String())
		}
	})
	return buf
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startQuickTunnel runs `cloudflared tunnel --url <target>` and returns
// the ephemeral trycloudflare.com URL it prints. The process dies with
// the test via t.Cleanup.
func startQuickTunnel(t *testing.T, ctx context.Context, cloudflared, target string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, cloudflared, "tunnel", "--url", target)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting cloudflared: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	urlCh := make(chan string, 1)
	scan := func(r io.Reader) {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			if u, ok := parseQuickTunnelURL(sc.Text()); ok {
				select {
				case urlCh <- u:
				default:
				}
			}
		}
	}
	go scan(stdout)
	go scan(stderr)

	select {
	case u := <-urlCh:
		return u
	case <-time.After(tunnelStartTimeout):
		t.Fatal("cloudflared did not print a quick-tunnel URL in time — check outbound connectivity (QUIC 7844 or HTTPS 443)")
		return ""
	}
}

// waitHTTP polls a URL until it answers 200.
func waitHTTP(t *testing.T, url string, timeout time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 10 * time.Second}
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("%s not ready after %s: %v", what, timeout, lastErr)
}

// submitForm POSTs the validation form and returns the submission_id
// from the Success envelope.
func submitForm(t *testing.T, endpoint, runID string) string {
	t.Helper()
	resp, err := http.PostForm(endpoint, url.Values{"run_id": {runID}})
	if err != nil {
		t.Fatalf("POST %s: %v", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s: HTTP %d: %s", endpoint, resp.StatusCode, body)
	}
	var success struct {
		Status       string `json:"status"`
		SubmissionID string `json:"submission_id"`
	}
	if err := json.Unmarshal(body, &success); err != nil {
		t.Fatalf("POST %s: response not JSON: %v: %s", endpoint, err, body)
	}
	if success.SubmissionID == "" {
		t.Fatalf("POST %s: no submission_id in response: %s", endpoint, body)
	}
	return success.SubmissionID
}

// waitForEvent drains the receiver until an event with the wanted type
// and submission ID arrives. Unrelated events (a late event from a prior
// leg, Postmark noise) are logged and skipped, not failed on.
func waitForEvent(t *testing.T, rec *eventReceiver, wantEvent, wantSubmission string) receivedEvent {
	t.Helper()
	deadline := time.After(eventTimeout)
	for {
		select {
		case ev := <-rec.Events:
			if ev.Event == wantEvent && ev.SubmissionID == wantSubmission {
				return ev
			}
			t.Logf("ignoring event %q for submission %q (waiting for %q/%q)", ev.Event, ev.SubmissionID, wantEvent, wantSubmission)
		case <-deadline:
			t.Fatalf("no %q event for submission %s within %s — check the Postmark server's webhook activity for delivery errors", wantEvent, wantSubmission, eventTimeout)
		}
	}
}

func assertEvent(t *testing.T, ev receivedEvent, wantEndpoint string) {
	t.Helper()
	if !ev.SignatureOK {
		t.Errorf("event %q: X-Posthorn-Signature did not verify against the shared secret", ev.Event)
	}
	if ev.Endpoint != wantEndpoint {
		t.Errorf("event %q: endpoint = %q, want %q", ev.Event, ev.Endpoint, wantEndpoint)
	}
}

// waitForSuppressedSend re-submits until the response is the FR86
// suppressed contract. The suppression row is written during event
// ingestion, which completed before the forwarded event was observed,
// so this normally succeeds on the first try; the retry loop absorbs
// any write-visibility lag.
func waitForSuppressedSend(t *testing.T, endpoint, runID string) {
	t.Helper()
	deadline := time.Now().Add(suppressionTimeout)
	var last string
	for time.Now().Before(deadline) {
		resp, err := http.PostForm(endpoint, url.Values{"run_id": {runID}})
		if err != nil {
			t.Fatalf("POST %s: %v", endpoint, err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		var success struct {
			Status     string            `json:"status"`
			Suppressed map[string]string `json:"suppressed"`
		}
		_ = json.Unmarshal(body, &success)
		if resp.StatusCode == http.StatusOK && success.Status == "suppressed" {
			if reason := success.Suppressed[postmarkHardBounceAddress]; reason != "hard_bounce" {
				t.Errorf("suppressed reason for %s = %q, want %q (body: %s)", postmarkHardBounceAddress, reason, "hard_bounce", body)
			}
			return
		}
		last = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, body)
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("send to the bounced address was never suppressed within %s; last response: %s", suppressionTimeout, last)
}

// cleanupStaleWebhooks removes leftover trycloudflare registrations from
// crashed or cancelled runs. Quick-tunnel URLs are single-run ephemera:
// any registration pointing at one is dead by definition.
func cleanupStaleWebhooks(t *testing.T, pm *postmarkServerAPI) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	hooks, err := pm.ListWebhooks(ctx)
	if err != nil {
		t.Fatalf("listing existing Postmark webhooks: %v", err)
	}
	for _, h := range hooks {
		if strings.Contains(h.URL, ".trycloudflare.com") {
			if err := pm.DeleteWebhook(ctx, h.ID); err != nil {
				t.Fatalf("deleting stale webhook %d (%s): %v", h.ID, h.URL, err)
			}
			t.Logf("deleted stale webhook %d → %s", h.ID, h.URL)
		}
	}
}
