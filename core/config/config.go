// Package config defines the Posthorn configuration schema and TOML loader.
//
// Configuration flows: TOML file → resolveEnvVars → toml.Unmarshal → Validate.
// The Config struct is the single source of truth for runtime behavior.
// Validation runs at load time (FR24) so operators get fast feedback
// rather than runtime surprises.
package config

import (
	"errors"
	"fmt"
	"net/mail"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/craigmccaskill/posthorn/csrf"
	"github.com/craigmccaskill/posthorn/transport"
)

// Config is the top-level configuration object.
type Config struct {
	Endpoints    []EndpointConfig    `toml:"endpoints"`
	Logging      LoggingConfig       `toml:"logging"`
	SMTPListener *SMTPListenerConfig `toml:"smtp_listener"`
}

// SMTPListenerConfig is the top-level [smtp_listener] block (FR62).
// When non-nil after parse, cmd/posthorn starts an SMTP ingress
// alongside the HTTP one. Lives in this package to avoid a config →
// smtp circular dependency; the smtp package converts it to its
// internal ListenerConfig shape.
type SMTPListenerConfig struct {
	Listen                  string              `toml:"listen"`
	RequireTLS              bool                `toml:"require_tls"`
	TLSCert                 string              `toml:"tls_cert"`
	TLSKey                  string              `toml:"tls_key"`
	ClientCertCA            string              `toml:"client_cert_ca"`
	AuthRequired            string              `toml:"auth_required"`
	SMTPUsers               []SMTPUser          `toml:"smtp_users"`
	AllowedSenders          []string            `toml:"allowed_senders"`
	AllowedRecipients       []string            `toml:"allowed_recipients"`
	MaxRecipientsPerSession int                 `toml:"max_recipients_per_session"`
	MaxMessageSize          string              `toml:"max_message_size"`
	IdleTimeout             Duration            `toml:"idle_timeout"`
	Transport               TransportConfig     `toml:"transport"`
}

// SMTPUser is a single AUTH PLAIN credential pair.
type SMTPUser struct {
	Username string `toml:"username"`
	Password string `toml:"password"`
}

// Auth mode values for EndpointConfig.Auth. Empty / unset is equivalent to
// AuthForm (FR45 — v1.0 configs unchanged).
const (
	AuthForm   = "form"
	AuthAPIKey = "api-key"
)

// EndpointConfig configures one ingress endpoint. Multiple endpoints in one
// Config are independent — no shared rate-limit budget, no cross-endpoint
// state (FR2).
//
// Two modes:
//   - Form mode (default; v1.0 behavior): browser POSTs form-encoded bodies,
//     defended by honeypot / Origin / rate limit / max-body-size.
//   - API-key mode (v1.1): server-to-server callers POST JSON bodies with
//     Authorization: Bearer <key>; browser defenses do not apply (FR31, FR32).
type EndpointConfig struct {
	Path                 string           `toml:"path"`
	To                   []string         `toml:"to"`
	From                 string           `toml:"from"`
	Transport            TransportConfig  `toml:"transport"`
	RateLimit            *RateLimitConfig `toml:"rate_limit"`
	TrustedProxies       []string         `toml:"trusted_proxies"`
	Honeypot             string           `toml:"honeypot"`
	AllowedOrigins       []string         `toml:"allowed_origins"`
	MaxBodySize          string           `toml:"max_body_size"` // e.g. "32KB"; parsed at handler-construction time
	Required             []string         `toml:"required"`
	EmailField           string           `toml:"email_field"`
	ReplyToEmailField    string           `toml:"reply_to_email_field"`
	Subject              string           `toml:"subject"`
	Body                 string           `toml:"body"`
	LogFailedSubmissions *bool            `toml:"log_failed_submissions"`
	RedirectSuccess      string           `toml:"redirect_success"`
	RedirectError        string           `toml:"redirect_error"`

	// v1.1: API mode. Auth selects the endpoint shape; empty defaults to
	// AuthForm preserving v1.0 behavior (FR31, FR45). APIKeys is the list
	// of valid Bearer tokens when Auth is AuthAPIKey (FR33). Multiple keys
	// support rotation. IdempotencyCacheSize sets the per-endpoint cache
	// capacity (FR42); zero or unset means use the package default.
	Auth                 string   `toml:"auth"`
	APIKeys              []string `toml:"api_keys"`
	IdempotencyCacheSize int      `toml:"idempotency_cache_size"`

	// v1.0 block C: dry-run mode (FR56). When true, the handler runs the
	// full pipeline up to but not including transport.Send and returns
	// 200 with a JSON body containing the prepared transport.Message.
	// Operators use this to debug template rendering and recipient
	// resolution without sending mail.
	DryRun bool `toml:"dry_run"`

	// v1.0 block C: GDPR-shaped IP-stripping option (FR59). When true,
	// the resolved client IP is omitted from all log lines for this
	// endpoint. Rate-limit keying is unaffected — IP is still computed
	// internally; it just doesn't reach the log surface.
	StripClientIP bool `toml:"strip_client_ip"`

	// v1.0 block C: CSRF (FR57, ADR-16). When CSRFSecret is non-empty,
	// the handler requires a `_csrf_token` form field on every form-mode
	// submission and verifies its HMAC-SHA256 signature against
	// CSRFSecret. Tokens older than CSRFTokenTTL (default 1h) are
	// rejected with 403. Form-mode only — api-mode endpoints reject
	// csrf_secret at parse time.
	CSRFSecret   string   `toml:"csrf_secret"`
	CSRFTokenTTL Duration `toml:"csrf_token_ttl"`
}

// TransportConfig is the polymorphic transport block. Type names a concrete
// transport; Settings is transport-specific. v1.0 supports only "postmark".
// New transports in v1.1+ (resend, mailgun, ses, smtp) extend the Type
// switch in Validate without breaking config compatibility.
type TransportConfig struct {
	Type     string         `toml:"type"`
	Settings map[string]any `toml:"settings"`
}

// RateLimitConfig is the per-endpoint token-bucket configuration.
type RateLimitConfig struct {
	Count    int      `toml:"count"`
	Interval Duration `toml:"interval"`
}

// LoggingConfig is the global logging configuration.
type LoggingConfig struct {
	Level  string `toml:"level"`  // debug | info | warn | error (default info)
	Format string `toml:"format"` // json (only json supported in v1.0)
}

// Duration wraps time.Duration with TOML support. BurntSushi/toml does not
// natively unmarshal time.Duration, so we provide a TextUnmarshaler.
type Duration time.Duration

// UnmarshalText parses a duration string like "1m", "30s", "1h30m".
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// Std returns the underlying time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Load reads a TOML config file from path, resolves ${env.VAR} placeholders,
// parses the TOML, and runs validation. Returns the validated Config or an
// error describing the first problem encountered.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	resolved, err := resolveEnvVars(raw)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	var cfg Config
	if err := toml.Unmarshal(resolved, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse TOML: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	return &cfg, nil
}

// envVarPattern matches ${env.VARNAME} placeholders. Variable names must
// be UPPER_SNAKE_CASE per POSIX env-var conventions; this also reduces
// false matches in body templates and other user-controlled strings.
var envVarPattern = regexp.MustCompile(`\$\{env\.([A-Z_][A-Z0-9_]*)\}`)

// resolveEnvVars replaces ${env.VAR} placeholders with os.Getenv values.
// Reports all missing variables in a single error so operators don't play
// whack-a-mole on first run.
func resolveEnvVars(raw []byte) ([]byte, error) {
	var missing []string
	seen := map[string]bool{}
	out := envVarPattern.ReplaceAllFunc(raw, func(match []byte) []byte {
		sub := envVarPattern.FindSubmatch(match)
		name := string(sub[1])
		val, ok := os.LookupEnv(name)
		if !ok {
			if !seen[name] {
				missing = append(missing, name)
				seen[name] = true
			}
			return match // leave as-is; caller will surface the error
		}
		return []byte(val)
	})
	if len(missing) > 0 {
		return nil, fmt.Errorf("env var(s) not set: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// Validate checks structural and semantic constraints on a parsed Config.
// Returns the first error; runs cheap checks before expensive ones.
func (c *Config) Validate() error {
	if len(c.Endpoints) == 0 {
		return errors.New("at least one endpoint required")
	}

	seenPaths := map[string]bool{}
	for i, ep := range c.Endpoints {
		if err := ep.Validate(); err != nil {
			return fmt.Errorf("endpoints[%d] (%s): %w", i, ep.Path, err)
		}
		if seenPaths[ep.Path] {
			return fmt.Errorf("duplicate endpoint path: %s", ep.Path)
		}
		seenPaths[ep.Path] = true
	}

	if c.Logging.Format != "" && c.Logging.Format != "json" {
		return fmt.Errorf("logging.format: only \"json\" supported in v1.0, got %q", c.Logging.Format)
	}
	switch c.Logging.Level {
	case "", "debug", "info", "warn", "error":
		// ok
	default:
		return fmt.Errorf("logging.level: must be one of debug|info|warn|error, got %q", c.Logging.Level)
	}

	if c.SMTPListener != nil {
		if err := c.SMTPListener.Validate(); err != nil {
			return fmt.Errorf("smtp_listener: %w", err)
		}
	}

	return nil
}

// Validate runs structural checks on the SMTP listener block. The
// detailed semantic checks (auth/cert combinations) live in the smtp
// package's own Validate; here we just confirm required fields are
// present and the transport block is well-formed.
func (s *SMTPListenerConfig) Validate() error {
	if s.Listen == "" {
		return errors.New("listen is required (e.g., \":2525\")")
	}
	if len(s.AllowedSenders) == 0 {
		return errors.New("allowed_senders: at least one entry required")
	}
	if err := s.Transport.Validate(); err != nil {
		return fmt.Errorf("transport: %w", err)
	}
	if s.MaxRecipientsPerSession < 0 {
		return fmt.Errorf("max_recipients_per_session: must be non-negative, got %d", s.MaxRecipientsPerSession)
	}
	if s.IdleTimeout.Std() < 0 {
		return fmt.Errorf("idle_timeout: must be non-negative, got %v", s.IdleTimeout.Std())
	}
	return nil
}

// Validate checks one endpoint's configuration.
func (e *EndpointConfig) Validate() error {
	if e.Path == "" {
		return errors.New("path is required")
	}
	if !strings.HasPrefix(e.Path, "/") {
		return fmt.Errorf("path must start with /: %q", e.Path)
	}

	if len(e.To) == 0 {
		return errors.New("to is required (one or more recipient addresses)")
	}
	for _, addr := range e.To {
		if _, err := mail.ParseAddress(addr); err != nil {
			return fmt.Errorf("to: invalid email %q: %w", addr, err)
		}
	}

	if e.From == "" {
		return errors.New("from is required")
	}
	if _, err := mail.ParseAddress(e.From); err != nil {
		return fmt.Errorf("from: invalid email %q: %w", e.From, err)
	}

	if e.Subject == "" {
		return errors.New("subject is required")
	}
	if e.Body == "" {
		return errors.New("body is required")
	}

	if err := e.Transport.Validate(); err != nil {
		return fmt.Errorf("transport: %w", err)
	}

	// FR31: resolve effective auth mode. Empty / unset defaults to form.
	auth := e.Auth
	if auth == "" {
		auth = AuthForm
	}
	switch auth {
	case AuthForm, AuthAPIKey:
		// valid
	default:
		return fmt.Errorf("auth: must be %q or %q, got %q", AuthForm, AuthAPIKey, e.Auth)
	}

	if auth == AuthAPIKey {
		// FR33: api-mode requires non-empty api_keys.
		if len(e.APIKeys) == 0 {
			return errors.New("api_keys: at least one key required when auth = \"api-key\"")
		}
		for i, k := range e.APIKeys {
			if strings.TrimSpace(k) == "" {
				return fmt.Errorf("api_keys[%d]: empty key", i)
			}
		}
		// FR32: api-mode rejects form-mode browser defenses at parse time
		// (ADR-10). Silent ignore would let an operator think they were
		// protected when they weren't.
		if e.Honeypot != "" {
			return errors.New("honeypot: not valid on auth=\"api-key\" endpoints (api-mode is authenticated; browser bot defenses do not apply)")
		}
		if e.AllowedOrigins != nil {
			return errors.New("allowed_origins: not valid on auth=\"api-key\" endpoints (api-mode is authenticated; browser CORS defenses do not apply)")
		}
		if e.RedirectSuccess != "" {
			return errors.New("redirect_success: not valid on auth=\"api-key\" endpoints (servers do not follow redirects in this flow)")
		}
		if e.RedirectError != "" {
			return errors.New("redirect_error: not valid on auth=\"api-key\" endpoints (servers do not follow redirects in this flow)")
		}
		if e.CSRFSecret != "" {
			return errors.New("csrf_secret: not valid on auth=\"api-key\" endpoints (api-mode callers are server-to-server; CSRF defense is form-mode only)")
		}
		// FR42: idempotency_cache_size must be positive when set; zero
		// means "use the default" (handler-side resolution).
		if e.IdempotencyCacheSize < 0 {
			return fmt.Errorf("idempotency_cache_size: must be non-negative, got %d", e.IdempotencyCacheSize)
		}
	} else {
		// Form mode: api-mode-only fields must be unset. Catches the
		// "operator forgot to set auth = api-key" misconfiguration.
		if e.IdempotencyCacheSize != 0 {
			return errors.New("idempotency_cache_size: only valid on auth=\"api-key\" endpoints")
		}
		if len(e.APIKeys) > 0 {
			return errors.New("api_keys: must be unset unless auth = \"api-key\"")
		}
		// FR57: form-mode CSRF. When csrf_secret is set, validate it
		// passes the minimum-length check. csrf_token_ttl must be
		// positive when set; zero means "use the default" at handler-
		// construction time.
		if e.CSRFSecret != "" {
			if err := csrf.ValidateSecret([]byte(e.CSRFSecret)); err != nil {
				return err
			}
		}
		if e.CSRFTokenTTL.Std() < 0 {
			return fmt.Errorf("csrf_token_ttl: must be non-negative, got %v", e.CSRFTokenTTL.Std())
		}
	}

	// NFR4: explicitly-empty allowed_origins is a misconfiguration.
	// BurntSushi/toml leaves the slice nil when the key is absent and
	// returns a non-nil empty slice when the operator wrote `allowed_origins = []`.
	// (Already rejected above for api-mode; this catches form-mode.)
	if e.AllowedOrigins != nil && len(e.AllowedOrigins) == 0 {
		return errors.New("allowed_origins is explicitly empty; either remove the key (to allow all origins) or list at least one origin")
	}

	if e.RateLimit != nil {
		if e.RateLimit.Count <= 0 {
			return fmt.Errorf("rate_limit.count must be positive, got %d", e.RateLimit.Count)
		}
		if e.RateLimit.Interval.Std() <= 0 {
			return fmt.Errorf("rate_limit.interval must be positive, got %v", e.RateLimit.Interval.Std())
		}
	}

	return nil
}

// Validate checks transport configuration for the declared type. Dispatch
// is via the transport package's registry — each transport (postmark,
// resend, mailgun, ses, smtp-out) registers its validator at init.
// Adding a new transport requires no edits here.
func (t *TransportConfig) Validate() error {
	if t.Type == "" {
		return errors.New("type is required (e.g., \"postmark\")")
	}
	reg, ok := transport.Lookup(t.Type)
	if !ok {
		return transport.UnknownTypeError(t.Type)
	}
	return reg.Validate(t.Settings)
}
