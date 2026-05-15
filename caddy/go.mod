module github.com/craigmccaskill/posthorn/caddy

go 1.25.0

// Pre-v1.0 development: resolve the core module from the local checkout.
// This `replace` directive comes out as part of the v1.0.0 release prep
// (Story 7.3) so external `xcaddy build` invocations resolve core via
// the published module proxy.
replace github.com/craigmccaskill/posthorn => ../core

require (
	github.com/caddyserver/caddy/v2 v2.10.0
	github.com/craigmccaskill/posthorn v0.0.0-00010101000000-000000000000
	go.uber.org/zap v1.27.0
	go.uber.org/zap/exp v0.3.0
)
