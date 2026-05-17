// Package smtp implements Posthorn's inbound SMTP listener (FR60–FR68,
// NFR22, NFR23, ADR-12, ADR-13). Internal apps that emit SMTP — Ghost's
// admin login, Gitea's notifications, legacy on-prem systems — point at
// this listener and Posthorn forwards their messages via the configured
// outbound HTTP API transport.
//
// Posthorn is NOT a mail server. It doesn't host mailboxes, doesn't act
// as an MX, doesn't do inbound receive-side spam filtering. The SMTP
// listener is an authenticated relay for known internal clients only —
// open-relay prevention (sender allowlist + recipient allowlist/cap +
// AUTH required + STARTTLS required) is the structural defense.
package smtp

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/craigmccaskill/posthorn/config"
)

// AuthMode selects which authentication shapes the listener accepts.
type AuthMode string

const (
	// AuthSMTP requires SMTP AUTH PLAIN (FR63). The default.
	AuthSMTP AuthMode = "smtp-auth"
	// AuthClientCert requires a TLS client certificate signed by the
	// configured CA (FR63, FR66).
	AuthClientCert AuthMode = "client-cert"
	// AuthEither accepts either SMTP AUTH or client-cert.
	AuthEither AuthMode = "either"
)

// User is a single SMTP AUTH PLAIN credential pair.
type User struct {
	Username string `toml:"username"`
	Password string `toml:"password"`
}

// ListenerConfig is the top-level [smtp_listener] block. When the
// operator's TOML includes this block, cmd/posthorn starts an SMTP
// ingress alongside the HTTP one.
type ListenerConfig struct {
	// Listen is the TCP address (e.g. ":2525"). Required.
	Listen string `toml:"listen"`

	// RequireTLS forces STARTTLS upgrade before AUTH/MAIL/RCPT.
	// Default true; setting to false is not recommended outside local
	// development.
	RequireTLS bool `toml:"require_tls"`

	// TLSCert / TLSKey are filesystem paths to the certificate and
	// key used for STARTTLS. Required when RequireTLS is true OR when
	// AuthRequired is "client-cert" (TLS is mandatory for cert auth).
	TLSCert string `toml:"tls_cert"`
	TLSKey  string `toml:"tls_key"`

	// ClientCertCA is the filesystem path to a PEM-encoded CA bundle
	// whose-signed client certs are accepted. Required when
	// AuthRequired is "client-cert" or "either".
	ClientCertCA string `toml:"client_cert_ca"`

	// AuthRequired selects the auth mode. Default "smtp-auth".
	AuthRequired AuthMode `toml:"auth_required"`

	// SMTPUsers is the list of valid AUTH PLAIN credential pairs.
	// Required when AuthRequired is "smtp-auth" or "either".
	SMTPUsers []User `toml:"smtp_users"`

	// AllowedSenders is the sender allowlist (FR64). Each entry is
	// either an exact address (`noreply@example.com`) or a domain
	// wildcard (`*@example.com`). Required (non-empty).
	AllowedSenders []string `toml:"allowed_senders"`

	// AllowedRecipients is the recipient allowlist (FR65). Same
	// syntax as AllowedSenders. EITHER this or MaxRecipientsPerSession
	// must be set to a meaningful bound.
	AllowedRecipients []string `toml:"allowed_recipients"`

	// MaxRecipientsPerSession is the open-relay-prevention cap on
	// RCPT TO commands per session (FR65). Default 10 when unset and
	// AllowedRecipients is empty.
	MaxRecipientsPerSession int `toml:"max_recipients_per_session"`

	// MaxMessageSize is the maximum DATA blob size (FR66). Format
	// matches the existing max_body_size shape ("1MB", "32KB", etc.).
	// Default "1MB".
	MaxMessageSize string `toml:"max_message_size"`

	// IdleTimeout closes a connection idle this long. Default 60s.
	IdleTimeout config.Duration `toml:"idle_timeout"`

	// Transport is the outbound transport block — same shape as
	// [endpoints.transport] (FR68).
	Transport config.TransportConfig `toml:"transport"`
}

// Validate runs parse-time checks on the listener config. Returns the
// first error so operators see actionable feedback. Order: listen,
// allowed_senders (most fundamental), auth shape, TLS shape, then
// detail checks.
func (c *ListenerConfig) Validate() error {
	if c.Listen == "" {
		return errors.New("smtp_listener.listen is required (e.g., \":2525\")")
	}
	if len(c.AllowedSenders) == 0 {
		return errors.New("smtp_listener.allowed_senders: required (non-empty); open-relay prevention demands a sender allowlist")
	}
	mode := c.AuthRequired
	if mode == "" {
		mode = AuthSMTP
	}
	switch mode {
	case AuthSMTP, AuthClientCert, AuthEither:
		// ok
	default:
		return fmt.Errorf("smtp_listener.auth_required: must be %q, %q, or %q; got %q",
			AuthSMTP, AuthClientCert, AuthEither, c.AuthRequired)
	}

	// AuthSMTP / AuthEither require at least one user.
	if mode == AuthSMTP || mode == AuthEither {
		if len(c.SMTPUsers) == 0 {
			return fmt.Errorf("smtp_listener.smtp_users: at least one user required when auth_required = %q", mode)
		}
		for i, u := range c.SMTPUsers {
			if strings.TrimSpace(u.Username) == "" {
				return fmt.Errorf("smtp_listener.smtp_users[%d].username: required", i)
			}
			if u.Password == "" {
				return fmt.Errorf("smtp_listener.smtp_users[%d].password: required", i)
			}
		}
	}

	// Client-cert mode requires a CA.
	if mode == AuthClientCert || mode == AuthEither {
		if c.ClientCertCA == "" {
			return fmt.Errorf("smtp_listener.client_cert_ca: required when auth_required = %q", mode)
		}
	}

	// STARTTLS / TLS-only client cert require cert + key.
	if c.RequireTLS || mode == AuthClientCert || mode == AuthEither {
		if c.TLSCert == "" {
			return errors.New("smtp_listener.tls_cert: required when require_tls=true or auth_required includes client-cert")
		}
		if c.TLSKey == "" {
			return errors.New("smtp_listener.tls_key: required when require_tls=true or auth_required includes client-cert")
		}
	}

	// Open-relay prevention (FR65): require one of allowlist or cap.
	if len(c.AllowedRecipients) == 0 && c.MaxRecipientsPerSession == 0 {
		// Both unset — set a default cap rather than leaving the
		// listener as an open relay. Operator can explicitly opt
		// into "no cap" by setting a very large value or supplying
		// an allowlist with "*".
		// Documented default: 10.
	} else if c.MaxRecipientsPerSession < 0 {
		return fmt.Errorf("smtp_listener.max_recipients_per_session: must be non-negative, got %d", c.MaxRecipientsPerSession)
	}

	if c.IdleTimeout.Std() < 0 {
		return fmt.Errorf("smtp_listener.idle_timeout: must be non-negative, got %v", c.IdleTimeout.Std())
	}

	return nil
}

// EffectiveAuthMode returns the configured AuthRequired, defaulting to
// AuthSMTP when unset.
func (c *ListenerConfig) EffectiveAuthMode() AuthMode {
	if c.AuthRequired == "" {
		return AuthSMTP
	}
	return c.AuthRequired
}

// EffectiveMaxRecipients returns the configured cap, defaulting to 10
// when unset and AllowedRecipients is also empty.
func (c *ListenerConfig) EffectiveMaxRecipients() int {
	if c.MaxRecipientsPerSession > 0 {
		return c.MaxRecipientsPerSession
	}
	if len(c.AllowedRecipients) == 0 {
		return 10 // default open-relay-prevention cap
	}
	// Allowlist is in effect; allow unlimited recipients matching it.
	return 0
}

// EffectiveIdleTimeout returns the configured idle timeout, defaulting
// to 60s when unset.
func (c *ListenerConfig) EffectiveIdleTimeout() time.Duration {
	if c.IdleTimeout.Std() == 0 {
		return 60 * time.Second
	}
	return c.IdleTimeout.Std()
}
