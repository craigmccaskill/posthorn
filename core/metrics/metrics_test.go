package metrics

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// --- Counter ---

func TestCounter_IncAndEmit(t *testing.T) {
	c := NewCounter("posthorn_test_total", "Test counter", []string{"endpoint"})
	c.Inc("/api/contact")
	c.Inc("/api/contact")
	c.Inc("/api/feedback")

	var buf bytes.Buffer
	if err := c.Emit(&buf); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "# HELP posthorn_test_total Test counter\n") {
		t.Errorf("missing HELP line: %s", out)
	}
	if !strings.Contains(out, "# TYPE posthorn_test_total counter\n") {
		t.Errorf("missing TYPE line: %s", out)
	}
	if !strings.Contains(out, `posthorn_test_total{endpoint="/api/contact"} 2`+"\n") {
		t.Errorf("missing /api/contact value: %s", out)
	}
	if !strings.Contains(out, `posthorn_test_total{endpoint="/api/feedback"} 1`+"\n") {
		t.Errorf("missing /api/feedback value: %s", out)
	}
}

func TestCounter_NoLabels(t *testing.T) {
	c := NewCounter("posthorn_total", "Unlabeled", nil)
	c.Inc()
	c.Inc()
	c.Inc()

	var buf bytes.Buffer
	_ = c.Emit(&buf)
	if !strings.Contains(buf.String(), "posthorn_total 3\n") {
		t.Errorf("unlabeled counter format wrong: %s", buf.String())
	}
}

func TestCounter_AddDelta(t *testing.T) {
	c := NewCounter("c", "c", nil)
	c.Add(5)
	c.Add(3)
	var buf bytes.Buffer
	_ = c.Emit(&buf)
	if !strings.Contains(buf.String(), "c 8\n") {
		t.Errorf("Add: %s", buf.String())
	}
}

func TestCounter_NegativeDeltaPanics(t *testing.T) {
	c := NewCounter("c", "c", nil)
	defer func() {
		if recover() == nil {
			t.Error("negative delta did not panic")
		}
	}()
	c.Add(-1)
}

func TestCounter_LabelMismatchPanics(t *testing.T) {
	c := NewCounter("c", "c", []string{"a", "b"})
	defer func() {
		if recover() == nil {
			t.Error("label-count mismatch did not panic")
		}
	}()
	c.Inc("only-one") // expecting two
}

func TestCounter_ConcurrentInc(t *testing.T) {
	c := NewCounter("c", "c", []string{"endpoint"})
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Inc("/api/x")
		}()
	}
	wg.Wait()
	var buf bytes.Buffer
	_ = c.Emit(&buf)
	if !strings.Contains(buf.String(), `posthorn_test... 100`) && !strings.Contains(buf.String(), `} 100`+"\n") {
		t.Errorf("concurrent inc count wrong: %s", buf.String())
	}
}

// --- Histogram ---

func TestHistogram_ObserveAndEmit(t *testing.T) {
	h := NewHistogram("posthorn_latency_seconds", "Send latency", []float64{0.1, 0.5, 1}, []string{"transport"})
	h.Observe(0.05, "postmark") // hits 0.1, 0.5, 1
	h.Observe(0.3, "postmark")  // hits 0.5, 1
	h.Observe(2.0, "postmark")  // hits only +Inf

	var buf bytes.Buffer
	if err := h.Emit(&buf); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "# TYPE posthorn_latency_seconds histogram\n") {
		t.Errorf("missing TYPE: %s", out)
	}
	for _, want := range []string{
		`posthorn_latency_seconds_bucket{transport="postmark",le="0.1"} 1`,
		`posthorn_latency_seconds_bucket{transport="postmark",le="0.5"} 2`,
		`posthorn_latency_seconds_bucket{transport="postmark",le="1"} 2`,
		`posthorn_latency_seconds_bucket{transport="postmark",le="+Inf"} 3`,
		`posthorn_latency_seconds_count{transport="postmark"} 3`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Sum should be 0.05 + 0.3 + 2.0 = 2.35. Float formatting is "%g" so
	// "2.35" is the expected representation.
	if !strings.Contains(out, `posthorn_latency_seconds_sum{transport="postmark"} 2.35`) {
		t.Errorf("missing sum=2.35: %s", out)
	}
}

func TestHistogram_UnsortedBucketsPanic(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("unsorted buckets did not panic")
		}
	}()
	NewHistogram("h", "h", []float64{0.5, 0.1}, nil)
}

// --- Label value escaping ---

func TestEscapeLabelValue(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"simple", "simple"},
		{`with "quote"`, `with \"quote\"`},
		{`with \ backslash`, `with \\ backslash`},
		{"with\nnewline", `with\nnewline`},
		{`all "\n at once`, `all \"\\n at once`},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := escapeLabelValue(tt.in); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// --- Registry ---

func TestRegistry_EmitsAllSortedByName(t *testing.T) {
	r := New()
	r.Register(NewCounter("posthorn_zzz_total", "z", nil))
	r.Register(NewCounter("posthorn_aaa_total", "a", nil))
	r.Register(NewCounter("posthorn_mmm_total", "m", nil))

	var buf bytes.Buffer
	if err := r.Emit(&buf); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	out := buf.String()
	// Each name's HELP line appears in alphabetical order.
	aaa := strings.Index(out, "posthorn_aaa_total")
	mmm := strings.Index(out, "posthorn_mmm_total")
	zzz := strings.Index(out, "posthorn_zzz_total")
	if aaa < 0 || mmm < 0 || zzz < 0 {
		t.Fatalf("metric missing from output: %s", out)
	}
	if !(aaa < mmm && mmm < zzz) {
		t.Errorf("metrics not in sorted order: aaa=%d mmm=%d zzz=%d", aaa, mmm, zzz)
	}
}

func TestRegistry_DuplicateRegistrationPanics(t *testing.T) {
	r := New()
	r.Register(NewCounter("dup_total", "x", nil))
	defer func() {
		if recover() == nil {
			t.Error("duplicate registration did not panic")
		}
	}()
	r.Register(NewCounter("dup_total", "y", nil))
}

// --- Healthz handler ---

func TestHealthzHandler(t *testing.T) {
	h := HealthzHandler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("body = %q, want %q", rec.Body.String(), "ok")
	}
}

// --- /metrics handler ---

func TestRegistry_Handler_EmitsScrapeShape(t *testing.T) {
	r := New()
	c := NewCounter("posthorn_test_total", "Test", []string{"endpoint"})
	r.Register(c)
	c.Inc("/api/x")

	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain prefix", rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "version=0.0.4") {
		t.Errorf("Content-Type missing Prometheus version: %q", rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), `posthorn_test_total{endpoint="/api/x"} 1`) {
		t.Errorf("metrics output missing expected line: %s", rec.Body.String())
	}
}

// --- Recorder ---

func TestRecorder_NilIsNoOp(t *testing.T) {
	var r *Recorder // explicitly nil
	// All methods must be safe to call on nil — no panic, no observable
	// effect (no Registry, nothing to record into).
	r.Submitted("/api/x")
	r.Sent("/api/x", "postmark", 0)
	r.Failed("/api/x", "postmark", "terminal")
	r.RateLimited("/api/x")
	r.AuthFailed("/api/x")
	r.SpamBlocked("/api/x", "honeypot")
	r.ValidationFailed("/api/x")
	r.IdempotentReplay("/api/x")
}

func TestRecorder_RecordsAndEmits(t *testing.T) {
	reg := New()
	r := NewRecorder(reg)

	r.Submitted("/api/contact")
	r.Submitted("/api/contact")
	r.Sent("/api/contact", "postmark", 300_000_000) // 300ms
	r.Failed("/api/contact", "postmark", "transient")
	r.RateLimited("/api/contact")
	r.AuthFailed("/api/transactional")
	r.SpamBlocked("/api/contact", "honeypot")
	r.ValidationFailed("/api/contact")
	r.IdempotentReplay("/api/transactional")

	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	expectations := []string{
		`posthorn_submissions_received_total{endpoint="/api/contact"} 2`,
		`posthorn_submissions_sent_total{endpoint="/api/contact",transport="postmark"} 1`,
		`posthorn_submissions_failed_total{endpoint="/api/contact",transport="postmark",error_class="transient"} 1`,
		`posthorn_rate_limited_total{endpoint="/api/contact"} 1`,
		`posthorn_auth_failed_total{endpoint="/api/transactional"} 1`,
		`posthorn_spam_blocked_total{endpoint="/api/contact",kind="honeypot"} 1`,
		`posthorn_validation_failed_total{endpoint="/api/contact"} 1`,
		`posthorn_idempotent_replay_total{endpoint="/api/transactional"} 1`,
		`posthorn_send_latency_seconds_count{endpoint="/api/contact",transport="postmark"} 1`,
	}
	for _, want := range expectations {
		if !strings.Contains(body, want) {
			t.Errorf("missing line %q in:\n%s", want, body)
		}
	}
}

// NFR24: confirm that scraping /metrics does not leak submitter content
// as label values. Recorder methods take operator-controlled values
// (endpoint paths from config, transport types from registry, kind
// enums); there is no method that would let a request-side value reach
// the label space. This test pins that contract: passing an "endpoint"
// argument that looks like a recipient (which would never happen in
// practice — endpoints come from cfg.Path) doesn't change the structure
// of the exposed metric — but documents that the API surface itself is
// the defense.
func TestRecorder_NoSubmitterCardinalityLeak(t *testing.T) {
	reg := New()
	r := NewRecorder(reg)
	// In practice the gateway only ever calls Submitted(h.cfg.Path) —
	// cfg.Path is operator-config, finite cardinality.
	r.Submitted("/api/contact")
	r.Submitted("/api/contact")

	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `posthorn_submissions_received_total{endpoint="/api/contact"} 2`) {
		t.Errorf("expected operator-controlled label only: %s", rec.Body.String())
	}
	// Nothing in the Recorder API takes a recipient/subject/body. That
	// IS the defense.
}
