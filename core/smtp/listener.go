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

	"github.com/craigmccaskill/posthorn/config"
	"github.com/craigmccaskill/posthorn/log"
	"github.com/craigmccaskill/posthorn/metrics"
	"github.com/craigmccaskill/posthorn/ratelimit"
	"github.com/craigmccaskill/posthorn/storage"
	"github.com/craigmccaskill/posthorn/transport"
)

// AUTH brute-force budget — token bucket per remote IP, mirroring the
// HTTP api-mode defense (gateway's defaultAuthFailureBudget). Each
// failed AUTH consumes a token; an exhausted bucket gets 421 and the
// connection closes. Vars (not consts) so tests can override; #50.
var (
	authFailureBudget   = 10
	authFailureInterval = time.Minute
)

// Listener is the inbound SMTP ingress. Owns a TCP listener and a
// goroutine per accepted connection. Implements ingress.Ingress.
type Listener struct {
	cfg ListenerConfig
	// name is cfg.Name resolved once (FR95): the metrics `endpoint` label,
	// the submission-log endpoint, and the `listener` log field. Every
	// call site reads this rather than re-deriving the default.
	name      string
	transport transport.Transport
	maxBody   int64
	tlsConfig *tls.Config // nil when RequireTLS is false and no client-cert
	logger    *slog.Logger
	recorder  *metrics.Recorder
	gate      *storage.Gate // nil = no [storage]; v1.x behavior (FR77)

	mu       sync.Mutex
	listener net.Listener
	stopped  chan struct{}
	wg       sync.WaitGroup

	// authFail is the per-IP AUTH brute-force limiter (#50). Never nil.
	authFail *ratelimit.Limiter

	// Connection accounting (#50): global + per-IP concurrent caps,
	// enforced in the accept loop before a session goroutine spawns.
	connMu      sync.Mutex
	activeConns int
	perIPConns  map[string]int
}

// New constructs a Listener from a validated config and a transport
// instance. Returns an error if the TLS materials are configured but
// can't be loaded. maxBodySize is the parsed byte count from
// ListenerConfig.MaxMessageSize (caller parses; this package treats
// it opaquely).
// AttachStorage wires the optional storage gate (FR76/FR77). Called by
// cmd/posthorn when a [storage] block is configured. Nil-safe: without
// it the listener is v1.x.
func (l *Listener) AttachStorage(gate *storage.Gate) {
	l.gate = gate
}

func New(cfg ListenerConfig, tp transport.Transport, maxBodySize int64, logger *slog.Logger, recorder *metrics.Recorder) (*Listener, error) {
	if logger == nil {
		logger = log.Discard()
	}
	name := cfg.Name
	if name == "" {
		name = config.DefaultSMTPListenerName
	}
	// FR95: every line this listener logs names it, so two listeners in
	// one process can be told apart.
	logger = logger.With(slog.String("listener", name))
	tlsCfg, err := buildTLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	authFail, err := ratelimit.New(authFailureBudget, authFailureInterval, 0)
	if err != nil {
		return nil, fmt.Errorf("auth-failure limiter: %w", err)
	}
	return &Listener{
		cfg:        cfg,
		name:       name,
		transport:  tp,
		maxBody:    maxBodySize,
		tlsConfig:  tlsCfg,
		logger:     logger,
		recorder:   recorder,
		stopped:    make(chan struct{}),
		authFail:   authFail,
		perIPConns: make(map[string]int),
	}, nil
}

// acquireConnSlot reserves a connection slot for remoteIP, enforcing
// the global and per-IP caps. Returns false when either cap is hit.
func (l *Listener) acquireConnSlot(remoteIP string) bool {
	l.connMu.Lock()
	defer l.connMu.Unlock()
	if l.activeConns >= l.cfg.EffectiveMaxConnections() {
		return false
	}
	if l.perIPConns[remoteIP] >= l.cfg.EffectiveMaxConnectionsPerIP() {
		return false
	}
	l.activeConns++
	l.perIPConns[remoteIP]++
	return true
}

// releaseConnSlot returns a slot reserved by acquireConnSlot.
func (l *Listener) releaseConnSlot(remoteIP string) {
	l.connMu.Lock()
	defer l.connMu.Unlock()
	l.activeConns--
	if n := l.perIPConns[remoteIP] - 1; n > 0 {
		l.perIPConns[remoteIP] = n
	} else {
		delete(l.perIPConns, remoteIP)
	}
}

// authFailureExceeded records one AUTH failure for remoteIP and reports
// whether the budget is now exhausted (#50). Successful auths never
// consume budget — mirrors the HTTP api-mode brute-force defense.
func (l *Listener) authFailureExceeded(remoteIP string) bool {
	return !l.authFail.Allow(remoteIP)
}

// remoteIPOf extracts the host part of a connection's remote address.
func remoteIPOf(conn net.Conn) string {
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return conn.RemoteAddr().String()
	}
	return host
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
		return nil, fmt.Errorf("load TLS cert/key: %w", err)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if mode == AuthClientCert || mode == AuthEither {
		caBytes, err := os.ReadFile(cfg.ClientCertCA)
		if err != nil {
			return nil, fmt.Errorf("read client_cert_ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caBytes) {
			return nil, errors.New("client_cert_ca: no certificates parsed")
		}
		tlsCfg.ClientCAs = pool
		// VerifyClientCertIfGiven lets AUTH-PLAIN clients without a
		// cert through (AuthEither path); for AuthClientCert we tighten
		// the check at the session level after the handshake.
		tlsCfg.ClientAuth = tls.VerifyClientCertIfGiven
	}
	return tlsCfg, nil
}

// Name identifies the ingress in startup and shutdown errors
// (ingress.Ingress interface): "smtp" for a lone unnamed listener, and
// the listener's own name alongside it otherwise, so a bind failure or a
// drain timeout says which of several listeners it was.
func (l *Listener) Name() string {
	if l.name == config.DefaultSMTPListenerName {
		return "smtp"
	}
	return fmt.Sprintf("smtp %q", l.name)
}

// Addr returns the address the listener is bound to, or nil before
// Start has bound it. With a ":0" listen address this is the only way
// to learn the port.
func (l *Listener) Addr() net.Addr {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.listener == nil {
		return nil
	}
	return l.listener.Addr()
}

// Start opens the TCP listener and accepts connections until Stop is
// called. Returns nil on graceful shutdown.
func (l *Listener) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", l.cfg.Listen)
	if err != nil {
		return fmt.Errorf("smtp listen: %w", err)
	}
	l.mu.Lock()
	select {
	case <-l.stopped:
		// Stop ran before the bind. It had no socket to close, so close
		// this one here; otherwise Accept would block with nothing left
		// to wake it.
		l.mu.Unlock()
		_ = ln.Close()
		return nil
	default:
	}
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

	// #50: enforce the global and per-IP concurrent-connection caps
	// before any protocol work. Refused connections get a one-line 421
	// so well-behaved clients back off and retry.
	remoteIP := remoteIPOf(conn)
	if !l.acquireConnSlot(remoteIP) {
		// Bound the refusal write: a zero-window client under exactly the
		// flood this cap defends against must not block this goroutine
		// (it is counted in l.wg, so a hang would stall Stop's drain).
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, _ = conn.Write([]byte("421 4.7.0 Too many connections, try again later\r\n"))
		l.logger.Info("smtp_connection_refused",
			slog.String("remote_ip", remoteIP),
			slog.String("reason", "connection_cap"),
		)
		return
	}
	defer l.releaseConnSlot(remoteIP)

	sess := newSession(conn, l)
	sess.run()
}

// idleDeadline returns the absolute time at which an idle connection
// should be timed out, given now and the configured idle timeout.
func (l *Listener) idleDeadline(now time.Time) time.Time {
	return now.Add(l.cfg.EffectiveIdleTimeout())
}
