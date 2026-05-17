package ingress

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// HTTPIngress wraps an http.Server lifecycle to satisfy the Ingress
// interface. Used by cmd/posthorn for the HTTP form / api-mode listener;
// owns the ServeMux that routes each endpoint to its gateway.Handler.
type HTTPIngress struct {
	server *http.Server
	logger *slog.Logger

	mu        sync.Mutex
	started   bool
	stopCalls int
}

// NewHTTPIngress wraps a fully-configured *http.Server in the Ingress
// lifecycle. cmd/posthorn constructs the server with its own preferred
// timeouts (ReadHeaderTimeout, IdleTimeout) and passes it here. The
// logger is for ingress-lifecycle events; the wrapped handler owns its
// own logging.
func NewHTTPIngress(server *http.Server, logger *slog.Logger) *HTTPIngress {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(noopWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
	}
	return &HTTPIngress{
		server: server,
		logger: logger,
	}
}

// Name returns "http" (Ingress interface).
func (h *HTTPIngress) Name() string { return "http" }

// Start runs http.Server.ListenAndServe. Returns nil when Stop is
// invoked; non-nil on any other error (e.g., bind failure).
func (h *HTTPIngress) Start(_ context.Context) error {
	h.mu.Lock()
	h.started = true
	h.mu.Unlock()

	h.logger.Info("http ingress listening", slog.String("addr", h.server.Addr))
	err := h.server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Stop signals the HTTP server to drain in-flight requests and shut
// down. The context bounds the drain time; once it expires, in-flight
// connections are forcibly closed.
func (h *HTTPIngress) Stop(ctx context.Context) error {
	h.mu.Lock()
	h.stopCalls++
	first := h.stopCalls == 1
	h.mu.Unlock()
	if !first {
		// Idempotent: subsequent Stop calls are no-ops.
		return nil
	}
	h.logger.Info("http ingress stopping")
	return h.server.Shutdown(ctx)
}

// noopWriter is an io.Writer that discards output. Used by NewHTTPIngress
// when the caller passes nil for logger (rare; only in tests that don't
// care about ingress-lifecycle log lines).
type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) { return len(p), nil }

// Compile-time check: HTTPIngress satisfies Ingress.
var _ Ingress = (*HTTPIngress)(nil)

// drainTimeout is the default deadline NewHTTPIngress uses if Stop is
// called with a context that has no deadline. Public so cmd/posthorn
// can override during testing.
var DrainTimeout = 10 * time.Second
