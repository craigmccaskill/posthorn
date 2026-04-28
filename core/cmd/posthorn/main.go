// Command posthorn is the standalone Posthorn email-gateway binary.
//
// Usage:
//
//	posthorn serve --config /path/to/config.toml [--listen :8080]
//	posthorn validate --config /path/to/config.toml
//	posthorn version
//	posthorn help
//
// Loads a TOML config (with ${env.VAR} placeholder resolution), constructs
// one gateway.Handler per configured endpoint, mounts them on an
// http.ServeMux, and serves until SIGTERM/SIGINT.
//
// The validate subcommand parses + validates the config without starting
// the listener, useful for CI and pre-deploy checks.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/craigmccaskill/posthorn/config"
	"github.com/craigmccaskill/posthorn/gateway"
	"github.com/craigmccaskill/posthorn/transport"
)

// version is the release version. Replaced at build time with -ldflags
// "-X main.version=v1.0.0" in the release workflow (Story 5.3).
var version = "v0.0.1-dev"

const usage = `posthorn — self-hosted email gateway for cloud platforms that block outbound SMTP.

Usage:
  posthorn serve     [--config <path>] [--listen <addr>]
  posthorn validate  [--config <path>]
  posthorn version
  posthorn help

Default config path:  /etc/posthorn/config.toml
Default listen addr:  :8080

Examples:
  posthorn serve --config ./posthorn.toml --listen :8080
  posthorn validate --config ./posthorn.toml
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "serve":
		if err := runServe(args); err != nil {
			fmt.Fprintln(os.Stderr, "posthorn:", err)
			os.Exit(1)
		}
	case "validate":
		if err := runValidate(args); err != nil {
			fmt.Fprintln(os.Stderr, "posthorn:", err)
			os.Exit(1)
		}
	case "version", "-v", "--version":
		fmt.Println("posthorn", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "posthorn: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
}

// --- serve ---

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fs.String("config", "/etc/posthorn/config.toml", "path to TOML config file")
	listen := fs.String("listen", ":8080", "TCP listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	logger := buildLogger(cfg.Logging)
	logger.Info("posthorn starting",
		slog.String("version", version),
		slog.String("listen", *listen),
		slog.String("config", *configPath),
		slog.Int("endpoints", len(cfg.Endpoints)),
	)

	mux, err := buildMux(cfg, logger)
	if err != nil {
		return fmt.Errorf("build router: %w", err)
	}

	server := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	return runServerUntilSignal(server, logger)
}

// runServerUntilSignal starts the HTTP server, waits for SIGTERM/SIGINT,
// then drains in-flight requests with a 15s shutdown deadline (longer
// than the per-request 10s hard timeout from FR22 so in-flight retries
// can complete gracefully). A second signal forces immediate exit.
func runServerUntilSignal(server *http.Server, logger *slog.Logger) error {
	serverErr := make(chan error, 1)
	go func() {
		err := server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		if err != nil {
			return fmt.Errorf("listener: %w", err)
		}
		return nil
	case sig := <-sigCh:
		logger.Info("shutdown signal received", slog.String("signal", sig.String()))
	}

	// Forced-exit watcher: a second signal during shutdown bypasses the
	// graceful drain.
	go func() {
		sig := <-sigCh
		logger.Warn("second signal received, forcing exit", slog.String("signal", sig.String()))
		os.Exit(1)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	logger.Info("posthorn stopped")
	return nil
}

// --- validate ---

func runValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	configPath := fs.String("config", "/etc/posthorn/config.toml", "path to TOML config file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	// Load only validates structurally; we also try to construct a
	// transport and a gateway.Handler for each endpoint so template parse
	// errors and transport-key issues surface here too.
	for i, ep := range cfg.Endpoints {
		t, err := buildTransport(ep.Transport)
		if err != nil {
			return fmt.Errorf("endpoints[%d] (%s): transport: %w", i, ep.Path, err)
		}
		if _, err := gateway.New(ep, t); err != nil {
			return fmt.Errorf("endpoints[%d] (%s): %w", i, ep.Path, err)
		}
	}

	fmt.Printf("config OK: %d endpoint(s)\n", len(cfg.Endpoints))
	return nil
}

// --- shared plumbing ---

// buildMux constructs an http.ServeMux mapping each configured endpoint
// path to its gateway.Handler. Endpoints share no state. Each handler
// gets the logger so per-request submission_id propagation works.
func buildMux(cfg *config.Config, logger *slog.Logger) (*http.ServeMux, error) {
	mux := http.NewServeMux()
	for i, ep := range cfg.Endpoints {
		t, err := buildTransport(ep.Transport)
		if err != nil {
			return nil, fmt.Errorf("endpoints[%d] (%s): transport: %w", i, ep.Path, err)
		}
		h, err := gateway.New(ep, t, gateway.WithLogger(logger))
		if err != nil {
			return nil, fmt.Errorf("endpoints[%d] (%s): %w", i, ep.Path, err)
		}
		mux.Handle(ep.Path, h)
		logger.Info("endpoint registered",
			slog.String("path", ep.Path),
			slog.String("transport", ep.Transport.Type),
			slog.Int("recipients", len(ep.To)),
		)
	}
	return mux, nil
}

// buildTransport constructs a transport from its config block. v1.0
// supports only "postmark"; future transports add cases.
func buildTransport(cfg config.TransportConfig) (transport.Transport, error) {
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

// buildLogger returns a slog.Logger configured per the config's Logging
// section. v1.0 supports JSON format only (NFR7); level defaults to info.
func buildLogger(cfg config.LoggingConfig) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	return slog.New(handler)
}
