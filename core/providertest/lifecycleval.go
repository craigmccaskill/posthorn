// Lifecycle live-validation support (Story 18.3). These helpers back the
// `-tags lifecyclelive` orchestration in lifecyclelive_test.go: a minimal
// Postmark server-API client (webhook registration + suppression
// hygiene), the cloudflared quick-tunnel URL parser, the forwarded-event
// receiver with independent HMAC verification, and the generated
// validation config. They carry no build tag so the hermetic suite
// (`make test`) exercises them on every PR; only the orchestration that
// touches the real Postmark API and a real tunnel is tagged.
//
// The receiver recomputes the X-Posthorn-Signature HMAC itself rather
// than importing core/lifecycle's signer: the point of the validation is
// that an INDEPENDENT consumer, holding only the shared secret and the
// documented contract, can verify what Posthorn forwards.
package providertest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sync"
	"time"
)

// postmarkHardBounceAddress is Postmark's documented blackhole address:
// sending to it generates a real hard-bounce event (and webhook) without
// affecting sender reputation or bounce limits. Postmark also adds it to
// the server's own suppression list after each bounce, so runs must
// clear it first via DeleteSuppressions — otherwise the next run's send
// is suppressed provider-side and no bounce event ever fires.
// https://postmarkapp.com/support/article/1239-how-to-test-bounces
const postmarkHardBounceAddress = "hardbounce@bounce-testing.postmarkapp.com"

// postmarkAPIBase is the production Postmark server-API endpoint.
// Injectable on postmarkServerAPI for hermetic tests.
const postmarkAPIBase = "https://api.postmarkapp.com"

// postmarkServerAPI is a minimal client for the two server-API surfaces
// the validation needs: webhook CRUD and suppression deletion. Bespoke
// per ADR-1; the whole thing is well under the ~200-line bar.
type postmarkServerAPI struct {
	Token   string // X-Postmark-Server-Token; never logged (NFR3 discipline)
	BaseURL string // defaults to postmarkAPIBase when empty
	Client  *http.Client
}

func (p *postmarkServerAPI) base() string {
	if p.BaseURL != "" {
		return p.BaseURL
	}
	return postmarkAPIBase
}

func (p *postmarkServerAPI) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (p *postmarkServerAPI) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("postmark api: marshal %s %s: %w", method, path, err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.base()+path, rd)
	if err != nil {
		return fmt.Errorf("postmark api: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Postmark-Server-Token", p.Token)
	resp, err := p.client().Do(req)
	if err != nil {
		return fmt.Errorf("postmark api: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("postmark api: %s %s: HTTP %d: %s", method, path, resp.StatusCode, respBody)
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("postmark api: decode %s %s: %w", method, path, err)
		}
	}
	return nil
}

// postmarkWebhook is the subset of the webhook resource the validation
// reads back: enough to identify and delete stale registrations.
type postmarkWebhook struct {
	ID  int64  `json:"ID"`
	URL string `json:"Url"`
}

// postmarkWebhookRequest is the creation payload. HttpAuth carries the
// generated basic-auth pair Posthorn's /events/postmark requires (FR82);
// Postmark supports it natively, per the LifecycleConfig doc comment.
type postmarkWebhookRequest struct {
	URL           string                  `json:"Url"`
	MessageStream string                  `json:"MessageStream"`
	HTTPAuth      postmarkWebhookAuth     `json:"HttpAuth"`
	Triggers      postmarkWebhookTriggers `json:"Triggers"`
}

type postmarkWebhookAuth struct {
	Username string `json:"Username"`
	Password string `json:"Password"`
}

type postmarkWebhookTriggers struct {
	Delivery      postmarkTrigger `json:"Delivery"`
	Bounce        postmarkTrigger `json:"Bounce"`
	SpamComplaint postmarkTrigger `json:"SpamComplaint"`
}

type postmarkTrigger struct {
	Enabled        bool `json:"Enabled"`
	IncludeContent bool `json:"IncludeContent,omitempty"`
}

// validationTriggers enables exactly the event classes Posthorn ingests
// in v2.0's minimal slice: Delivery (the happy path), Bounce (the
// suppression trigger), SpamComplaint (the other suppression trigger).
func validationTriggers() postmarkWebhookTriggers {
	return postmarkWebhookTriggers{
		Delivery:      postmarkTrigger{Enabled: true},
		Bounce:        postmarkTrigger{Enabled: true},
		SpamComplaint: postmarkTrigger{Enabled: true},
	}
}

// ListWebhooks returns the server's registered webhooks (outbound stream).
func (p *postmarkServerAPI) ListWebhooks(ctx context.Context) ([]postmarkWebhook, error) {
	var out struct {
		Webhooks []postmarkWebhook `json:"Webhooks"`
	}
	if err := p.do(ctx, http.MethodGet, "/webhooks?MessageStream=outbound", nil, &out); err != nil {
		return nil, err
	}
	return out.Webhooks, nil
}

// CreateWebhook registers a webhook and returns its ID for teardown.
func (p *postmarkServerAPI) CreateWebhook(ctx context.Context, req postmarkWebhookRequest) (int64, error) {
	var out struct {
		ID int64 `json:"ID"`
	}
	if err := p.do(ctx, http.MethodPost, "/webhooks", req, &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

// DeleteWebhook removes a webhook registration by ID.
func (p *postmarkServerAPI) DeleteWebhook(ctx context.Context, id int64) error {
	return p.do(ctx, http.MethodDelete, fmt.Sprintf("/webhooks/%d", id), nil, nil)
}

// DeleteSuppressions removes addresses from the stream's provider-side
// suppression list. Postmark caps requests at 50 addresses; the
// validation only ever sends one (the blackhole address), so no batching.
func (p *postmarkServerAPI) DeleteSuppressions(ctx context.Context, stream string, emails []string) error {
	type entry struct {
		EmailAddress string `json:"EmailAddress"`
	}
	body := struct {
		Suppressions []entry `json:"Suppressions"`
	}{}
	for _, e := range emails {
		body.Suppressions = append(body.Suppressions, entry{EmailAddress: e})
	}
	return p.do(ctx, http.MethodPost, "/message-streams/"+stream+"/suppressions/delete", body, nil)
}

// quickTunnelURLPattern matches the ephemeral URL cloudflared prints when
// establishing an anonymous quick tunnel (no account, no credentials).
var quickTunnelURLPattern = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

// parseQuickTunnelURL extracts the quick-tunnel URL from a line of
// cloudflared output. cloudflared prints it inside a decorated banner on
// stderr; scanning line-by-line with this is resilient to the framing.
func parseQuickTunnelURL(line string) (string, bool) {
	m := quickTunnelURLPattern.FindString(line)
	return m, m != ""
}

// verifyEventSignature checks an X-Posthorn-Signature header value
// ("sha256=<hex>", FR84) against the body using the shared secret,
// recomputed independently of core/lifecycle.
func verifyEventSignature(secret string, body []byte, header string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(header))
}

// receivedEvent is one forwarded lifecycle callback as seen by the
// validation's receiver: the normalized fields the assertions need, the
// raw body, and whether the HMAC verified.
type receivedEvent struct {
	Event        string `json:"event"`
	SubmissionID string `json:"submission_id"`
	Endpoint     string `json:"endpoint"`
	Recipient    string `json:"recipient"`
	SignatureOK  bool   `json:"-"`
	Raw          []byte `json:"-"`
}

// eventReceiver is the stand-in for an operator's app: it accepts
// Posthorn's forwarded lifecycle callbacks, verifies the signature, and
// hands each event to the test over a channel. Always answers 200 so
// Posthorn's forwarder never retries against it.
type eventReceiver struct {
	secret string
	Events chan receivedEvent

	mu  sync.Mutex
	all []receivedEvent
}

func newEventReceiver(secret string) *eventReceiver {
	return &eventReceiver{secret: secret, Events: make(chan receivedEvent, 64)}
}

func (r *eventReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	if err != nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	var ev receivedEvent
	_ = json.Unmarshal(body, &ev)
	ev.Raw = body
	ev.SignatureOK = verifyEventSignature(r.secret, body, req.Header.Get("X-Posthorn-Signature"))
	r.mu.Lock()
	r.all = append(r.all, ev)
	r.mu.Unlock()
	select {
	case r.Events <- ev:
	default: // never block Posthorn's forwarder on a full channel
	}
	w.WriteHeader(http.StatusOK)
}

// lifecycleValidationOpts parameterizes the generated validation config.
type lifecycleValidationOpts struct {
	DBPath        string // SQLite file inside the run's temp dir
	BasicAuthUser string // generated per run
	BasicAuthPass string // generated per run
	From          string // verified Postmark sender signature
	DeliveryTo    string // real mailbox that accepts mail (Delivery events only fire on acceptance)
	WebhookURL    string // the local eventReceiver
	WebhookSecret string // generated per run
}

// lifecycleValidationConfig renders the TOML config the validation runs
// Posthorn under: storage + lifecycle enabled, one delivery-leg endpoint
// and one bounce-leg endpoint (Postmark's blackhole address), both
// forwarding events to the receiver. The Postmark token is referenced as
// an ${env...} placeholder so it never touches disk (NFR3 discipline);
// the posthorn process inherits it from the test environment.
func lifecycleValidationConfig(o lifecycleValidationOpts) string {
	return fmt.Sprintf(`# Generated by the Story 18.3 lifecycle validation run. Ephemeral.

[storage]
path = %q

[lifecycle]
basic_auth_username = %q
basic_auth_password = %q

[[endpoints]]
path = "/validate/delivery"
to = [%q]
from = %q
subject = "Posthorn lifecycle validation (delivery leg)"
body = "Story 18.3 delivery-leg send. run_id: {{.run_id}}"
required = ["run_id"]
webhook_url = %q
webhook_secret = %q

[endpoints.transport]
type = "postmark"

[endpoints.transport.settings]
api_key = "${env.POSTMARK_SERVER_TOKEN}"

[[endpoints]]
path = "/validate/bounce"
to = [%q]
from = %q
subject = "Posthorn lifecycle validation (bounce leg)"
body = "Story 18.3 bounce-leg send. run_id: {{.run_id}}"
required = ["run_id"]
webhook_url = %q
webhook_secret = %q

[endpoints.transport]
type = "postmark"

[endpoints.transport.settings]
api_key = "${env.POSTMARK_SERVER_TOKEN}"
`,
		o.DBPath,
		o.BasicAuthUser, o.BasicAuthPass,
		o.DeliveryTo, o.From, o.WebhookURL, o.WebhookSecret,
		postmarkHardBounceAddress, o.From, o.WebhookURL, o.WebhookSecret,
	)
}
