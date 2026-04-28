// Package gateway holds the HTTP form-ingress handler for Posthorn.
//
// One Handler instance is constructed per configured endpoint. Multiple
// endpoints get multiple Handlers, each independent (FR2). The Handler
// implements [net/http.Handler] and is path-unaware: it assumes the caller
// has already routed the request to the right endpoint. The standalone
// binary uses [net/http.ServeMux] for routing; the Caddy adapter checks
// the path in its module wrapper.
//
// The Handler is built up across multiple stories. Story 2.2 established
// the request lifecycle (method check → content-type check → form parse →
// transport send → response). Story 2.3 added validation. Subsequent
// stories layer in spam protection, rate limiting, templating, retry
// policy, and structured logging without changing the public API.
package gateway

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"

	"github.com/craigmccaskill/posthorn/config"
	"github.com/craigmccaskill/posthorn/ratelimit"
	"github.com/craigmccaskill/posthorn/response"
	"github.com/craigmccaskill/posthorn/spam"
	"github.com/craigmccaskill/posthorn/template"
	"github.com/craigmccaskill/posthorn/transport"
	"github.com/craigmccaskill/posthorn/validate"
)

// defaultEmailField is the form field name searched for the submitter's
// email when EndpointConfig.EmailField is unset.
const defaultEmailField = "email"

// Handler accepts form submissions and forwards them via a Transport.
//
// Construct via [New]. The struct's fields are unexported because
// post-construction mutation is not supported; tests use the constructor
// and the Option pattern.
type Handler struct {
	cfg            config.EndpointConfig
	transport      transport.Transport
	renderer       *template.Renderer
	limiter        *ratelimit.Limiter // nil if rate_limit not configured
	trustedProxies []netip.Prefix
	emailField     string // resolved at construction (cfg.EmailField or default)
	maxBodySize    int64  // 0 = no cap
}

// Option configures a Handler at construction time. Reserved for future
// stories (rate limiter, templates, logger) that need optional dependencies.
// v1.0 keeps the surface minimal.
type Option func(*Handler)

// New constructs a Handler from a parsed endpoint config and a transport.
//
// Returns an error if the transport is nil, if the template parser fails,
// or if the body template references a missing file. The caller is
// expected to have validated the config (e.g., via [config.Config.Validate])
// for structural correctness; New surfaces template-specific failures here.
func New(cfg config.EndpointConfig, t transport.Transport, opts ...Option) (*Handler, error) {
	if t == nil {
		return nil, errors.New("gateway: transport is nil")
	}
	emailField := cfg.EmailField
	if emailField == "" {
		emailField = defaultEmailField
	}

	// Build the reserved-names set for the template renderer. These fields
	// are intentionally excluded from the custom-fields passthrough block
	// in the rendered body — operators have already accounted for them in
	// their config (FR13).
	reserved := make([]string, 0, len(cfg.Required)+2)
	reserved = append(reserved, cfg.Required...)
	reserved = append(reserved, emailField)
	if cfg.Honeypot != "" {
		reserved = append(reserved, cfg.Honeypot)
	}

	renderer, err := template.NewRenderer(cfg.Subject, cfg.Body, reserved)
	if err != nil {
		return nil, fmt.Errorf("gateway: %w", err)
	}

	maxBody, err := spam.ParseSize(cfg.MaxBodySize)
	if err != nil {
		return nil, fmt.Errorf("gateway: max_body_size: %w", err)
	}

	prefixes, err := ratelimit.ParsePrefixes(cfg.TrustedProxies)
	if err != nil {
		return nil, fmt.Errorf("gateway: trusted_proxies: %w", err)
	}

	var limiter *ratelimit.Limiter
	if cfg.RateLimit != nil {
		limiter, err = ratelimit.New(cfg.RateLimit.Count, cfg.RateLimit.Interval.Std(), 0)
		if err != nil {
			return nil, fmt.Errorf("gateway: rate_limit: %w", err)
		}
	}

	h := &Handler{
		cfg:            cfg,
		transport:      t,
		renderer:       renderer,
		limiter:        limiter,
		trustedProxies: prefixes,
		emailField:     emailField,
		maxBodySize:    maxBody,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// ServeHTTP implements [net/http.Handler].
//
// Pipeline order (current implementation; rate limit slots in at step 5
// in Story 3.2):
//
//  1. Body size cap    → http.MaxBytesReader (if configured)
//  2. Method check     → POST only          → 405
//  3. Content-type     → form-encoded only  → 400
//  4. Origin check     → fail-closed if allowed_origins set → 403
//  5. (Rate limit)     → token bucket per IP → 429 (Story 3.2)
//  6. Parse form       → r.ParseForm        → 413/400
//  7. Honeypot check   → silent 200 if filled → 200 (silent)
//  8. Required fields  → all required present and non-empty → 422
//  9. Email format     → submitter email well-formed → 422
// 10. Render subject/body, transport.Send → 200 or 502
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Body size cap (FR7) — wrapped before any read so even malicious
	// chunked-encoding senders can't exceed the cap. The actual rejection
	// fires when ParseForm tries to read past the limit.
	if h.maxBodySize > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, h.maxBodySize)
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !isFormContentType(r.Header.Get("Content-Type")) {
		http.Error(w, "form-encoded body required (application/x-www-form-urlencoded or multipart/form-data)", http.StatusBadRequest)
		return
	}

	// Origin/Referer check (FR6, NFR4). Runs before ParseForm because
	// it doesn't need the body — saves CPU on direct-POST bots.
	if len(h.cfg.AllowedOrigins) > 0 {
		origin, referer := spam.ExtractOriginAndReferer(r)
		if result, _ := spam.CheckOrigin(origin, referer, h.cfg.AllowedOrigins); result == spam.HardReject {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}

	// Rate limit check (FR8, FR9). Per-IP, with proxy-aware extraction.
	// Runs before ParseForm because it's O(1) and prevents body-parse
	// CPU exhaustion attacks.
	if h.limiter != nil {
		clientIP := ratelimit.ClientIP(r, h.trustedProxies)
		if !h.limiter.Allow(clientIP) {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
	}

	if err := r.ParseForm(); err != nil {
		// Distinguish body-size-limit errors from other parse failures so
		// operators get the right status code (FR7 → 413).
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, fmt.Sprintf("parse form: %v", err), http.StatusBadRequest)
		return
	}

	// Honeypot check (FR5) — silent 200 if triggered so bots can't
	// distinguish honeypot rejection from success.
	if spam.CheckHoneypot(r.Form, h.cfg.Honeypot) == spam.SilentReject {
		response.WriteJSON(w, http.StatusOK, response.Success{})
		return
	}

	// Required-field check. Operators see all missing fields at once
	// rather than fix-and-retry. (FR8)
	missing := validate.RequiredFields(r.Form, h.cfg.Required)

	// Email format check. Only inspects the field if it's present and
	// non-empty; missing-and-required would have been caught above. (FR9)
	fieldErrors := map[string]string{}
	if v := strings.TrimSpace(r.Form.Get(h.emailField)); v != "" && !validate.Email(v) {
		fieldErrors[h.emailField] = "invalid email format"
	}

	if len(missing) > 0 || len(fieldErrors) > 0 {
		response.WriteJSON(w, http.StatusUnprocessableEntity, response.Validation(missing, fieldErrors))
		return
	}

	// Render subject and body templates against form fields (FR12, FR13).
	// Custom-fields passthrough block is appended automatically inside
	// renderer.RenderBody for any form keys not named in templates or
	// reserved (required + email + honeypot).
	subject, err := h.renderer.RenderSubject(r.Form)
	if err != nil {
		// Template execution errors at request time are extremely rare with
		// missingkey=zero (the only common runtime error path is method
		// calls on form values, which we don't expose). Still: fail-safe.
		http.Error(w, "submission could not be processed", http.StatusInternalServerError)
		return
	}
	body, err := h.renderer.RenderBody(r.Form)
	if err != nil {
		http.Error(w, "submission could not be processed", http.StatusInternalServerError)
		return
	}

	msg := transport.Message{
		From:     h.cfg.From,
		To:       h.cfg.To,
		Subject:  subject,
		BodyText: body,
	}

	// Send. Retry policy (FR19-22) and 10s timeout (FR22) land in Story 4.1.
	if err := h.transport.Send(r.Context(), msg); err != nil {
		// Generic error string per architecture doc Open Q5: don't leak
		// whether the failure was config (4xx upstream) vs runtime (network).
		http.Error(w, "submission could not be delivered", http.StatusBadGateway)
		return
	}

	// JSON response shape (FR14) for success. Content negotiation
	// (FR15: redirect on browser submits) lands in Story 2.5 when the
	// CLI binary wires up redirect URLs end-to-end.
	response.WriteJSON(w, http.StatusOK, response.Success{})
}

// isFormContentType returns true if the Content-Type header indicates a
// form-encoded body. Both application/x-www-form-urlencoded and
// multipart/form-data are accepted (FR1).
//
// The header may carry parameters (e.g., "; boundary=..." for multipart,
// "; charset=utf-8" for urlencoded); these are stripped before comparison.
// Comparison is case-insensitive per RFC 7231.
func isFormContentType(ct string) bool {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(strings.ToLower(ct))
	return ct == "application/x-www-form-urlencoded" || ct == "multipart/form-data"
}
