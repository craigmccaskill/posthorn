// Package gateway holds the HTTP form-ingress handler for Posthorn.
//
// One Handler instance is constructed per configured endpoint. Multiple
// endpoints get multiple Handlers, each independent (FR2). The Handler
// implements [net/http.Handler] and is path-unaware: it assumes the caller
// has already routed the request to the right endpoint. The cmd/posthorn
// binary uses [net/http.ServeMux] for routing.
package gateway

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/craigmccaskill/posthorn/config"
	"github.com/craigmccaskill/posthorn/csrf"
	"github.com/craigmccaskill/posthorn/idempotency"
	"github.com/craigmccaskill/posthorn/log"
	"github.com/craigmccaskill/posthorn/metrics"
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

// toOverrideField is the api-mode JSON body field whose value (a string or
// array of strings) replaces the endpoint's configured recipients for the
// request (FR46, ADR-11). Sender identity (from) is intentionally not
// overridable — see ADR-11 for the safety reasoning.
const toOverrideField = "to_override"

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
	idemCache            *idempotency.Cache // nil on form-mode endpoints
	trustedProxies       []netip.Prefix
	emailField           string // resolved at construction (cfg.EmailField or default)
	maxBodySize          int64  // 0 = no cap
	logFailedSubmissions bool   // resolved at construction (default true)
	csrfTokenTTL         time.Duration // resolved at construction (cfg.CSRFTokenTTL or default)
	logger               *slog.Logger
	recorder             *metrics.Recorder // nil = no-op (default)
}

// defaultCSRFTokenTTL is the resolved value when cfg.CSRFTokenTTL is
// zero (operator didn't set it). One hour matches FR57's stated default.
const defaultCSRFTokenTTL = time.Hour

// Option configures a Handler at construction time.
type Option func(*Handler)

// WithLogger overrides the default (discard) logger. The cmd/posthorn
// binary passes the logger built from the [logging] config block.
func WithLogger(l *slog.Logger) Option {
	return func(h *Handler) {
		if l != nil {
			h.logger = l
		}
	}
}

// WithRecorder configures a metrics.Recorder for observability. Default
// is nil, in which case all Recorder method calls are no-ops (the
// methods are nil-safe). The cmd/posthorn binary constructs the
// Recorder once and shares it across all handlers.
func WithRecorder(r *metrics.Recorder) Option {
	return func(h *Handler) {
		h.recorder = r
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
	reserved := make([]string, 0, len(cfg.Required)+3)
	reserved = append(reserved, cfg.Required...)
	reserved = append(reserved, emailField)
	if cfg.Honeypot != "" {
		reserved = append(reserved, cfg.Honeypot)
	}
	if cfg.Auth == config.AuthAPIKey {
		// FR46: to_override is a structural api-mode field, not template
		// content. Keep it out of the custom-fields passthrough block.
		reserved = append(reserved, toOverrideField)
	}
	if cfg.CSRFSecret != "" {
		// FR57: the CSRF token field is structural, not template content.
		reserved = append(reserved, csrf.TokenField)
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

	// Idempotency cache: one per api-mode endpoint (FR41, ADR-8). Capacity
	// defaults to DefaultCapacity when the operator hasn't set
	// idempotency_cache_size (FR42).
	var idemCache *idempotency.Cache
	if cfg.Auth == config.AuthAPIKey {
		capacity := cfg.IdempotencyCacheSize
		if capacity <= 0 {
			capacity = idempotency.DefaultCapacity
		}
		idemCache, err = idempotency.New(capacity, idempotency.DefaultTTL)
		if err != nil {
			return nil, fmt.Errorf("gateway: idempotency: %w", err)
		}
	}

	// log_failed_submissions defaults to true per FR16 (operator can recover
	// the data from logs on terminal failure). *bool distinguishes "operator
	// omitted" from "operator explicitly set false" (ADR-4).
	logFailed := true
	if cfg.LogFailedSubmissions != nil {
		logFailed = *cfg.LogFailedSubmissions
	}

	csrfTTL := cfg.CSRFTokenTTL.Std()
	if csrfTTL == 0 {
		csrfTTL = defaultCSRFTokenTTL
	}

	h := &Handler{
		cfg:                  cfg,
		transport:            t,
		renderer:             renderer,
		limiter:              limiter,
		idemCache:            idemCache,
		trustedProxies:       prefixes,
		emailField:           emailField,
		maxBodySize:          maxBody,
		logFailedSubmissions: logFailed,
		csrfTokenTTL:         csrfTTL,
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

	apiMode := h.cfg.Auth == config.AuthAPIKey

	// Determine the rate-limit bucket key. Form mode uses client IP (FR9).
	// API mode uses the matched API key (FR35), which requires the auth
	// check to run before the rate-limit gate.
	var rateLimitKey string
	if apiMode {
		// FR34: parse Authorization: Bearer <key>; constant-time match
		// against api_keys list (NFR19). Failed auth: 401 with no key
		// material in logs (NFR21).
		matched, ok := h.authenticateAPIRequest(r)
		if !ok {
			logger.Info("auth_failed",
				slog.Int64("latency_ms", time.Since(start).Milliseconds()),
			)
			h.recorder.AuthFailed(h.cfg.Path)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		rateLimitKey = matched

		// Idempotency check (FR40–FR44). The lookup runs before rate
		// limit so a replay doesn't consume a token from the operator's
		// rate-limit budget — the work is already done.
		if idemKey := r.Header.Get("Idempotency-Key"); idemKey != "" {
			if err := idempotency.ValidateKey(idemKey); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			cached, inFlight := h.idemCache.Lookup(idemKey)
			if cached != nil {
				logger.Info("idempotent_replay",
					slog.String("idempotency_key", idemKey),
					slog.Int("replayed_status", cached.Status),
					slog.Int64("latency_ms", time.Since(start).Milliseconds()),
				)
				h.recorder.IdempotentReplay(h.cfg.Path)
				replayCachedResponse(w, cached)
				return
			}
			if inFlight {
				logger.Info("idempotent_conflict",
					slog.String("idempotency_key", idemKey),
					slog.Int64("latency_ms", time.Since(start).Milliseconds()),
				)
				http.Error(w, "duplicate request in flight for this Idempotency-Key", http.StatusConflict)
				return
			}
			if !h.idemCache.ClaimInFlight(idemKey) {
				// Race lost between Lookup and ClaimInFlight — another
				// goroutine claimed in between. Treat as in-flight (FR44).
				logger.Info("idempotent_conflict",
					slog.String("idempotency_key", idemKey),
					slog.Int64("latency_ms", time.Since(start).Milliseconds()),
				)
				http.Error(w, "duplicate request in flight for this Idempotency-Key", http.StatusConflict)
				return
			}
			// Install a recording wrapper so the deferred Store can replay
			// what we wrote. NFR20: cache the complete original response.
			rw := &recordingResponseWriter{ResponseWriter: w}
			w = rw
			defer h.finalizeIdempotent(idemKey, rw)
		}
	} else {
		// FR1: form-mode endpoints accept form-encoded bodies only.
		if !isFormContentType(r.Header.Get("Content-Type")) {
			http.Error(w, "form-encoded body required (application/x-www-form-urlencoded or multipart/form-data)", http.StatusBadRequest)
			return
		}

		// Origin/Referer check (FR6, NFR4). Form-mode only; API mode is
		// authenticated so browser CORS defenses do not apply.
		if len(h.cfg.AllowedOrigins) > 0 {
			origin, referer := spam.ExtractOriginAndReferer(r)
			if result, reason := spam.CheckOrigin(origin, referer, h.cfg.AllowedOrigins); result == spam.HardReject {
				logger.Info("spam_blocked",
					slog.String("kind", "origin"),
					slog.String("reason", reason),
					slog.Int64("latency_ms", time.Since(start).Milliseconds()),
				)
				h.recorder.SpamBlocked(h.cfg.Path, "origin")
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}

		rateLimitKey = ratelimit.ClientIP(r, h.trustedProxies)
	}

	// Rate limit check (FR8). Bucket key differs by mode (set above).
	if h.limiter != nil {
		if !h.limiter.Allow(rateLimitKey) {
			attrs := []any{slog.Int64("latency_ms", time.Since(start).Milliseconds())}
			if !apiMode && !h.cfg.StripClientIP {
				// NFR21: never log api-key values, even derived/truncated.
				// Form mode logs client_ip for operator forensics — unless
				// strip_client_ip is set (FR59 GDPR option).
				attrs = append(attrs, slog.String("client_ip", rateLimitKey))
			}
			logger.Info("rate_limited", attrs...)
			h.recorder.RateLimited(h.cfg.Path)
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
	}

	// Body parse: form-mode reads form-encoded; api-mode reads JSON.
	if apiMode {
		// FR37: api-mode requires application/json. Form-encoded bodies
		// (or anything else) get 415, not silently accepted.
		if !isJSONContentType(r.Header.Get("Content-Type")) {
			http.Error(w, "JSON body required (application/json)", http.StatusUnsupportedMediaType)
			return
		}
		values, err := parseJSONBody(r.Body)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				logger.Info("body_too_large",
					slog.Int64("limit_bytes", h.maxBodySize),
					slog.Int64("latency_ms", time.Since(start).Milliseconds()),
				)
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, fmt.Sprintf("parse JSON: %v", err), http.StatusBadRequest)
			return
		}
		r.Form = values
	} else {
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
	}

	// Honeypot check (FR5) — silent 200 if triggered so bots can't
	// distinguish honeypot rejection from success. The response body
	// mirrors the real success path's shape (status + submission_id)
	// for the same reason: an observant bot inspecting the body must
	// not be able to tell the two paths apart. Form mode only — API
	// mode is authenticated and has no browser bots.
	if !apiMode && spam.CheckHoneypot(r.Form, h.cfg.Honeypot) == spam.SilentReject {
		logger.Info("spam_blocked",
			slog.String("kind", "honeypot"),
			slog.Int64("latency_ms", time.Since(start).Milliseconds()),
		)
		h.recorder.SpamBlocked(h.cfg.Path, "honeypot")
		response.WriteJSON(w, http.StatusOK, response.Success{
			Status:       "ok",
			SubmissionID: submissionID,
		})
		return
	}

	// FR57: CSRF check (form-mode only). When csrf_secret is configured,
	// every submission must carry a verifiable _csrf_token form field.
	// Failures return 403 with no detail — the structured log line has
	// the operator-facing reason.
	if !apiMode && h.cfg.CSRFSecret != "" {
		token := r.Form.Get(csrf.TokenField)
		if err := csrf.Verify(token, []byte(h.cfg.CSRFSecret), h.csrfTokenTTL, time.Now()); err != nil {
			logger.Info("csrf_rejected",
				slog.String("reason", err.Error()),
				slog.Int64("latency_ms", time.Since(start).Milliseconds()),
			)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
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
		h.recorder.ValidationFailed(h.cfg.Path)
		response.WriteJSON(w, http.StatusUnprocessableEntity, response.Validation(missing, fieldErrors))
		return
	}

	logger.Info("submission_received")
	h.recorder.Submitted(h.cfg.Path)

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

	// FR46: per-request to_override (api-mode only). Absent → use cfg.To;
	// present but invalid → 422. Each entry passes the same email-syntax
	// check as form-mode submissions (FR11 reuse). Form mode ignores any
	// "to_override" form field — recipients are operator-controlled there
	// by design (ADR-11).
	toAddresses := h.cfg.To
	if apiMode {
		if override, present := r.Form[toOverrideField]; present {
			if len(override) == 0 {
				response.WriteJSON(w, http.StatusUnprocessableEntity,
					response.Validation(nil, map[string]string{
						toOverrideField: "must contain at least one address",
					}))
				return
			}
			validated := make([]string, 0, len(override))
			invalid := []string{}
			for _, addr := range override {
				trimmed := strings.TrimSpace(addr)
				if !validate.Email(trimmed) {
					invalid = append(invalid, addr)
					continue
				}
				validated = append(validated, trimmed)
			}
			if len(invalid) > 0 {
				response.WriteJSON(w, http.StatusUnprocessableEntity,
					response.Validation(nil, map[string]string{
						toOverrideField: fmt.Sprintf("invalid email address(es): %v", invalid),
					}))
				return
			}
			toAddresses = validated
		}
	}

	msg := transport.Message{
		From:     h.cfg.From,
		To:       toAddresses,
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

	// FR56: dry-run mode. Short-circuit before transport.Send and return
	// the prepared Message so operators can debug template rendering and
	// recipient resolution without sending mail. The idempotency cache
	// treats this as a normal 200 (cacheable per the deferred Store).
	if h.cfg.DryRun {
		logger.Info("submission_dry_run",
			slog.Int64("latency_ms", time.Since(start).Milliseconds()),
		)
		response.WriteJSON(w, http.StatusOK, response.DryRun{
			Status:       "dry_run",
			SubmissionID: submissionID,
			PreparedMessage: response.PreparedMessage{
				From:     msg.From,
				To:       msg.To,
				ReplyTo:  msg.ReplyTo,
				Subject:  msg.Subject,
				BodyText: msg.BodyText,
			},
		})
		return
	}

	// Send with retry policy (FR19-22) under the request hard timeout.
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	result, err := sendWithRetry(ctx, h.transport, msg, logger)
	if err != nil {
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
		h.recorder.Failed(h.cfg.Path, h.cfg.Transport.Type, errorClassName(err))
		http.Error(w, "submission could not be delivered", http.StatusBadGateway)
		return
	}

	latency := time.Since(start)
	sentAttrs := []any{
		slog.Int64("latency_ms", latency.Milliseconds()),
	}
	if result.MessageID != "" {
		sentAttrs = append(sentAttrs, slog.String("transport_message_id", result.MessageID))
	}
	logger.Info("submission_sent", sentAttrs...)
	h.recorder.Sent(h.cfg.Path, h.cfg.Transport.Type, latency)
	response.WriteJSON(w, http.StatusOK, response.Success{
		Status:       "ok",
		SubmissionID: submissionID,
	})
}

// recordingResponseWriter wraps an http.ResponseWriter to capture the
// status code, body bytes, and Content-Type header so an idempotent
// request's response can be stored verbatim for replay (NFR20). All
// methods pass through to the underlying writer.
type recordingResponseWriter struct {
	http.ResponseWriter
	status        int
	contentType   string
	body          bytes.Buffer
	headerWritten bool
}

func (r *recordingResponseWriter) WriteHeader(code int) {
	if r.headerWritten {
		return
	}
	r.headerWritten = true
	r.status = code
	r.contentType = r.ResponseWriter.Header().Get("Content-Type")
	r.ResponseWriter.WriteHeader(code)
}

func (r *recordingResponseWriter) Write(b []byte) (int, error) {
	if !r.headerWritten {
		// Match net/http's implicit 200 behavior — Write without a
		// preceding WriteHeader is a 200.
		r.WriteHeader(http.StatusOK)
	}
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

// finalizeIdempotent runs in the request-handling defer to either Store
// the captured response under idemKey or Abandon the in-flight claim.
//
// Cache policy: 2xx and 422 (deterministic validation failures) get
// stored. Everything else (4xx-non-422, 429, 5xx, no-status-written)
// abandons so the next caller retries fresh. 429 rate-limit and 5xx
// transport-failure responses are transient by nature; caching them
// would freeze a stale answer for 24h.
func (h *Handler) finalizeIdempotent(idemKey string, rw *recordingResponseWriter) {
	cacheable := rw.status == http.StatusOK || rw.status == http.StatusUnprocessableEntity
	if !cacheable {
		h.idemCache.Abandon(idemKey)
		return
	}
	h.idemCache.Store(idemKey, idempotency.Response{
		Status:      rw.status,
		Body:        rw.body.Bytes(),
		ContentType: rw.contentType,
	})
}

// errorClassName extracts the ErrorClass.String() value from a transport
// error so it can be used as a metric label. Returns "unknown" when the
// error isn't a *transport.TransportError (which would be a contract bug
// from a transport implementation).
func errorClassName(err error) string {
	var te *transport.TransportError
	if errors.As(err, &te) {
		return te.Class.String()
	}
	return "unknown"
}

// replayCachedResponse writes a previously-stored idempotent response
// back to the caller byte-for-byte (NFR20).
func replayCachedResponse(w http.ResponseWriter, r *idempotency.Response) {
	if r.ContentType != "" {
		w.Header().Set("Content-Type", r.ContentType)
	}
	w.WriteHeader(r.Status)
	_, _ = w.Write(r.Body)
}

// authenticateAPIRequest extracts the Bearer token from r's Authorization
// header and matches it against the endpoint's configured api_keys using
// constant-time comparison (NFR19). Returns the matched key on success
// (callers use it as the rate-limit bucket key per FR35) and a boolean
// indicating whether any key matched.
//
// The matched key MUST NOT be returned in HTTP responses, written to logs,
// or exposed in error messages (NFR21). The caller's responsibility is to
// use the returned key only as opaque bucket identity.
func (h *Handler) authenticateAPIRequest(r *http.Request) (string, bool) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return "", false
	}
	// RFC 6750 §2.1: scheme name is case-insensitive ("Bearer" / "bearer"
	// / "BEARER" all valid). Token portion is opaque, no whitespace
	// tolerance beyond the single separator space.
	const prefix = "Bearer "
	if len(authHeader) <= len(prefix) || !strings.EqualFold(authHeader[:len(prefix)], prefix) {
		return "", false
	}
	token := authHeader[len(prefix):]
	if token == "" {
		return "", false
	}

	tokenBytes := []byte(token)
	var matched string
	// Iterate every configured key regardless of early match. Returning
	// on the first match would reveal via timing which position in the
	// api_keys list matched. ConstantTimeCompare itself is constant-time
	// for equal-length inputs (and returns 0 quickly for length mismatch,
	// which leaks key length only — acceptable since operators choose
	// their key shapes).
	for _, k := range h.cfg.APIKeys {
		if subtle.ConstantTimeCompare(tokenBytes, []byte(k)) == 1 {
			matched = k
		}
	}
	return matched, matched != ""
}

// sendWithRetry implements the FR19-22 retry policy:
//
//   - On *transport.TransportError with class ErrTransient: wait 1s, retry once.
//   - On *transport.TransportError with class ErrRateLimited: wait 5s, retry once.
//   - On any class ErrTerminal (or non-TransportError): no retry.
//   - The provided ctx carries the 10s request hard timeout (FR22);
//     if it expires during the backoff, the second attempt is skipped.
//
// Returns the transport result and nil on success, or a zero result and the
// most recent TransportError on failure.
func sendWithRetry(ctx context.Context, t transport.Transport, msg transport.Message, logger *slog.Logger) (transport.SendResult, error) {
	result, err := t.Send(ctx, msg)
	if err == nil {
		return result, nil
	}

	var te *transport.TransportError
	if !errors.As(err, &te) {
		// Non-TransportError — caller contract violation; treat as terminal.
		return transport.SendResult{}, err
	}

	var delay time.Duration
	switch te.Class {
	case transport.ErrTransient:
		delay = transientRetryDelay
	case transport.ErrRateLimited:
		delay = rateLimitedRetryDelay
	default:
		// ErrTerminal, ErrUnknown — no retry.
		return transport.SendResult{}, err
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
		return transport.SendResult{}, err
	case <-time.After(delay):
	}

	retryResult, retryErr := t.Send(ctx, msg)
	if retryErr == nil {
		logger.Info("send_retry_succeeded")
		return retryResult, nil
	}
	logger.Info("send_retry_failed",
		slog.String("error", retryErr.Error()),
	)
	return transport.SendResult{}, retryErr
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

// isJSONContentType returns true if the Content-Type header indicates a
// JSON body. Used on api-mode endpoints (FR37). Parameters after `;` (e.g.
// `; charset=utf-8`) are tolerated.
func isJSONContentType(ct string) bool {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(strings.ToLower(ct))
	return ct == "application/json"
}

// parseJSONBody decodes a JSON object request body into a url.Values map,
// matching the shape produced by r.ParseForm so the downstream validation,
// templating, and transport pipeline can be ingress-agnostic (FR36, FR38,
// FR39).
//
// Coercion rules (architecture doc Open Q5):
//   - String values pass through as single-element slices.
//   - Booleans and numbers coerce to their string representation via
//     strconv. JSON numbers decode as float64; integers in the safe range
//     format without a decimal point.
//   - null values are omitted (treated as absent — matches form-mode where
//     an unset field never appears in r.Form).
//   - Arrays of primitives become multi-element slices (parallel to
//     form-mode multi-value fields like `name=a&name=b`).
//   - Nested objects, top-level non-object bodies, and arrays containing
//     non-primitives are rejected with a clear error (HTTP 400 via caller).
func parseJSONBody(body io.Reader) (url.Values, error) {
	raw := make(map[string]any)
	dec := json.NewDecoder(body)
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	// Reject trailing content after the top-level object — e.g. `{"a":1}{"b":2}`
	// or `{}garbage`. dec.More() reports whether the decoder has more JSON
	// values to consume from the stream.
	if dec.More() {
		return nil, errors.New("unexpected trailing content after top-level JSON object")
	}

	out := make(url.Values, len(raw))
	for k, v := range raw {
		switch val := v.(type) {
		case nil:
			// Omit: matches "form field never submitted" semantics.
		case string:
			out[k] = []string{val}
		case bool:
			out[k] = []string{strconv.FormatBool(val)}
		case float64:
			out[k] = []string{formatJSONNumber(val)}
		case []any:
			strs, err := coerceJSONArray(k, val)
			if err != nil {
				return nil, err
			}
			out[k] = strs
		case map[string]any:
			return nil, fmt.Errorf("nested objects are not supported in v1.1 (field %q is an object)", k)
		default:
			return nil, fmt.Errorf("unsupported JSON type for field %q: %T", k, v)
		}
	}
	return out, nil
}

// coerceJSONArray handles []any values from json.Unmarshal — converts
// every element to a string. Nested arrays and objects are rejected
// (consistent with the top-level "no nested objects" rule).
func coerceJSONArray(field string, arr []any) ([]string, error) {
	out := make([]string, 0, len(arr))
	for i, v := range arr {
		switch val := v.(type) {
		case nil:
			out = append(out, "")
		case string:
			out = append(out, val)
		case bool:
			out = append(out, strconv.FormatBool(val))
		case float64:
			out = append(out, formatJSONNumber(val))
		default:
			return nil, fmt.Errorf("field %q[%d]: nested arrays and objects are not supported in v1.1", field, i)
		}
	}
	return out, nil
}

// formatJSONNumber renders a JSON number (always float64 from
// json.Unmarshal) as a compact string. Integer-valued floats render
// without a trailing ".0".
func formatJSONNumber(f float64) string {
	// FormatFloat with -1 precision uses the shortest representation that
	// round-trips. Integer values come out without a decimal point.
	return strconv.FormatFloat(f, 'f', -1, 64)
}
