// Package config defines the Posthorn configuration schema and TOML loader.
//
// Configuration flows: TOML file → resolveEnvVars → toml.Unmarshal → Validate.
// Both deployment shapes (standalone binary and Caddy adapter) end up with
// the same Config struct, which is the single source of truth for runtime
// behavior. Validation runs at load time (FR24) so operators get fast
// feedback rather than runtime surprises.
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
)

// Config is the top-level configuration object.
type Config struct {
	Endpoints []EndpointConfig `toml:"endpoints"`
	Logging   LoggingConfig    `toml:"logging"`
}

// EndpointConfig configures one HTTP form-ingress endpoint. Multiple
// endpoints in one Config are independent — no shared rate-limit budget,
// no cross-endpoint state (FR2).
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
	Subject              string           `toml:"subject"`
	Body                 string           `toml:"body"`
	LogFailedSubmissions *bool            `toml:"log_failed_submissions"`
	RedirectSuccess      string           `toml:"redirect_success"`
	RedirectError        string           `toml:"redirect_error"`
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

	// NFR4: explicitly-empty allowed_origins is a misconfiguration.
	// BurntSushi/toml leaves the slice nil when the key is absent and
	// returns a non-nil empty slice when the operator wrote `allowed_origins = []`.
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

// Validate checks transport configuration for the declared type.
func (t *TransportConfig) Validate() error {
	if t.Type == "" {
		return errors.New("type is required (e.g., \"postmark\")")
	}

	switch t.Type {
	case "postmark":
		apiKey, ok := t.Settings["api_key"].(string)
		if !ok || apiKey == "" {
			return errors.New("postmark transport requires settings.api_key")
		}
	default:
		return fmt.Errorf("unknown transport type %q (v1.0 supports: postmark)", t.Type)
	}

	return nil
}
