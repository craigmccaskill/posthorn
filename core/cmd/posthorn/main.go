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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/craigmccaskill/posthorn/config"
	"github.com/craigmccaskill/posthorn/gateway"
	"github.com/craigmccaskill/posthorn/ingress"
	"github.com/craigmccaskill/posthorn/lifecycle"
	"github.com/craigmccaskill/posthorn/metrics"
	"github.com/craigmccaskill/posthorn/smtp"
	"github.com/craigmccaskill/posthorn/spam"
	"github.com/craigmccaskill/posthorn/storage"
	"github.com/craigmccaskill/posthorn/transport"
)

// version is the release version. Replaced at build time with -ldflags
// "-X main.version=v1.0.0" in the release workflow (Story 5.3).
var version = "v0.0.1-dev"

const usage = `posthorn — the unified outbound mail layer for self-hosted projects.

Usage:
  posthorn serve         [--config <path>] [--listen <addr>]
  posthorn validate      [--config <path>]
  posthorn suppressions  <list | add <email> [--reason <r>] | remove <email>> [--config <path>]
  posthorn version
  posthorn help

Default config path:  /etc/posthorn/config.toml
Default listen addr:  :8080

Examples:
  posthorn serve --config ./posthorn.toml --listen :8080
  posthorn validate --config ./posthorn.toml
  posthorn suppressions list --config ./posthorn.toml
  posthorn suppressions add bounced@example.com --config ./posthorn.toml
  posthorn suppressions remove bounced@example.com --config ./posthorn.toml
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
	case "suppressions":
		if err := runSuppressions(args); err != nil {
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

	// One metrics registry + recorder for the whole process, shared by the
	// HTTP mux (which also exposes /metrics) and the SMTP ingress (#57) so
	// SMTP submissions land in the same scrape as HTTP ones.
	metricsReg := metrics.New()
	recorder := metrics.NewRecorder(metricsReg)

	// v2.0: optional storage spine (FR76). Presence-activates (NFR25);
	// without [storage] the gate stays nil and every path is v1.x.
	var gate *storage.Gate
	if cfg.Storage != nil {
		maxSize, err := spam.ParseSize(cfg.Storage.EffectiveMaxSize())
		if err != nil {
			return fmt.Errorf("storage.max_size: %w", err)
		}
		store, err := storage.Open(storage.Config{
			Path:      cfg.Storage.Path,
			InMemory:  cfg.Storage.InMemory,
			Retention: cfg.Storage.EffectiveRetention(),
			MaxSize:   maxSize,
		})
		if err != nil {
			return fmt.Errorf("open storage: %w", err)
		}
		defer func() { _ = store.Close() }()
		gate = storage.NewGate(store, logger)
		recorder.SetStorageHealthy(true)
		logger.Info("storage_enabled",
			slog.String("path", cfg.Storage.Path),
			slog.Bool("in_memory", cfg.Storage.InMemory),
			slog.Duration("retention", cfg.Storage.EffectiveRetention()),
			slog.String("max_size", cfg.Storage.EffectiveMaxSize()),
		)
	}

	mux, transports, err := buildMux(cfg, logger, metricsReg, recorder, gate)
	if err != nil {
		return fmt.Errorf("build router: %w", err)
	}

	// Server-level timeouts bound slow-body senders and slow-reading
	// clients so they can't hold a connection indefinitely (#42).
	// WriteTimeout's deadline is set when the request headers are read,
	// so it must cover the WHOLE remaining request: body read (bounded
	// by ReadTimeout) plus handler execution (bounded by the handler's
	// own 10s requestTimeout, which includes retry backoff). It must
	// therefore exceed ReadTimeout + requestTimeout, or a legitimate
	// slow upload followed by a provider retry would have its response
	// truncated after the mail was already sent — and form mode has no
	// idempotency, so the user's resubmit would double-send.
	server := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second, // > ReadTimeout (15s) + handler requestTimeout (10s)
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10, // 64 KB; explicit, was stdlib 1MB default
	}

	// HTTP ingress is the v1.0 form/api-mode listener. v1.0 block D
	// (SMTP ingress) appends a second ingress to this slice when the
	// operator configures [smtp_listener].
	ingresses := []ingress.Ingress{
		ingress.NewHTTPIngress(server, logger),
	}

	// v1.0 block D / v2.1 block F: SMTP listeners (FR62, FR96). One per
	// [smtp_listener] table or [[smtp_listeners]] entry, each with its
	// own transport, registered under the listener's name so queued
	// replays find it.
	smtpIngs, err := buildSMTPIngresses(cfg, logger, recorder, gate, transports)
	if err != nil {
		return err
	}
	ingresses = append(ingresses, smtpIngs...)

	if gate != nil {
		if err := checkUnnamedListenerQueue(cfg, gate.Store(), transports, logger); err != nil {
			return err
		}
	}

	// v2.0: background retry worker + storage maintenance (FR78-FR80).
	// Both stop when the ingresses shut down.
	if gate != nil {
		ctx, stop := context.WithCancel(context.Background())
		defer stop()
		worker := &storage.Worker{
			Store:  gate.Store(),
			Send:   queuedSendFunc(transports),
			Logger: logger,
		}
		go worker.Run(ctx, storage.Hooks{
			OnSent:       recorder.QueueSent,
			OnRetryAgain: recorder.QueueRetried,
			OnDeadLetter: recorder.QueueDeadLettered,
		})
		go gate.RunMaintenance(ctx, 0, 0, cfg.Storage.EffectiveRetention(), storage.MaintenanceHooks{
			OnHealth: recorder.SetStorageHealthy,
			OnDepth:  recorder.SetQueueDepth,
		})

		// v2.0: lifecycle event ingestion + callback forwarding (FR82-FR85).
		if cfg.Lifecycle != nil {
			webhooks := map[string]lifecycle.Webhook{}
			for _, ep := range cfg.Endpoints {
				if ep.WebhookURL != "" {
					webhooks[ep.Path] = lifecycle.Webhook{URL: ep.WebhookURL, Secret: ep.WebhookSecret}
				}
			}
			fwd := &lifecycle.Forwarder{
				Webhooks: webhooks,
				Gate:     gate,
				Logger:   logger,
				OnResult: recorder.LifecycleForward,
			}
			eventsHandler, err := lifecycle.NewHandler(
				cfg.Lifecycle.BasicAuthUsername, cfg.Lifecycle.BasicAuthPassword,
				gate, fwd, logger, recorder)
			if err != nil {
				return fmt.Errorf("build lifecycle handler: %w", err)
			}
			mux.Handle("/events/postmark", eventsHandler)
			go fwd.RunQueue(ctx, 0)
			logger.Info("lifecycle_enabled", slog.Int("webhook_endpoints", len(webhooks)))
		}
	}

	return runIngressesUntilSignal(ingresses, logger)
}

// queuedSendFunc resolves a queued submission's endpoint back to its
// transport. An endpoint that no longer exists in the config (edited
// between restarts) yields a terminal error, which dead-letters the
// row rather than looping forever.
func queuedSendFunc(transports map[string]transport.Transport) storage.SendFunc {
	return func(ctx context.Context, endpoint string, msg transport.Message) (transport.SendResult, error) {
		t, ok := transports[endpoint]
		if !ok {
			return transport.SendResult{}, &transport.TransportError{
				Class:   transport.ErrTerminal,
				Message: fmt.Sprintf("endpoint %q no longer configured", endpoint),
			}
		}
		return t.Send(ctx, msg)
	}
}

// checkUnnamedListenerQueue refuses to start when mail accepted on the
// unnamed [smtp_listener] is still in the retry queue and no listener
// answers to that name any more.
//
// Queued rows find their transport by the listener's name. Adding a
// second listener forces a name onto the first (FR94), so the rows a
// v2.0 process queued under "smtp_listener" would resolve to nothing
// and queuedSendFunc would dead-letter mail the client already got a
// 250 for (ADR-21: a 2xx means the send is Posthorn's to finish).
// Keeping name = "smtp_listener" on the original listener until the
// queue drains avoids that, and the error says so.
//
// Deliberately narrow. An endpoint or listener the operator removed or
// renamed by choice keeps the existing behavior: its rows dead-letter.
// A storage read error is logged and ignored; the disk never blocks
// startup (NFR27).
func checkUnnamedListenerQueue(cfg *config.Config, store *storage.Store, transports map[string]transport.Transport, logger *slog.Logger) error {
	const name = config.DefaultSMTPListenerName
	if len(cfg.Listeners()) == 0 {
		return nil
	}
	if _, ok := transports[name]; ok {
		return nil
	}
	queued, err := store.QueuedByEndpoint()
	if err != nil {
		logger.Warn("queue_endpoint_check_failed", slog.String("error", err.Error()))
		return nil
	}
	n := queued[name]
	if n == 0 {
		return nil
	}
	return fmt.Errorf("%d queued submission(s) were accepted by the unnamed [smtp_listener] and are still waiting to be retried, "+
		"but no listener is named %q now, so they would be dropped. Set name = %q on the listener that should deliver them, "+
		"restart, and rename it once posthorn_retry_queue_depth reaches 0", n, name, name)
}

// buildSMTPIngress converts the config-package SMTPListenerConfig into
// the smtp-package ListenerConfig, constructs the outbound transport
// via the same registry the HTTP endpoints use, and returns the
// resulting smtp.Listener (which satisfies ingress.Ingress) plus the
// transport itself (the retry worker needs it to replay queued rows).
func buildSMTPIngress(c *config.SMTPListenerConfig, logger *slog.Logger, recorder *metrics.Recorder) (ingress.Ingress, transport.Transport, error) {
	tp, err := buildTransport(c.Transport)
	if err != nil {
		return nil, nil, fmt.Errorf("transport: %w", err)
	}
	// Parse max_message_size (default 1MB if unset).
	rawSize := c.MaxMessageSize
	if rawSize == "" {
		rawSize = "1MB"
	}
	maxBody, err := spam.ParseSize(rawSize)
	if err != nil {
		return nil, nil, fmt.Errorf("max_message_size: %w", err)
	}
	listenerCfg := smtp.ListenerConfig{
		Name:                    c.EffectiveName(),
		Listen:                  c.Listen,
		RequireTLS:              c.EffectiveRequireTLS(),
		TLSCert:                 c.TLSCert,
		TLSKey:                  c.TLSKey,
		ClientCertCA:            c.ClientCertCA,
		AuthRequired:            smtp.AuthMode(c.AuthRequired),
		AllowedSenders:          c.AllowedSenders,
		AllowedRecipients:       c.AllowedRecipients,
		MaxRecipientsPerSession: c.MaxRecipientsPerSession,
		MaxConnections:          c.MaxConnections,
		MaxConnectionsPerIP:     c.MaxConnectionsPerIP,
		MaxMessageSize:          rawSize,
		IdleTimeout:             c.IdleTimeout,
		Transport:               c.Transport,
	}
	listenerCfg.SMTPUsers = make([]smtp.User, len(c.SMTPUsers))
	for i, u := range c.SMTPUsers {
		listenerCfg.SMTPUsers[i] = smtp.User{Username: u.Username, Password: u.Password}
	}
	if err := listenerCfg.Validate(); err != nil {
		return nil, nil, err
	}
	ing, err := smtp.New(listenerCfg, tp, maxBody, logger, recorder)
	if err != nil {
		return nil, nil, err
	}
	return ing, tp, nil
}

// buildSMTPIngresses builds one ingress per configured listener (FR96):
// the single [smtp_listener] table or every [[smtp_listeners]] entry.
// Storage is attached when present, and each listener's transport is
// registered under the listener's name, which is also the endpoint
// label the submission log carries, so a queued replay resolves back to
// the transport that accepted it.
func buildSMTPIngresses(cfg *config.Config, logger *slog.Logger, recorder *metrics.Recorder, gate *storage.Gate, transports map[string]transport.Transport) ([]ingress.Ingress, error) {
	var out []ingress.Ingress
	for i, lc := range cfg.Listeners() {
		name := lc.EffectiveName()
		ing, tp, err := buildSMTPIngress(lc, logger, recorder)
		if err != nil {
			return nil, fmt.Errorf("build %s: %w", cfg.ListenerLabel(i), err)
		}
		if gate != nil {
			if l, ok := ing.(*smtp.Listener); ok {
				l.AttachStorage(gate)
			}
		}
		out = append(out, ing)
		transports[name] = tp
		logger.Info("smtp_listener registered",
			slog.String("listener", name),
			slog.String("listen", lc.Listen),
			slog.String("transport", lc.Transport.Type),
			slog.Int("smtp_users", len(lc.SMTPUsers)),
		)
	}
	return out, nil
}

// runIngressesUntilSignal starts each ingress in its own goroutine,
// waits for SIGTERM/SIGINT, then drains in-flight work by stopping
// every ingress at once under one 15s deadline (longer than the
// per-request 10s hard timeout from FR22 so in-flight retries can
// complete gracefully). A second signal forces immediate exit.
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
	err := stopIngresses(shutdownCtx, ingresses)
	logger.Info("posthorn stopped")
	return err
}

// stopIngresses stops every ingress concurrently and waits for all of
// them (FR96). Each Stop closes its listening socket before it waits,
// so no ingress keeps accepting work while another drains, and each one
// gets the whole deadline instead of what the ones before it left. The
// returned error joins every failure, each naming its ingress.
func stopIngresses(ctx context.Context, ingresses []ingress.Ingress) error {
	errs := make([]error, len(ingresses))
	var wg sync.WaitGroup
	for i, ing := range ingresses {
		wg.Add(1)
		go func(i int, ing ingress.Ingress) {
			defer wg.Done()
			if err := ing.Stop(ctx); err != nil {
				errs[i] = fmt.Errorf("%s ingress graceful shutdown: %w", ing.Name(), err)
			}
		}(i, ing)
	}
	wg.Wait()
	return errors.Join(errs...)
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

	// Build the SMTP ingress too (#58): its semantic checks (smtp_users
	// required, TLS cert/key readable, client-cert CA parseable) live in
	// buildSMTPIngress, not config.Load — so without this a listener-only
	// config that passes `validate` could still fail at `serve`.
	listeners := cfg.Listeners()
	for i, lc := range listeners {
		if _, _, err := buildSMTPIngress(lc, buildLogger(cfg.Logging), nil); err != nil {
			return fmt.Errorf("%s: %w", cfg.ListenerLabel(i), err)
		}
	}

	summary := fmt.Sprintf("%d endpoint(s)", len(cfg.Endpoints))
	switch {
	case cfg.SMTPListener != nil:
		summary += " + " + cfg.SMTPListener.EffectiveName()
	case len(listeners) > 0:
		parts := make([]string, len(listeners))
		for i, lc := range listeners {
			parts[i] = fmt.Sprintf("%s (%s)", lc.EffectiveName(), lc.Listen)
		}
		summary += fmt.Sprintf(" + %d smtp listener(s): %s", len(listeners), strings.Join(parts, ", "))
	}
	fmt.Printf("config OK: %s\n", summary)
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
func buildMux(cfg *config.Config, logger *slog.Logger, metricsReg *metrics.Registry, recorder *metrics.Recorder, gate *storage.Gate) (*http.ServeMux, map[string]transport.Transport, error) {
	mux := http.NewServeMux()
	transports := make(map[string]transport.Transport, len(cfg.Endpoints))

	for i, ep := range cfg.Endpoints {
		t, err := buildTransport(ep.Transport)
		if err != nil {
			return nil, nil, fmt.Errorf("endpoints[%d] (%s): transport: %w", i, ep.Path, err)
		}
		h, err := gateway.New(ep, t,
			gateway.WithLogger(logger),
			gateway.WithRecorder(recorder),
		)
		if err != nil {
			return nil, nil, fmt.Errorf("endpoints[%d] (%s): %w", i, ep.Path, err)
		}
		if gate != nil {
			h.AttachStorage(gate)
		}
		transports[ep.Path] = t
		mux.Handle(ep.Path, h)
		logger.Info("endpoint registered",
			slog.String("path", ep.Path),
			slog.String("transport", ep.Transport.Type),
			slog.Int("recipients", len(ep.To)),
		)
	}

	// FR54: /healthz — always-on liveness probe. With storage configured
	// it also reports the canary state (FR80); always 200 because a
	// degraded disk doesn't stop mail flow (NFR27).
	var storageState metrics.StorageState
	if gate != nil {
		storageState = gate.Healthy
	}
	mux.Handle("/healthz", metrics.HealthzHandlerWithStorage(storageState))
	// FR55: /metrics — Prometheus exposition. Same registry as the
	// Recorder above so all observations land in the scrape.
	mux.Handle("/metrics", metricsReg.Handler())

	return mux, transports, nil
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
