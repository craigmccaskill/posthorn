package posthorn_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"

	pconfig "github.com/craigmccaskill/posthorn/config"
	posthorn "github.com/craigmccaskill/posthorn/caddy"
)

// minimalCaddyfile mirrors minimalTOML from core/config tests — the
// smallest valid directive used as a baseline.
const minimalCaddyfile = `
posthorn /api/contact {
	to alice@example.com
	from noreply@example.com
	subject Contact
	body Body
	transport postmark {
		api_key test-key
	}
}
`

func parse(t *testing.T, input string) *posthorn.Handler {
	t.Helper()
	d := caddyfile.NewTestDispenser(input)
	var h posthorn.Handler
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile: %v", err)
	}
	return &h
}

func TestUnmarshalCaddyfile_Minimal(t *testing.T) {
	h := parse(t, minimalCaddyfile)
	if h.Path != "/api/contact" {
		t.Errorf("Path = %q, want /api/contact", h.Path)
	}
	if !reflect.DeepEqual(h.To, []string{"alice@example.com"}) {
		t.Errorf("To = %v, want [alice@example.com]", h.To)
	}
	if h.From != "noreply@example.com" {
		t.Errorf("From = %q, want noreply@example.com", h.From)
	}
	if h.Subject != "Contact" {
		t.Errorf("Subject = %q, want Contact", h.Subject)
	}
	if h.Body != "Body" {
		t.Errorf("Body = %q, want Body", h.Body)
	}
	if h.Transport.Type != "postmark" {
		t.Errorf("Transport.Type = %q, want postmark", h.Transport.Type)
	}
	if got, _ := h.Transport.Settings["api_key"].(string); got != "test-key" {
		t.Errorf("Transport.Settings[api_key] = %q, want test-key", got)
	}
}

func TestUnmarshalCaddyfile_FullDirective(t *testing.T) {
	input := `
posthorn /api/contact {
	to alice@example.com bob@example.com
	from "Contact <noreply@example.com>"
	required name email message
	email_field email
	honeypot _gotcha
	allowed_origins https://example.com https://www.example.com
	max_body_size 64KB
	trusted_proxies 10.0.0.0/8 192.168.0.0/16
	subject "Contact from {{.name}}"
	body templates/contact.txt
	redirect_success /thank-you
	redirect_error /contact?error=1
	log_failed_submissions false
	rate_limit 5 1m
	transport postmark {
		api_key test-key
	}
}
`
	h := parse(t, input)

	tests := []struct {
		name string
		got  any
		want any
	}{
		{"Path", h.Path, "/api/contact"},
		{"To", h.To, []string{"alice@example.com", "bob@example.com"}},
		{"From", h.From, "Contact <noreply@example.com>"},
		{"Required", h.Required, []string{"name", "email", "message"}},
		{"EmailField", h.EmailField, "email"},
		{"Honeypot", h.Honeypot, "_gotcha"},
		{"AllowedOrigins", h.AllowedOrigins, []string{"https://example.com", "https://www.example.com"}},
		{"MaxBodySize", h.MaxBodySize, "64KB"},
		{"TrustedProxies", h.TrustedProxies, []string{"10.0.0.0/8", "192.168.0.0/16"}},
		{"Subject", h.Subject, "Contact from {{.name}}"},
		{"Body", h.Body, "templates/contact.txt"},
		{"RedirectSuccess", h.RedirectSuccess, "/thank-you"},
		{"RedirectError", h.RedirectError, "/contact?error=1"},
	}
	for _, tt := range tests {
		if !reflect.DeepEqual(tt.got, tt.want) {
			t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
		}
	}

	if h.LogFailedSubmissions == nil || *h.LogFailedSubmissions != false {
		t.Errorf("LogFailedSubmissions = %v, want *bool(false)", h.LogFailedSubmissions)
	}

	if h.RateLimit == nil {
		t.Fatal("RateLimit is nil")
	}
	if h.RateLimit.Count != 5 {
		t.Errorf("RateLimit.Count = %d, want 5", h.RateLimit.Count)
	}
	if h.RateLimit.Interval.Std() != time.Minute {
		t.Errorf("RateLimit.Interval = %v, want 1m", h.RateLimit.Interval.Std())
	}
}

func TestUnmarshalCaddyfile_LogFailedSubmissionsTrue(t *testing.T) {
	input := `
posthorn /api/x {
	to a@b
	from c@d
	subject S
	body B
	transport postmark { api_key k }
	log_failed_submissions true
}
`
	h := parse(t, input)
	if h.LogFailedSubmissions == nil || *h.LogFailedSubmissions != true {
		t.Errorf("LogFailedSubmissions = %v, want *bool(true)", h.LogFailedSubmissions)
	}
}

func TestUnmarshalCaddyfile_Errors(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string // substring expected in the error
	}{
		{
			name:  "missing path",
			input: `posthorn { to a@b }`,
			want:  "Wrong argument count",
		},
		{
			name:  "extra path arg",
			input: `posthorn /one /two { to a@b }`,
			want:  "expected exactly one path argument",
		},
		{
			name: "unknown subdirective",
			input: `posthorn /x {
	wat foo
}`,
			want: "unknown subdirective",
		},
		{
			name: "bad rate_limit count",
			input: `posthorn /x {
	rate_limit abc 1m
}`,
			want: "rate_limit count",
		},
		{
			name: "bad rate_limit interval",
			input: `posthorn /x {
	rate_limit 5 nope
}`,
			want: "rate_limit interval",
		},
		{
			name: "bad log_failed_submissions value",
			input: `posthorn /x {
	log_failed_submissions maybe
}`,
			want: "expected \"true\" or \"false\"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := caddyfile.NewTestDispenser(tc.input)
			var h posthorn.Handler
			err := h.UnmarshalCaddyfile(d)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

// TestParityWithTOML is the load-bearing parity test for Story 6.2's
// "single source of truth" claim. Both Caddyfile and TOML are parsed
// into the equivalent EndpointConfig (via the adapter's translation
// for Caddyfile, via core/config.Load for TOML); the two configs are
// then deep-equal compared.
//
// The test is intentionally explicit about the equivalent inputs
// rather than fixture-driven: a future reviewer should be able to
// eyeball both representations side-by-side and confirm parity.
func TestParityWithTOML(t *testing.T) {
	caddyfileInput := `
posthorn /api/contact {
	to alice@example.com bob@example.com
	from "Contact <noreply@example.com>"
	required name email message
	honeypot _gotcha
	allowed_origins https://example.com
	max_body_size 32KB
	subject "Contact: {{.name}}"
	body "From {{.name}}\n\n{{.message}}"
	rate_limit 5 1m
	transport postmark {
		api_key test-key
	}
}
`

	tomlInput := `
[[endpoints]]
path = "/api/contact"
to = ["alice@example.com", "bob@example.com"]
from = "Contact <noreply@example.com>"
required = ["name", "email", "message"]
honeypot = "_gotcha"
allowed_origins = ["https://example.com"]
max_body_size = "32KB"
subject = "Contact: {{.name}}"
body = "From {{.name}}\n\n{{.message}}"

[endpoints.rate_limit]
count = 5
interval = "1m"

[endpoints.transport]
type = "postmark"

[endpoints.transport.settings]
api_key = "test-key"
`

	// Parse the Caddyfile side.
	d := caddyfile.NewTestDispenser(caddyfileInput)
	var h posthorn.Handler
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("Caddyfile parse: %v", err)
	}
	caddyEP := pconfig.EndpointConfig{
		Path:                 h.Path,
		To:                   h.To,
		From:                 h.From,
		Transport:            h.Transport,
		RateLimit:            h.RateLimit,
		TrustedProxies:       h.TrustedProxies,
		Honeypot:             h.Honeypot,
		AllowedOrigins:       h.AllowedOrigins,
		MaxBodySize:          h.MaxBodySize,
		Required:             h.Required,
		EmailField:           h.EmailField,
		Subject:              h.Subject,
		Body:                 h.Body,
		LogFailedSubmissions: h.LogFailedSubmissions,
		RedirectSuccess:      h.RedirectSuccess,
		RedirectError:        h.RedirectError,
	}

	// Parse the TOML side via the real loader.
	tmpDir := t.TempDir()
	tomlPath := filepath.Join(tmpDir, "config.toml")
	if err := os.WriteFile(tomlPath, []byte(tomlInput), 0o600); err != nil {
		t.Fatalf("write toml: %v", err)
	}
	cfg, err := pconfig.Load(tomlPath)
	if err != nil {
		t.Fatalf("TOML load: %v", err)
	}
	if len(cfg.Endpoints) != 1 {
		t.Fatalf("expected 1 endpoint from TOML, got %d", len(cfg.Endpoints))
	}
	tomlEP := cfg.Endpoints[0]

	if !reflect.DeepEqual(caddyEP, tomlEP) {
		t.Errorf("Caddyfile and TOML produced different EndpointConfigs:\n  caddy: %+v\n  toml:  %+v", caddyEP, tomlEP)
	}
}
