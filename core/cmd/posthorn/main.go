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
	"github.com/craigmccaskill/posthorn/ingress"
	"github.com/craigmccaskill/posthorn/metrics"
	"github.com/craigmccaskill/posthorn/smtp"
	"github.com/craigmccaskill/posthorn/spam"
	"github.com/craigmccaskill/posthorn/transport"
)

// version is the release version. Replaced at build time with -ldflags
// "-X main.version=v1.0.0" in the release workflow (Story 5.3).
var version = "v0.0.1-dev"

const usage = `posthorn — the unified outbound mail layer for self-hosted projects.

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

	// HTTP ingress is the v1.0 form/api-mode listener. v1.0 block D
	// (SMTP ingress) appends a second ingress to this slice when the
	// operator configures [smtp_listener].
	ingresses := []ingress.Ingress{
		ingress.NewHTTPIngress(server, logger),
	}

	// v1.0 block D: optional SMTP listener (FR62). Built only when the
	// operator's TOML includes [smtp_listener].
	if cfg.SMTPListener != nil {
		smtpIng, err := buildSMTPIngress(cfg.SMTPListener, logger)
		if err != nil {
			return fmt.Errorf("build smtp_listener: %w", err)
		}
		ingresses = append(ingresses, smtpIng)
		logger.Info("smtp_listener registered",
			slog.String("listen", cfg.SMTPListener.Listen),
			slog.String("transport", cfg.SMTPListener.Transport.Type),
			slog.Int("smtp_users", len(cfg.SMTPListener.SMTPUsers)),
		)
	}

	return runIngressesUntilSignal(ingresses, logger)
}

// buildSMTPIngress converts the config-package SMTPListenerConfig into
// the smtp-package ListenerConfig, constructs the outbound transport
// via the same registry the HTTP endpoints use, and returns the
// resulting smtp.Listener (which satisfies ingress.Ingress).
func buildSMTPIngress(c *config.SMTPListenerConfig, logger *slog.Logger) (ingress.Ingress, error) {
	tp, err := buildTransport(c.Transport)
	if err != nil {
		return nil, fmt.Errorf("transport: %w", err)
	}
	// Parse max_message_size (default 1MB if unset).
	rawSize := c.MaxMessageSize
	if rawSize == "" {
		rawSize = "1MB"
	}
	maxBody, err := spam.ParseSize(rawSize)
	if err != nil {
		return nil, fmt.Errorf("max_message_size: %w", err)
	}
	listenerCfg := smtp.ListenerConfig{
		Listen:                  c.Listen,
		RequireTLS:              c.RequireTLS,
		TLSCert:                 c.TLSCert,
		TLSKey:                  c.TLSKey,
		ClientCertCA:            c.ClientCertCA,
		AuthRequired:            smtp.AuthMode(c.AuthRequired),
		AllowedSenders:          c.AllowedSenders,
		AllowedRecipients:       c.AllowedRecipients,
		MaxRecipientsPerSession: c.MaxRecipientsPerSession,
		MaxMessageSize:          rawSize,
		IdleTimeout:             c.IdleTimeout,
		Transport:               c.Transport,
	}
	listenerCfg.SMTPUsers = make([]smtp.User, len(c.SMTPUsers))
	for i, u := range c.SMTPUsers {
		listenerCfg.SMTPUsers[i] = smtp.User{Username: u.Username, Password: u.Password}
	}
	if err := listenerCfg.Validate(); err != nil {
		return nil, err
	}
	return smtp.New(listenerCfg, tp, maxBody, logger, nil /* recorder wired by buildMux for HTTP only in v1.0 */)
}

// runIngressesUntilSignal starts each ingress in its own goroutine,
// waits for SIGTERM/SIGINT, then drains in-flight work via Stop on
// each ingress with a 15s deadline (longer than the per-request 10s
// hard timeout from FR22 so in-flight retries can complete
// gracefully). A second signal forces immediate exit.
func runIngressesUntilSignal(ingresses []ingress.Ingress, logger *slog.Logger) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, len(ingresses))
	for _, ing := range ingresses {
		ing := ing
		go func() {
			err := ing.Start(ctx)
			if err != nil {
				errCh <- fmt.Errorf("%s ingress: %w", ing.Name(), err)
				return
			}
			errCh <- nil
		}()
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil {
			return err
		}
		// First ingress returned without error — unusual, but treat
		// as graceful end (other ingresses will follow).
	case sig := <-sigCh:
		logger.Info("shutdown signal received", slog.String("signal", sig.String()))
	}

	// Forced-exit watcher.
	go func() {
		sig := <-sigCh
		logger.Warn("second signal received, forcing exit", slog.String("signal", sig.String()))
		os.Exit(1)
	}()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	var firstErr error
	for _, ing := range ingresses {
		if err := ing.Stop(shutdownCtx); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s ingress graceful shutdown: %w", ing.Name(), err)
		}
	}
	logger.Info("posthorn stopped")
	return firstErr
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
// gets the logger and the shared metrics Recorder so per-request
// submission_id propagation and operator observability work.
//
// The mux additionally registers `/healthz` (FR54) and `/metrics`
// (FR55) at fixed paths. Operators can firewall those paths at the
// reverse proxy if internal-only access is desired.
func buildMux(cfg *config.Config, logger *slog.Logger) (*http.ServeMux, error) {
	mux := http.NewServeMux()

	metricsReg := metrics.New()
	recorder := metrics.NewRecorder(metricsReg)

	for i, ep := range cfg.Endpoints {
		t, err := buildTransport(ep.Transport)
		if err != nil {
			return nil, fmt.Errorf("endpoints[%d] (%s): transport: %w", i, ep.Path, err)
		}
		h, err := gateway.New(ep, t,
			gateway.WithLogger(logger),
			gateway.WithRecorder(recorder),
		)
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

	// FR54: /healthz — always-on liveness probe.
	mux.Handle("/healthz", metrics.HealthzHandler())
	// FR55: /metrics — Prometheus exposition. Same registry as the
	// Recorder above so all observations land in the scrape.
	mux.Handle("/metrics", metricsReg.Handler())

	return mux, nil
}

// buildTransport constructs a transport from its config block. Dispatch
// is via the transport package's registry — each transport (postmark,
// resend, mailgun, ses, smtp-out) registers its builder at init.
// Adding a new transport requires no edits here.
func buildTransport(cfg config.TransportConfig) (transport.Transport, error) {
	reg, ok := transport.Lookup(cfg.Type)
	if !ok {
		return nil, transport.UnknownTypeError(cfg.Type)
	}
	return reg.Build(cfg.Settings)
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
