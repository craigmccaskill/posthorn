package smtp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/craigmccaskill/posthorn/log"
	"github.com/craigmccaskill/posthorn/metrics"
	"github.com/craigmccaskill/posthorn/transport"
)

// Listener is the inbound SMTP ingress. Owns a TCP listener and a
// goroutine per accepted connection. Implements ingress.Ingress.
type Listener struct {
	cfg       ListenerConfig
	transport transport.Transport
	maxBody   int64
	tlsConfig *tls.Config // nil when RequireTLS is false and no client-cert
	logger    *slog.Logger
	recorder  *metrics.Recorder

	mu       sync.Mutex
	listener net.Listener
	stopped  chan struct{}
	wg       sync.WaitGroup
}

// New constructs a Listener from a validated config and a transport
// instance. Returns an error if the TLS materials are configured but
// can't be loaded. maxBodySize is the parsed byte count from
// ListenerConfig.MaxMessageSize (caller parses; this package treats
// it opaquely).
func New(cfg ListenerConfig, tp transport.Transport, maxBodySize int64, logger *slog.Logger, recorder *metrics.Recorder) (*Listener, error) {
	if logger == nil {
		logger = log.Discard()
	}
	tlsCfg, err := buildTLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Listener{
		cfg:       cfg,
		transport: tp,
		maxBody:   maxBodySize,
		tlsConfig: tlsCfg,
		logger:    logger,
		recorder:  recorder,
		stopped:   make(chan struct{}),
	}, nil
}

// buildTLSConfig assembles the *tls.Config used for STARTTLS and (if
// configured) client-cert verification. Returns nil when no TLS is
// needed (RequireTLS false AND no client-cert auth).
func buildTLSConfig(cfg ListenerConfig) (*tls.Config, error) {
	mode := cfg.EffectiveAuthMode()
	needsTLS := cfg.RequireTLS || mode == AuthClientCert || mode == AuthEither
	if !needsTLS {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("smtp_listener: load TLS cert/key: %w", err)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if mode == AuthClientCert || mode == AuthEither {
		caBytes, err := os.ReadFile(cfg.ClientCertCA)
		if err != nil {
			return nil, fmt.Errorf("smtp_listener: read client_cert_ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caBytes) {
			return nil, errors.New("smtp_listener: client_cert_ca: no certificates parsed")
		}
		tlsCfg.ClientCAs = pool
		// VerifyClientCertIfGiven lets AUTH-PLAIN clients without a
		// cert through (AuthEither path); for AuthClientCert we tighten
		// the check at the session level after the handshake.
		tlsCfg.ClientAuth = tls.VerifyClientCertIfGiven
	}
	return tlsCfg, nil
}

// Name returns "smtp" (ingress.Ingress interface).
func (l *Listener) Name() string { return "smtp" }

// Start opens the TCP listener and accepts connections until Stop is
// called. Returns nil on graceful shutdown.
func (l *Listener) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", l.cfg.Listen)
	if err != nil {
		return fmt.Errorf("smtp listen: %w", err)
	}
	l.mu.Lock()
	l.listener = ln
	l.mu.Unlock()

	l.logger.Info("smtp ingress listening", slog.String("addr", l.cfg.Listen))

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-l.stopped:
				return nil // graceful stop
			default:
				if errors.Is(err, net.ErrClosed) {
					return nil
				}
				return fmt.Errorf("smtp accept: %w", err)
			}
		}
		l.wg.Add(1)
		go func(c net.Conn) {
			defer l.wg.Done()
			l.handleConnection(c)
		}(conn)
	}
}

// Stop signals the accept loop to return and waits for in-flight
// sessions to complete (bounded by ctx).
func (l *Listener) Stop(ctx context.Context) error {
	l.mu.Lock()
	if l.listener != nil {
		_ = l.listener.Close()
	}
	close(l.stopped)
	l.mu.Unlock()

	// Wait for in-flight sessions with the context's deadline.
	done := make(chan struct{})
	go func() {
		l.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		l.logger.Info("smtp ingress stopped")
		return nil
	case <-ctx.Done():
		l.logger.Warn("smtp ingress shutdown deadline exceeded; in-flight sessions abandoned")
		return ctx.Err()
	}
}

// handleConnection sets up the session struct and runs the state
// machine. Closes the connection on return.
func (l *Listener) handleConnection(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	sess := newSession(conn, l)
	sess.run()
}

// idleDeadline returns the absolute time at which an idle connection
// should be timed out, given now and the configured idle timeout.
func (l *Listener) idleDeadline(now time.Time) time.Time {
	return now.Add(l.cfg.EffectiveIdleTimeout())
}
