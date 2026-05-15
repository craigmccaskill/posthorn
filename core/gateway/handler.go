// Package gateway holds the HTTP form-ingress handler for Posthorn.
//
// One Handler instance is constructed per configured endpoint. Multiple
// endpoints get multiple Handlers, each independent (FR2). The Handler
// implements [net/http.Handler] and is path-unaware: it assumes the caller
// has already routed the request to the right endpoint. The standalone
// binary uses [net/http.ServeMux] for routing; the Caddy adapter checks
// the path in its module wrapper.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/craigmccaskill/posthorn/config"
	"github.com/craigmccaskill/posthorn/log"
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

// Retry timing — declared as vars so tests can override without
// waiting full real-time delays. Production never mutates these.
var (
	// requestTimeout is the hard upper bound on a single submission's
	// processing time, including any retries (FR22).
	requestTimeout = 10 * time.Second

	// transientRetryDelay is how long the handler waits before retrying
	// a transient transport failure (FR19).
	transientRetryDelay = 1 * time.Second

	// rateLimitedRetryDelay is how long the handler waits before retrying
	// a 429 from the upstream provider (FR20). Longer than transient
	// because the upstream is asking us to slow down.
	rateLimitedRetryDelay = 5 * time.Second
)

// Handler accepts form submissions and forwards them via a Transport.
//
// Construct via [New]. The struct's fields are unexported because
// post-construction mutation is not supported; tests use the constructor
// and the Option pattern.
type Handler struct {
	cfg                  config.EndpointConfig
	transport            transport.Transport
	renderer             *template.Renderer
	limiter              *ratelimit.Limiter // nil if rate_limit not configured
	trustedProxies       []netip.Prefix
	emailField           string // resolved at construction (cfg.EmailField or default)
	maxBodySize          int64  // 0 = no cap
	logFailedSubmissions bool   // resolved at construction (default true)
	logger               *slog.Logger
}

// Option configures a Handler at construction time.
type Option func(*Handler)

// WithLogger overrides the default (discard) logger. The Caddy adapter
// passes a slog wrapper around Caddy's zap logger; the standalone
// binary passes the logger built from the [logging] config block.
func WithLogger(l *slog.Logger) Option {
	return func(h *Handler) {
		if l != nil {
			h.logger = l
		}
	}
}

// New constructs a Handler from a parsed endpoint config and a transport.
//
// Returns an error if the transport is nil, if the template parser fails,
// if the body template references a missing file, or if rate-limit /
// trusted-proxies / max-body-size config values are malformed.
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

	// log_failed_submissions defaults to true per FR16 (operator can recover
	// the data from logs on terminal failure). *bool distinguishes "operator
	// omitted" from "operator explicitly set false" (ADR-4).
	logFailed := true
	if cfg.LogFailedSubmissions != nil {
		logFailed = *cfg.LogFailedSubmissions
	}

	h := &Handler{
		cfg:                  cfg,
		transport:            t,
		renderer:             renderer,
		limiter:              limiter,
		trustedProxies:       prefixes,
		emailField:           emailField,
		maxBodySize:          maxBody,
		logFailedSubmissions: logFailed,
		logger:               log.Discard(), // overridable via WithLogger
	}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// ServeHTTP implements [net/http.Handler].
//
// Pipeline order (per architecture doc §"Request flow"):
//
//  1. Body size cap    → http.MaxBytesReader (if configured)
//  2. Method check     → POST only          → 405
//  3. Content-type     → form-encoded only  → 400
//  4. Origin check     → fail-closed if allowed_origins set → 403
//  5. Rate limit       → token bucket per IP → 429
//  6. Parse form       → r.ParseForm        → 413/400
//  7. Honeypot check   → silent 200 if filled → 200 (silent)
//  8. Required fields  → all required present and non-empty → 422
//  9. Email format     → submitter email well-formed → 422
// 10. Render subject/body, transport.Send (with retries) → 200 or 502
//
// Submission lifecycle is logged at every decision point with a
// per-request UUID submission_id (NFR8) and the endpoint path (FR15).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	submissionID := log.SubmissionID()
	logger := h.logger.With(
		slog.String("submission_id", submissionID),
		slog.String("endpoint", h.cfg.Path),
		slog.String("transport", h.cfg.Transport.Type),
	)

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
		if result, reason := spam.CheckOrigin(origin, referer, h.cfg.AllowedOrigins); result == spam.HardReject {
			logger.Info("spam_blocked",
				slog.String("kind", "origin"),
				slog.String("reason", reason),
				slog.Int64("latency_ms", time.Since(start).Milliseconds()),
			)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}

	// Rate limit check (FR8, FR9). Per-IP, with proxy-aware extraction.
	if h.limiter != nil {
		clientIP := ratelimit.ClientIP(r, h.trustedProxies)
		if !h.limiter.Allow(clientIP) {
			logger.Info("rate_limited",
				slog.String("client_ip", clientIP),
				slog.Int64("latency_ms", time.Since(start).Milliseconds()),
			)
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
	}

	if err := r.ParseForm(); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			logger.Info("body_too_large",
				slog.Int64("limit_bytes", h.maxBodySize),
				slog.Int64("latency_ms", time.Since(start).Milliseconds()),
			)
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, fmt.Sprintf("parse form: %v", err), http.StatusBadRequest)
		return
	}

	// Honeypot check (FR5) — silent 200 if triggered so bots can't
	// distinguish honeypot rejection from success. The response body
	// mirrors the real success path's shape (status + submission_id)
	// for the same reason: an observant bot inspecting the body must
	// not be able to tell the two paths apart.
	if spam.CheckHoneypot(r.Form, h.cfg.Honeypot) == spam.SilentReject {
		logger.Info("spam_blocked",
			slog.String("kind", "honeypot"),
			slog.Int64("latency_ms", time.Since(start).Milliseconds()),
		)
		response.WriteJSON(w, http.StatusOK, response.Success{
			Status:       "ok",
			SubmissionID: submissionID,
		})
		return
	}

	// Validation: required fields + email format (FR8, FR9).
	missing := validate.RequiredFields(r.Form, h.cfg.Required)
	fieldErrors := map[string]string{}
	if v := strings.TrimSpace(r.Form.Get(h.emailField)); v != "" && !validate.Email(v) {
		fieldErrors[h.emailField] = "invalid email format"
	}
	if len(missing) > 0 || len(fieldErrors) > 0 {
		failedFields := append([]string{}, missing...)
		for f := range fieldErrors {
			failedFields = append(failedFields, f)
		}
		logger.Info("validation_failed",
			slog.Any("fields", failedFields),
			slog.Int64("latency_ms", time.Since(start).Milliseconds()),
		)
		response.WriteJSON(w, http.StatusUnprocessableEntity, response.Validation(missing, fieldErrors))
		return
	}

	logger.Info("submission_received")

	// Render subject and body templates against form fields (FR12, FR13).
	subject, err := h.renderer.RenderSubject(r.Form)
	if err != nil {
		logger.Error("template_render_failed", slog.String("error", err.Error()))
		http.Error(w, "submission could not be processed", http.StatusInternalServerError)
		return
	}
	body, err := h.renderer.RenderBody(r.Form)
	if err != nil {
		logger.Error("template_render_failed", slog.String("error", err.Error()))
		http.Error(w, "submission could not be processed", http.StatusInternalServerError)
		return
	}

	msg := transport.Message{
		From:     h.cfg.From,
		To:       h.cfg.To,
		Subject:  subject,
		BodyText: body,
	}

	// Reply-To header (PRD Open Question 4): when the operator names a
	// form field via reply_to_email_field, or leaves it unset (defaulting
	// to h.emailField), and the submission provides a value that passes
	// the email syntax check, set Reply-To. Otherwise leave it empty so
	// the receiver replies to the `from` address — the safe default.
	replyToField := h.cfg.ReplyToEmailField
	if replyToField == "" {
		replyToField = h.emailField
	}
	if v := strings.TrimSpace(r.Form.Get(replyToField)); v != "" && validate.Email(v) {
		msg.ReplyTo = v
	}

	// Send with retry policy (FR19-22) under the request hard timeout.
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	if err := sendWithRetry(ctx, h.transport, msg, logger); err != nil {
		// Terminal failure — log payload (or just metadata if operator
		// disabled it) so operators can recover from logs (FR16).
		if h.logFailedSubmissions {
			logger.Error("submission_failed",
				slog.String("error", err.Error()),
				slog.Any("form", redactedForm(r.Form, h.cfg.Honeypot)),
				slog.Int64("latency_ms", time.Since(start).Milliseconds()),
			)
		} else {
			logger.Error("submission_failed",
				slog.String("error", err.Error()),
				slog.Any("form_fields", formFieldNames(r.Form)),
				slog.Int64("latency_ms", time.Since(start).Milliseconds()),
			)
		}
		http.Error(w, "submission could not be delivered", http.StatusBadGateway)
		return
	}

	logger.Info("submission_sent",
		slog.Int64("latency_ms", time.Since(start).Milliseconds()),
	)
	response.WriteJSON(w, http.StatusOK, response.Success{
		Status:       "ok",
		SubmissionID: submissionID,
	})
}

// sendWithRetry implements the FR19-22 retry policy:
//
//   - On *transport.TransportError with class ErrTransient: wait 1s, retry once.
//   - On *transport.TransportError with class ErrRateLimited: wait 5s, retry once.
//   - On any class ErrTerminal (or non-TransportError): no retry.
//   - The provided ctx carries the 10s request hard timeout (FR22);
//     if it expires during the backoff, the second attempt is skipped.
//
// Returns nil on success or the most recent TransportError.
func sendWithRetry(ctx context.Context, t transport.Transport, msg transport.Message, logger *slog.Logger) error {
	err := t.Send(ctx, msg)
	if err == nil {
		return nil
	}

	var te *transport.TransportError
	if !errors.As(err, &te) {
		// Non-TransportError — caller contract violation; treat as terminal.
		return err
	}

	var delay time.Duration
	switch te.Class {
	case transport.ErrTransient:
		delay = transientRetryDelay
	case transport.ErrRateLimited:
		delay = rateLimitedRetryDelay
	default:
		// ErrTerminal, ErrUnknown — no retry.
		return err
	}

	logger.Info("send_retry_scheduled",
		slog.String("class", te.Class.String()),
		slog.Int("status", te.Status),
		slog.Duration("delay", delay),
	)

	select {
	case <-ctx.Done():
		// Hit the 10s hard timeout before we could retry. Surface the
		// original error.
		return err
	case <-time.After(delay):
	}

	retryErr := t.Send(ctx, msg)
	if retryErr == nil {
		logger.Info("send_retry_succeeded")
		return nil
	}
	logger.Info("send_retry_failed",
		slog.String("error", retryErr.Error()),
	)
	return retryErr
}

// redactedForm returns a copy of the form with the honeypot field
// redacted so terminal-failure logs don't immortalize spam payloads
// in operator logs. Non-honeypot fields pass through verbatim — the
// whole point of FR16 is letting the operator recover the submission
// from logs.
func redactedForm(form map[string][]string, honeypot string) map[string][]string {
	if len(form) == 0 {
		return nil
	}
	out := make(map[string][]string, len(form))
	for k, v := range form {
		if k == honeypot && honeypot != "" {
			out[k] = []string{"<redacted>"}
			continue
		}
		out[k] = v
	}
	return out
}

// formFieldNames returns just the keys of a form. Used when
// log_failed_submissions=false — operator wants to know SOMETHING
// happened but not retain submitter content.
func formFieldNames(form map[string][]string) []string {
	if len(form) == 0 {
		return nil
	}
	names := make([]string, 0, len(form))
	for k := range form {
		names = append(names, k)
	}
	return names
}

// isFormContentType returns true if the Content-Type header indicates a
// form-encoded body. Both application/x-www-form-urlencoded and
// multipart/form-data are accepted (FR1).
func isFormContentType(ct string) bool {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(strings.ToLower(ct))
	return ct == "application/x-www-form-urlencoded" || ct == "multipart/form-data"
}
