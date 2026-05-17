// Package ingress defines the lifecycle interface for things that
// accept inbound traffic and convert it to transport.Message values
// for the egress pipeline (FR60, FR68, ADR-12).
//
// v1.0 has two implementations:
//
//   - gateway.Handler — HTTP form / api-mode submissions, wrapped in
//     an HTTPIngress (in cmd/posthorn) that owns the http.Server
//     lifecycle.
//   - smtp.Listener — TCP listener accepting SMTP from internal apps,
//     parsing MIME into transport.Message.
//
// The interface itself is intentionally minimal: Start launches the
// listener loop and runs until Stop returns or the context cancels.
// All ingress-specific concepts (templates, content negotiation,
// idempotency keys, CSRF, SMTP AUTH, MIME parsing) live inside the
// implementation and never cross the interface boundary. Egress
// (transport.Send) is ingress-agnostic per ADR-12.
package ingress

import "context"

// Ingress is "thing that accepts inbound traffic and produces
// transport.Message values dispatched through a Transport." The
// cmd/posthorn binary holds a slice of Ingress values, calls Start
// on each in its own goroutine, and calls Stop on each on SIGTERM.
type Ingress interface {
	// Name returns a stable identifier for logging and metrics labels
	// (e.g., "http", "smtp"). Two Ingress instances of the same type
	// may share a Name — operators rarely care to disambiguate "the
	// HTTP ingress on :8080" from "the HTTP ingress on :8081" in a
	// single Posthorn process.
	Name() string

	// Start runs the ingress's accept loop. Blocks until Stop is
	// called or an unrecoverable error occurs. Returns nil on a
	// graceful Stop; non-nil only on errors that should crash the
	// process (e.g., bind failed).
	//
	// The provided context is informational — Stop is the canonical
	// shutdown trigger. Implementations may use ctx.Done() to abort
	// long-running operations when the parent context cancels.
	Start(ctx context.Context) error

	// Stop signals the ingress to drain in-flight work and return.
	// Implementations should be idempotent (multiple Stop calls are
	// safe). The provided context bounds the drain time; Stop
	// returns when drain completes or the context's deadline fires.
	Stop(ctx context.Context) error
}
