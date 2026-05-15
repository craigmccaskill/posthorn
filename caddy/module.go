// Package posthorn provides a Caddy v2 HTTP handler module that wraps the
// Posthorn email-gateway form-ingress pipeline.
//
// The directive `posthorn <path> { ... }` (parsed in caddyfile.go, Story 6.2)
// produces one Handler per endpoint. Each Handler holds an inner
// *gateway.Handler from the core module and delegates to it on matching
// requests. Non-matching requests pass through to the next handler in
// Caddy's middleware chain.
//
// ADR-6: the core module has zero Caddy dependency. This adapter is the
// only place Caddy types are referenced. Adding a Caddy import to core
// fails the workspace build.
package posthorn

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap/exp/zapslog"

	pconfig "github.com/craigmccaskill/posthorn/config"
	"github.com/craigmccaskill/posthorn/gateway"
	"github.com/craigmccaskill/posthorn/transport"
)

func init() {
	caddy.RegisterModule(Handler{})
}

// Handler is the Caddy v2 module wrapper. One instance per `posthorn`
// directive — i.e., one configured endpoint. Fields mirror
// [pconfig.EndpointConfig] with JSON tags suitable for Caddy's adapter
// pipeline; the Caddyfile unmarshaler (Story 6.2) populates them from
// the directive body. JSON-config users populate them directly.
type Handler struct {
	// Path is the URL path this handler responds to. Non-matching
	// requests are passed to the next handler so the rest of the
	// Caddy site continues to work.
	Path string `json:"path,omitempty"`

	// To is the recipient list. Required.
	To []string `json:"to,omitempty"`

	// From is the sender envelope address. Required. Must be on a
	// Postmark-verified domain.
	From string `json:"from,omitempty"`

	// Transport is the egress transport config. v1.0 supports only
	// {Type: "postmark", Settings: {api_key: ...}}.
	Transport pconfig.TransportConfig `json:"transport,omitempty"`

	// RateLimit, if non-nil, enables per-IP token-bucket rate limiting
	// for this endpoint.
	RateLimit *pconfig.RateLimitConfig `json:"rate_limit,omitempty"`

	// TrustedProxies is the CIDR list of proxies whose X-Forwarded-For
	// header should be trusted for client-IP extraction.
	TrustedProxies []string `json:"trusted_proxies,omitempty"`

	// Honeypot, if non-empty, is the form field that bots will fill in;
	// non-empty values trigger a silent 200.
	Honeypot string `json:"honeypot,omitempty"`

	// AllowedOrigins is the Origin/Referer allowlist. Configured ⇒
	// fail-closed when both headers are missing (NFR4).
	AllowedOrigins []string `json:"allowed_origins,omitempty"`

	// MaxBodySize is the per-request body cap (e.g., "32KB", "1MB").
	MaxBodySize string `json:"max_body_size,omitempty"`

	// Required is the list of form fields that must be present and
	// non-empty. Missing fields → 422.
	Required []string `json:"required,omitempty"`

	// EmailField is the form field validated as an email address.
	// Defaults to "email" if unset.
	EmailField string `json:"email_field,omitempty"`

	// Subject is the text/template source for the email subject.
	Subject string `json:"subject,omitempty"`

	// Body is the text/template source for the email body. Inline if it
	// contains "{{"; otherwise treated as a file path.
	Body string `json:"body,omitempty"`

	// LogFailedSubmissions, when non-nil, overrides the default behavior
	// of logging the full submission payload on terminal failure. nil ⇒
	// default true.
	LogFailedSubmissions *bool `json:"log_failed_submissions,omitempty"`

	// RedirectSuccess is the URL to redirect to on success when the
	// client prefers text/html.
	RedirectSuccess string `json:"redirect_success,omitempty"`

	// RedirectError is the URL to redirect to on validation/rate-limit
	// failures when the client prefers text/html.
	RedirectError string `json:"redirect_error,omitempty"`

	// inner is the wrapped core gateway handler. Constructed in Provision.
	inner *gateway.Handler
}

// CaddyModule returns the Caddy module info.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.posthorn",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision builds the inner gateway.Handler from the configured fields.
// Errors from this method surface via `caddy validate` and prevent the
// server from starting with a misconfigured directive.
func (h *Handler) Provision(ctx caddy.Context) error {
	// Expand `{env.VAR}` and other Caddy placeholders in transport
	// settings before constructing the transport. The api_key case is
	// load-bearing: the literal "{env.POSTMARK_API_KEY}" would be
	// rejected by Postmark with a 401 if not resolved.
	repl := caddy.NewReplacer()
	for k, v := range h.Transport.Settings {
		if s, ok := v.(string); ok {
			h.Transport.Settings[k] = repl.ReplaceAll(s, "")
		}
	}

	cfg := h.toEndpointConfig()

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("posthorn: %w", err)
	}

	t, err := buildTransport(cfg.Transport)
	if err != nil {
		return fmt.Errorf("posthorn: %w", err)
	}

	// Bridge Caddy's zap logger into the slog interface that core uses.
	// Caddy gives every module its own scoped *zap.Logger via ctx.Logger;
	// the zapslog handler is the supported way to feed those events into
	// slog without losing structured fields.
	zapLogger := ctx.Logger(h)
	slogger := slog.New(zapslog.NewHandler(zapLogger.Core()))

	inner, err := gateway.New(cfg, t, gateway.WithLogger(slogger))
	if err != nil {
		return fmt.Errorf("posthorn: %w", err)
	}
	h.inner = inner
	return nil
}

// Validate performs post-Provision sanity checks. The bulk of validation
// happens in Provision (via EndpointConfig.Validate) so this only catches
// the case where Provision didn't run.
func (h *Handler) Validate() error {
	if h.inner == nil {
		return errors.New("posthorn: handler not provisioned (internal error)")
	}
	if h.Path == "" {
		return errors.New("posthorn: path is required")
	}
	return nil
}

// ServeHTTP implements [caddyhttp.MiddlewareHandler].
//
// Matching requests (exact path equality) are handed to the wrapped
// gateway.Handler. Non-matching requests pass to next, so other handlers
// in the Caddy route chain — including the rest of the user's site — keep
// working. This mirrors the standalone binary's ServeMux routing.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if r.URL.Path != h.Path {
		return next.ServeHTTP(w, r)
	}
	h.inner.ServeHTTP(w, r)
	return nil
}

// toEndpointConfig maps the Caddy-facing field set onto the core
// EndpointConfig the gateway constructor expects. The two field sets are
// nearly identical by design; this single translation point keeps the
// duplication contained.
func (h *Handler) toEndpointConfig() pconfig.EndpointConfig {
	return pconfig.EndpointConfig{
		Path:                 h.Path,
		To:                   h.To,
		From:                 h.From,
		Transport:            h.Transport,
		RateLimit:            h.RateLimit,
		TrustedProxies:       h.TrustedProxies,
		Honeypot:             h.Honeypot,
		AllowedOrigins:       h.AllowedOrigins,
		MaxBodySize:          h.MaxBodySize,
		Required:             h.Required,
		EmailField:           h.EmailField,
		Subject:              h.Subject,
		Body:                 h.Body,
		LogFailedSubmissions: h.LogFailedSubmissions,
		RedirectSuccess:      h.RedirectSuccess,
		RedirectError:        h.RedirectError,
	}
}

// buildTransport mirrors the standalone binary's buildTransport switch
// (core/cmd/posthorn/main.go). v1.0 supports only "postmark". When v1.1+
// adds resend/mailgun/ses/smtp, both call sites get the new case.
//
// Worth a tiny refactor to a shared helper at some point — but moving it
// into core/transport creates a transport→config dependency that doesn't
// otherwise exist, and the duplication is ~10 lines. Filed mentally; not
// blocking 6.1.
func buildTransport(cfg pconfig.TransportConfig) (transport.Transport, error) {
	switch cfg.Type {
	case "postmark":
		apiKey, _ := cfg.Settings["api_key"].(string)
		if apiKey == "" {
			return nil, errors.New("postmark: api_key is empty")
		}
		baseURL, _ := cfg.Settings["base_url"].(string) // test-only escape hatch
		return transport.NewPostmarkTransport(apiKey, baseURL), nil
	default:
		return nil, fmt.Errorf("unknown transport type %q", cfg.Type)
	}
}

// Interface guards — compile-time confirmation that Handler satisfies the
// Caddy interfaces we claim. If a future Caddy version changes a
// signature, these break the build at module level instead of silently
// at runtime registration.
var (
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddy.Validator             = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
)
