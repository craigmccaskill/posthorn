package formward

import "github.com/caddyserver/caddy/v2"

// Module is the caddy-formward handler. It accepts form submissions on a
// configured path and forwards them as email via a configured transport.
//
// JSON config example:
//
//	{
//	    "handler": "formward",
//	    "path":    "/contact"
//	}
//
// All fields use omitempty so that a zero-value Module serializes to "{}",
// which is important for Caddy's config diffing logic.
type Module struct {
	// Path is the URL path this handler matches. Used as a route matcher when
	// the module is configured via Caddyfile, and as a structured log field
	// (NFR7 endpoint) on every request regardless of config source.
	Path string `json:"path,omitempty"`
}

// Provision implements caddy.Provisioner. Called once when the config is
// loaded (or reloaded). In Epic 1 this is a no-op; later stories add:
//   - transport construction (Epic 2)
//   - rate-limiter setup (Epic 3)
//   - template compilation (Epic 4)
//   - logger init and default population (Epic 2+)
func (m *Module) Provision(_ caddy.Context) error {
	return nil
}

// Validate implements caddy.Validator. Called after Provision; checks
// semantic constraints not caught by the Caddyfile parser. In Epic 1 this
// is a no-op; full validation lands when to/from/transport config is added
// in Epic 2.
func (m *Module) Validate() error {
	return nil
}
