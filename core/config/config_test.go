package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/craigmccaskill/posthorn/config"
)

// minimalTOML is the smallest valid config used as a baseline across many
// tests. Tests that need to break a specific field rebuild the string
// rather than mutating shared state, so each test stays independent.
const minimalTOML = `
[[endpoints]]
path = "/api/contact"
to = ["craig@example.com"]
from = "noreply@example.com"
subject = "Contact"
body = "Body"

[endpoints.transport]
type = "postmark"

[endpoints.transport.settings]
api_key = "test-key"
`

// writeConfig writes content to a temp file and returns the path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// loadString is a helper for tests that want to pass TOML inline rather
// than thread a temp-file path through every assertion.
func loadString(t *testing.T, content string) (*config.Config, error) {
	t.Helper()
	return config.Load(writeConfig(t, content))
}

func TestLoad_Success_MinimalConfig(t *testing.T) {
	cfg, err := loadString(t, minimalTOML)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := len(cfg.Endpoints); got != 1 {
		t.Fatalf("Endpoints length = %d, want 1", got)
	}
	ep := cfg.Endpoints[0]
	if ep.Path != "/api/contact" {
		t.Errorf("Path = %q, want %q", ep.Path, "/api/contact")
	}
	if got := ep.Transport.Type; got != "postmark" {
		t.Errorf("Transport.Type = %q", got)
	}
	if got, _ := ep.Transport.Settings["api_key"].(string); got != "test-key" {
		t.Errorf("api_key = %q", got)
	}
}

func TestLoad_Success_FullConfig(t *testing.T) {
	full := `
[[endpoints]]
path = "/api/contact"
to = ["craig@example.com", "support@example.com"]
from = "Contact <noreply@example.com>"
trusted_proxies = ["10.0.0.0/8"]
honeypot = "_gotcha"
allowed_origins = ["https://example.com"]
max_body_size = "32KB"
required = ["name", "email", "message"]
email_field = "email"
subject = "Contact from {{.name}}"
body = "From: {{.email}}\n\n{{.message}}"
log_failed_submissions = false
redirect_success = "/thank-you"
redirect_error = "/contact?error=true"

[endpoints.transport]
type = "postmark"

[endpoints.transport.settings]
api_key = "test-key"

[endpoints.rate_limit]
count = 5
interval = "1m"

[logging]
level = "info"
format = "json"
`
	cfg, err := loadString(t, full)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ep := cfg.Endpoints[0]
	if got := len(ep.To); got != 2 {
		t.Errorf("To length = %d, want 2", got)
	}
	if ep.RateLimit == nil {
		t.Fatal("RateLimit unset")
	}
	if ep.RateLimit.Count != 5 {
		t.Errorf("RateLimit.Count = %d, want 5", ep.RateLimit.Count)
	}
	if got := ep.RateLimit.Interval.Std(); got != time.Minute {
		t.Errorf("RateLimit.Interval = %v, want 1m", got)
	}
	if ep.LogFailedSubmissions == nil {
		t.Fatal("LogFailedSubmissions: pointer nil; expected explicit false")
	}
	if *ep.LogFailedSubmissions {
		t.Error("LogFailedSubmissions should be false")
	}
}

func TestLoad_MultipleEndpoints(t *testing.T) {
	multi := minimalTOML + `
[[endpoints]]
path = "/api/newsletter"
to = ["news@example.com"]
from = "noreply@example.com"
subject = "Newsletter signup"
body = "Body"

[endpoints.transport]
type = "postmark"

[endpoints.transport.settings]
api_key = "test-key"
`
	cfg, err := loadString(t, multi)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := len(cfg.Endpoints); got != 2 {
		t.Fatalf("Endpoints length = %d, want 2", got)
	}
	if cfg.Endpoints[0].Path != "/api/contact" {
		t.Errorf("[0].Path = %q", cfg.Endpoints[0].Path)
	}
	if cfg.Endpoints[1].Path != "/api/newsletter" {
		t.Errorf("[1].Path = %q", cfg.Endpoints[1].Path)
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := config.Load("/nonexistent/path/to/config.toml")
	if err == nil {
		t.Fatal("Load: expected error for missing file")
	}
	if !strings.Contains(err.Error(), "read config") {
		t.Errorf("error should mention 'read config': %v", err)
	}
}

func TestLoad_InvalidTOML(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"unclosed_string", `path = "no closing quote`},
		{"malformed_table", `[[[endpoints`},
		{"trailing_garbage", "endpoints = 5\n[broken syntax"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadString(t, tt.content)
			if err == nil {
				t.Fatal("expected parse error")
			}
			if !strings.Contains(err.Error(), "parse TOML") {
				t.Errorf("error should mention 'parse TOML': %v", err)
			}
		})
	}
}

// --- Validation: top-level ---

func TestValidate_NoEndpoints(t *testing.T) {
	_, err := loadString(t, "[logging]\nformat = \"json\"\n")
	if err == nil || !strings.Contains(err.Error(), "at least one endpoint") {
		t.Errorf("error: %v", err)
	}
}

func TestValidate_DuplicatePath(t *testing.T) {
	dup := minimalTOML + minimalTOML
	_, err := loadString(t, dup)
	if err == nil || !strings.Contains(err.Error(), "duplicate endpoint path") {
		t.Errorf("error: %v", err)
	}
}

// --- Validation: endpoint required fields ---

func TestValidate_PathRequired(t *testing.T) {
	noPath := strings.Replace(minimalTOML, `path = "/api/contact"`, "", 1)
	_, err := loadString(t, noPath)
	if err == nil || !strings.Contains(err.Error(), "path is required") {
		t.Errorf("error: %v", err)
	}
}

func TestValidate_PathMustStartWithSlash(t *testing.T) {
	bad := strings.Replace(minimalTOML, `path = "/api/contact"`, `path = "api/contact"`, 1)
	_, err := loadString(t, bad)
	if err == nil || !strings.Contains(err.Error(), "must start with /") {
		t.Errorf("error: %v", err)
	}
}

func TestValidate_ToRequired(t *testing.T) {
	noTo := strings.Replace(minimalTOML, `to = ["craig@example.com"]`, "", 1)
	_, err := loadString(t, noTo)
	if err == nil || !strings.Contains(err.Error(), "to is required") {
		t.Errorf("error: %v", err)
	}
}

func TestValidate_ToInvalidEmail(t *testing.T) {
	bad := strings.Replace(minimalTOML, `to = ["craig@example.com"]`, `to = ["not-an-email"]`, 1)
	_, err := loadString(t, bad)
	if err == nil || !strings.Contains(err.Error(), "to: invalid email") {
		t.Errorf("error: %v", err)
	}
}

func TestValidate_FromRequired(t *testing.T) {
	noFrom := strings.Replace(minimalTOML, `from = "noreply@example.com"`, "", 1)
	_, err := loadString(t, noFrom)
	if err == nil || !strings.Contains(err.Error(), "from is required") {
		t.Errorf("error: %v", err)
	}
}

func TestValidate_FromInvalidEmail(t *testing.T) {
	bad := strings.Replace(minimalTOML, `from = "noreply@example.com"`, `from = "not-an-email"`, 1)
	_, err := loadString(t, bad)
	if err == nil || !strings.Contains(err.Error(), "from: invalid email") {
		t.Errorf("error: %v", err)
	}
}

func TestValidate_SubjectRequired(t *testing.T) {
	noSubj := strings.Replace(minimalTOML, `subject = "Contact"`, "", 1)
	_, err := loadString(t, noSubj)
	if err == nil || !strings.Contains(err.Error(), "subject is required") {
		t.Errorf("error: %v", err)
	}
}

func TestValidate_BodyRequired(t *testing.T) {
	noBody := strings.Replace(minimalTOML, `body = "Body"`, "", 1)
	_, err := loadString(t, noBody)
	if err == nil || !strings.Contains(err.Error(), "body is required") {
		t.Errorf("error: %v", err)
	}
}

// --- Validation: transport ---

func TestValidate_TransportTypeRequired(t *testing.T) {
	noType := strings.Replace(minimalTOML, `type = "postmark"`, "", 1)
	_, err := loadString(t, noType)
	if err == nil || !strings.Contains(err.Error(), "type is required") {
		t.Errorf("error: %v", err)
	}
}

func TestValidate_UnknownTransportType(t *testing.T) {
	wrong := strings.Replace(minimalTOML, `type = "postmark"`, `type = "smtp"`, 1)
	_, err := loadString(t, wrong)
	if err == nil || !strings.Contains(err.Error(), "unknown transport type") {
		t.Errorf("error: %v", err)
	}
}

func TestValidate_PostmarkAPIKeyRequired(t *testing.T) {
	noKey := strings.Replace(minimalTOML, `api_key = "test-key"`, "", 1)
	_, err := loadString(t, noKey)
	if err == nil || !strings.Contains(err.Error(), "settings.api_key") {
		t.Errorf("error: %v", err)
	}
}

// --- Validation: NFR4 — allowed_origins explicitly empty ---

func TestValidate_AllowedOriginsExplicitlyEmpty(t *testing.T) {
	bad := minimalTOML + `allowed_origins = []
`
	// The append above adds at the end of the previous endpoint table — we
	// need to inject inside the endpoint block before the [endpoints.transport]
	// sub-table. Rebuild manually for clarity.
	bad = `
[[endpoints]]
path = "/api/contact"
to = ["craig@example.com"]
from = "noreply@example.com"
subject = "Contact"
body = "Body"
allowed_origins = []

[endpoints.transport]
type = "postmark"

[endpoints.transport.settings]
api_key = "test-key"
`
	_, err := loadString(t, bad)
	if err == nil || !strings.Contains(err.Error(), "allowed_origins is explicitly empty") {
		t.Errorf("error: %v", err)
	}
}

func TestValidate_AllowedOriginsAbsent_OK(t *testing.T) {
	// No allowed_origins key at all means "allow any origin" (FR6, fail-open
	// by absence of config). This must NOT error.
	if _, err := loadString(t, minimalTOML); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// --- Validation: rate limit ---

func TestValidate_RateLimitCountMustBePositive(t *testing.T) {
	bad := minimalTOML + `
[endpoints.rate_limit]
count = 0
interval = "1m"
`
	_, err := loadString(t, bad)
	if err == nil || !strings.Contains(err.Error(), "rate_limit.count") {
		t.Errorf("error: %v", err)
	}
}

func TestValidate_RateLimitIntervalMustBePositive(t *testing.T) {
	bad := minimalTOML + `
[endpoints.rate_limit]
count = 5
interval = "0s"
`
	_, err := loadString(t, bad)
	if err == nil || !strings.Contains(err.Error(), "rate_limit.interval") {
		t.Errorf("error: %v", err)
	}
}

// --- Validation: logging ---

func TestValidate_LoggingFormat(t *testing.T) {
	bad := minimalTOML + `
[logging]
format = "logfmt"
`
	_, err := loadString(t, bad)
	if err == nil || !strings.Contains(err.Error(), "logging.format") {
		t.Errorf("error: %v", err)
	}
}

func TestValidate_LoggingLevel(t *testing.T) {
	bad := minimalTOML + `
[logging]
level = "verbose"
`
	_, err := loadString(t, bad)
	if err == nil || !strings.Contains(err.Error(), "logging.level") {
		t.Errorf("error: %v", err)
	}
}

func TestValidate_LoggingLevels_Accepted(t *testing.T) {
	for _, lvl := range []string{"", "debug", "info", "warn", "error"} {
		t.Run(lvl, func(t *testing.T) {
			c := minimalTOML
			if lvl != "" {
				c += "\n[logging]\nlevel = \"" + lvl + "\"\n"
			}
			if _, err := loadString(t, c); err != nil {
				t.Errorf("level %q rejected: %v", lvl, err)
			}
		})
	}
}

// --- Env var resolution ---

func TestLoad_EnvVarResolution(t *testing.T) {
	t.Setenv("POSTMARK_API_KEY", "secret-token-from-env")
	c := strings.Replace(minimalTOML, `api_key = "test-key"`, `api_key = "${env.POSTMARK_API_KEY}"`, 1)

	cfg, err := loadString(t, c)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, _ := cfg.Endpoints[0].Transport.Settings["api_key"].(string)
	if got != "secret-token-from-env" {
		t.Errorf("api_key = %q, want %q", got, "secret-token-from-env")
	}
}

func TestLoad_EnvVarResolution_MultipleVars(t *testing.T) {
	t.Setenv("RECIP", "ops@example.com")
	t.Setenv("SENDER", "noreply@example.com")
	t.Setenv("KEY", "k")
	c := `
[[endpoints]]
path = "/x"
to = ["${env.RECIP}"]
from = "${env.SENDER}"
subject = "s"
body = "b"

[endpoints.transport]
type = "postmark"

[endpoints.transport.settings]
api_key = "${env.KEY}"
`
	cfg, err := loadString(t, c)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Endpoints[0].To[0] != "ops@example.com" {
		t.Errorf("to = %v", cfg.Endpoints[0].To)
	}
	if cfg.Endpoints[0].From != "noreply@example.com" {
		t.Errorf("from = %q", cfg.Endpoints[0].From)
	}
}

func TestLoad_EnvVarMissing(t *testing.T) {
	// Make sure the var is unset for this test even if the test environment
	// has it set globally.
	t.Setenv("DEFINITELY_UNSET_FOR_THIS_TEST", "x")
	os.Unsetenv("DEFINITELY_UNSET_FOR_THIS_TEST")

	c := strings.Replace(minimalTOML, `api_key = "test-key"`, `api_key = "${env.DEFINITELY_UNSET_FOR_THIS_TEST}"`, 1)
	_, err := loadString(t, c)
	if err == nil {
		t.Fatal("Load: expected error for missing env var")
	}
	if !strings.Contains(err.Error(), "DEFINITELY_UNSET_FOR_THIS_TEST") {
		t.Errorf("error should name the unset var: %v", err)
	}
}

func TestLoad_EnvVarMissing_AllReportedTogether(t *testing.T) {
	// Operators benefit from seeing all unset vars at once rather than
	// re-running on each fix. Verify the loader collects them.
	os.Unsetenv("MISSING_A")
	os.Unsetenv("MISSING_B")

	c := `
[[endpoints]]
path = "/x"
to = ["${env.MISSING_A}"]
from = "${env.MISSING_B}"
subject = "s"
body = "b"

[endpoints.transport]
type = "postmark"

[endpoints.transport.settings]
api_key = "k"
`
	_, err := loadString(t, c)
	if err == nil {
		t.Fatal("expected error for missing env vars")
	}
	if !strings.Contains(err.Error(), "MISSING_A") {
		t.Errorf("error should mention MISSING_A: %v", err)
	}
	if !strings.Contains(err.Error(), "MISSING_B") {
		t.Errorf("error should mention MISSING_B: %v", err)
	}
}

func TestLoad_EnvVar_NoPlaceholders_Passthrough(t *testing.T) {
	// Sanity: a config with no ${env.X} placeholders should load without
	// touching the environment.
	if _, err := loadString(t, minimalTOML); err != nil {
		t.Errorf("Load: %v", err)
	}
}

// TestLoad_EnvVar_BodyTemplateNotInterpolated guards against the env-var
// resolver accidentally substituting Go template syntax that happens to
// include a dollar-brace pattern. Body templates use {{.Name}} (Go's
// standard syntax), which doesn't match our envVarPattern, but this
// test pins the contract.
func TestLoad_EnvVar_BodyTemplateNotInterpolated(t *testing.T) {
	c := strings.Replace(
		minimalTOML,
		`body = "Body"`,
		`body = "Hello {{.name}}, your message was: {{.message}}"`,
		1,
	)
	cfg, err := loadString(t, c)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := "Hello {{.name}}, your message was: {{.message}}"
	if got := cfg.Endpoints[0].Body; got != want {
		t.Errorf("Body = %q, want %q (template syntax must pass through unchanged)", got, want)
	}
}
