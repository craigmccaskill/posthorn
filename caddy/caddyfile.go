package posthorn

import (
	"strconv"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"

	pconfig "github.com/craigmccaskill/posthorn/config"
)

// init registers the `posthorn` directive with Caddy's HTTP Caddyfile
// adapter. After this, operators can write:
//
//	example.com {
//	    posthorn /api/contact { ... }
//	}
//
// in a Caddyfile and have it adapt to a JSON config that loads our
// Handler with the http.handlers.posthorn module ID.
func init() {
	httpcaddyfile.RegisterHandlerDirective("posthorn", parseCaddyfileDirective)
}

// parseCaddyfileDirective is the entry point that httpcaddyfile calls
// when it encounters our directive. It defers to Handler's own
// UnmarshalCaddyfile so the parsing logic lives next to the struct.
func parseCaddyfileDirective(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var handler Handler
	if err := handler.UnmarshalCaddyfile(h.Dispenser); err != nil {
		return nil, err
	}
	return &handler, nil
}

// UnmarshalCaddyfile parses a `posthorn <path> { ... }` directive into
// the receiver. Grammar mirrors the TOML schema (single source of truth
// in core/config.EndpointConfig); see [site docs / deployment / caddy-adapter]
// for the operator-facing reference.
//
// Each subdirective maps 1:1 onto a field of Handler. Multi-arg lines
// (like `to a@b c@d`) build slices; bool subdirectives (`log_failed_submissions`)
// require an explicit "true"/"false" argument; the nested `transport`
// and `rate_limit` blocks have their own small grammars.
//
// `{env.VAR}` placeholders are NOT expanded here — Caddy preserves them
// in the JSON config and the Provision step calls caddy.Replacer on
// transport settings (where api keys live). Doing it during parse would
// freeze the env at adapt time, defeating Caddy's runtime-resolution
// model.
func (h *Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() { // consume the "posthorn" token; loop guards against multi-occurrence
		// Path is the single positional argument after the directive name.
		if !d.NextArg() {
			return d.ArgErr()
		}
		h.Path = d.Val()
		if d.NextArg() {
			return d.Errf("posthorn: expected exactly one path argument, got extra %q", d.Val())
		}

		// Walk the directive block body.
		for d.NextBlock(0) {
			switch d.Val() {
			case "to":
				args := d.RemainingArgs()
				if len(args) == 0 {
					return d.ArgErr()
				}
				h.To = append(h.To, args...)

			case "from":
				if !d.AllArgs(&h.From) {
					return d.ArgErr()
				}

			case "required":
				args := d.RemainingArgs()
				if len(args) == 0 {
					return d.ArgErr()
				}
				h.Required = append(h.Required, args...)

			case "email_field":
				if !d.AllArgs(&h.EmailField) {
					return d.ArgErr()
				}

			case "reply_to_email_field":
				if !d.AllArgs(&h.ReplyToEmailField) {
					return d.ArgErr()
				}

			case "honeypot":
				if !d.AllArgs(&h.Honeypot) {
					return d.ArgErr()
				}

			case "allowed_origins":
				args := d.RemainingArgs()
				if len(args) == 0 {
					return d.ArgErr()
				}
				h.AllowedOrigins = append(h.AllowedOrigins, args...)

			case "max_body_size":
				if !d.AllArgs(&h.MaxBodySize) {
					return d.ArgErr()
				}

			case "trusted_proxies":
				args := d.RemainingArgs()
				if len(args) == 0 {
					return d.ArgErr()
				}
				h.TrustedProxies = append(h.TrustedProxies, args...)

			case "subject":
				if !d.AllArgs(&h.Subject) {
					return d.ArgErr()
				}

			case "body":
				if !d.AllArgs(&h.Body) {
					return d.ArgErr()
				}

			case "redirect_success":
				if !d.AllArgs(&h.RedirectSuccess) {
					return d.ArgErr()
				}

			case "redirect_error":
				if !d.AllArgs(&h.RedirectError) {
					return d.ArgErr()
				}

			case "log_failed_submissions":
				var v string
				if !d.AllArgs(&v) {
					return d.ArgErr()
				}
				switch v {
				case "true":
					t := true
					h.LogFailedSubmissions = &t
				case "false":
					f := false
					h.LogFailedSubmissions = &f
				default:
					return d.Errf("log_failed_submissions: expected \"true\" or \"false\", got %q", v)
				}

			case "rate_limit":
				var countStr, intervalStr string
				if !d.AllArgs(&countStr, &intervalStr) {
					return d.ArgErr()
				}
				count, err := strconv.Atoi(countStr)
				if err != nil {
					return d.Errf("rate_limit count: %v", err)
				}
				dur, err := time.ParseDuration(intervalStr)
				if err != nil {
					return d.Errf("rate_limit interval: %v", err)
				}
				h.RateLimit = &pconfig.RateLimitConfig{
					Count:    count,
					Interval: pconfig.Duration(dur),
				}

			case "transport":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.Transport.Type = d.Val()
				if d.NextArg() {
					return d.Errf("transport: expected one type argument, got extra %q", d.Val())
				}
				h.Transport.Settings = map[string]any{}
				for nesting := d.Nesting(); d.NextBlock(nesting); {
					key := d.Val()
					var val string
					if !d.AllArgs(&val) {
						return d.ArgErr()
					}
					h.Transport.Settings[key] = val
				}

			default:
				return d.Errf("posthorn: unknown subdirective %q", d.Val())
			}
		}
	}
	return nil
}
