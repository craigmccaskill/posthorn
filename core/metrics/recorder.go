package metrics

import "time"

// LatencyBuckets is the histogram bucket set Posthorn uses for send
// latency. Covers sub-millisecond to 10s in a power-of-roughly-5 spacing
// that maps cleanly to the operator-meaningful regions (idempotent
// replay → ~ms; happy-path Postmark → ~300ms; retry → ~1s; near-timeout
// → ~5s; hard-timeout boundary → 10s).
var LatencyBuckets = []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10}

// Recorder is the typed entry point Posthorn's gateway and cmd/posthorn
// call to record metrics. Each method names the event semantically; the
// concrete metric storage (counters/histograms) is hidden so future
// metrics additions don't require gateway-side code changes.
//
// A nil *Recorder is a no-op — gateway code can call Recorder methods
// without an enabling-condition check; tests that don't care about
// metrics pass nil.
type Recorder struct {
	submitted      *Counter
	sent           *Counter
	failed         *Counter
	rateLimited    *Counter
	authFailed     *Counter
	spamBlocked    *Counter
	validationFailed *Counter
	idempotentReplay *Counter
	sendLatency    *Histogram
}

// NewRecorder constructs a Recorder backed by counters and histograms
// registered with reg. Call once per Posthorn process.
func NewRecorder(reg *Registry) *Recorder {
	r := &Recorder{
		submitted: NewCounter(
			"posthorn_submissions_received_total",
			"Submissions accepted past the body-parse stage, before transport.Send.",
			[]string{"endpoint"},
		),
		sent: NewCounter(
			"posthorn_submissions_sent_total",
			"Submissions successfully delivered to the upstream transport.",
			[]string{"endpoint", "transport"},
		),
		failed: NewCounter(
			"posthorn_submissions_failed_total",
			"Submissions that reached transport.Send but failed terminally (no retry, 502 to client).",
			[]string{"endpoint", "transport", "error_class"},
		),
		rateLimited: NewCounter(
			"posthorn_rate_limited_total",
			"Submissions rejected at the rate-limit gate (HTTP 429).",
			[]string{"endpoint"},
		),
		authFailed: NewCounter(
			"posthorn_auth_failed_total",
			"API-mode submissions rejected with HTTP 401 (missing or invalid Bearer token).",
			[]string{"endpoint"},
		),
		spamBlocked: NewCounter(
			"posthorn_spam_blocked_total",
			"Form-mode submissions silent-rejected by spam defenses (honeypot, origin).",
			[]string{"endpoint", "kind"},
		),
		validationFailed: NewCounter(
			"posthorn_validation_failed_total",
			"Submissions rejected with HTTP 422 for missing required fields or malformed email.",
			[]string{"endpoint"},
		),
		idempotentReplay: NewCounter(
			"posthorn_idempotent_replay_total",
			"API-mode requests served from the idempotency cache without re-sending.",
			[]string{"endpoint"},
		),
		sendLatency: NewHistogram(
			"posthorn_send_latency_seconds",
			"Wall-clock latency of transport.Send, including any retries.",
			LatencyBuckets,
			[]string{"endpoint", "transport"},
		),
	}
	reg.Register(r.submitted)
	reg.Register(r.sent)
	reg.Register(r.failed)
	reg.Register(r.rateLimited)
	reg.Register(r.authFailed)
	reg.Register(r.spamBlocked)
	reg.Register(r.validationFailed)
	reg.Register(r.idempotentReplay)
	reg.Register(r.sendLatency)
	return r
}

// Submitted records a submission that passed body parse + validation
// and is about to enter the transport. Called once per request before
// transport.Send.
func (r *Recorder) Submitted(endpoint string) {
	if r == nil {
		return
	}
	r.submitted.Inc(endpoint)
}

// Sent records a successful upstream delivery and observes the latency.
func (r *Recorder) Sent(endpoint, transport string, latency time.Duration) {
	if r == nil {
		return
	}
	r.sent.Inc(endpoint, transport)
	r.sendLatency.Observe(latency.Seconds(), endpoint, transport)
}

// Failed records a terminal transport failure (no retry, 502 to client).
// errorClass is the ErrorClass.String() value ("transient" / "rate_limited"
// / "terminal" / "unknown").
func (r *Recorder) Failed(endpoint, transport, errorClass string) {
	if r == nil {
		return
	}
	r.failed.Inc(endpoint, transport, errorClass)
}

// RateLimited records a 429 response at the rate-limit gate.
func (r *Recorder) RateLimited(endpoint string) {
	if r == nil {
		return
	}
	r.rateLimited.Inc(endpoint)
}

// AuthFailed records a 401 response from the API-mode auth check.
func (r *Recorder) AuthFailed(endpoint string) {
	if r == nil {
		return
	}
	r.authFailed.Inc(endpoint)
}

// SpamBlocked records a silent-200 spam rejection (honeypot or origin
// check). kind is "honeypot" or "origin".
func (r *Recorder) SpamBlocked(endpoint, kind string) {
	if r == nil {
		return
	}
	r.spamBlocked.Inc(endpoint, kind)
}

// ValidationFailed records a 422 response for required-fields or
// email-format failure.
func (r *Recorder) ValidationFailed(endpoint string) {
	if r == nil {
		return
	}
	r.validationFailed.Inc(endpoint)
}

// IdempotentReplay records a cache-hit replay (no transport send).
func (r *Recorder) IdempotentReplay(endpoint string) {
	if r == nil {
		return
	}
	r.idempotentReplay.Inc(endpoint)
}
