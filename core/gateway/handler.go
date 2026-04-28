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
	"strings"

	"github.com/craigmccaskill/posthorn/config"
	"github.com/craigmccaskill/posthorn/response"
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
	cfg        config.EndpointConfig
	transport  transport.Transport
	emailField string // resolved at construction (cfg.EmailField or default)
}

// Option configures a Handler at construction time. Reserved for future
// stories (rate limiter, templates, logger) that need optional dependencies.
// v1.0 keeps the surface minimal.
type Option func(*Handler)

// New constructs a Handler from a parsed endpoint config and a transport.
//
// Returns an error if the transport is nil. The caller is expected to have
// validated the config (e.g., via [config.Config.Validate]); New does not
// re-validate the config but does check the explicit dependencies.
func New(cfg config.EndpointConfig, t transport.Transport, opts ...Option) (*Handler, error) {
	if t == nil {
		return nil, errors.New("gateway: transport is nil")
	}
	emailField := cfg.EmailField
	if emailField == "" {
		emailField = defaultEmailField
	}
	h := &Handler{
		cfg:        cfg,
		transport:  t,
		emailField: emailField,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// ServeHTTP implements [net/http.Handler].
//
// Pipeline order (current implementation; subsequent stories insert checks
// at the appropriate points per architecture doc §"Request flow"):
//
//  1. Method check     → POST only          → 405
//  2. Content-type     → form-encoded only  → 400
//  3. Parse form       → r.ParseForm        → 400
//  4. Required fields  → all required present and non-empty → 422
//  5. Email format     → submitter email well-formed → 422
//  6. transport.Send   → upstream API       → 200 or 502
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !isFormContentType(r.Header.Get("Content-Type")) {
		http.Error(w, "form-encoded body required (application/x-www-form-urlencoded or multipart/form-data)", http.StatusBadRequest)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, fmt.Sprintf("parse form: %v", err), http.StatusBadRequest)
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

	// Build the Message. Subject and Body are literal config values for now;
	// Go template rendering with form-field interpolation lands in Story 2.4.
	msg := transport.Message{
		From:     h.cfg.From,
		To:       h.cfg.To,
		Subject:  h.cfg.Subject,
		BodyText: h.cfg.Body,
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
