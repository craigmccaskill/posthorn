---
title: "Architecture: Posthorn v1.0"
status: locked
created: 2026-04-27
synced_from_obsidian: 2026-04-27
---

# Architecture: Posthorn v1.0

This document describes the structure, lifecycle, and component design for the v1.0 implementation. Every architectural decision derives from a requirement in [the PRD](./02-prd.md), which in turn derives from a commitment in [the project brief](./01-project-brief.md). Nothing here introduces new behavior.

This is the implementation blueprint. Open it before writing code; reference it during code review.

> **2026-05-15 amendment:** The Caddy v2 adapter module was cut from v1.0 pre-tag. Posthorn is now a single Go module at `github.com/craigmccaskill/posthorn` with one deployment shape (standalone behind any reverse proxy). ADR-6 and ADR-7 are retired in-place at the bottom of this doc. The product reasoning for the cut is in the brief's status log.

## Module overview

Posthorn is a single Go module at `github.com/craigmccaskill/posthorn`, rooted at `core/` in the repo. It provides `cmd/posthorn` (the binary) plus the importable HTTP form handler, transport implementations, and supporting libraries.

```
                       ┌──────────────────────────────────┐
HTTP request ─────────▶│   posthorn binary (cmd/posthorn) │
                       │     │                            │
                       │     ▼                            │
                       │   core/gateway handler           │
                       │     │                            │
                       └─────┼────────────────────────────┘
                             │
                             ▼
                       ┌──────────────┐
                       │ Postmark API │
                       └──────────────┘
```

In production the binary sits behind a reverse proxy of the operator's choice (Caddy, nginx, Traefik, Cloudflare) which handles TLS termination and request routing. Posthorn is a plain HTTP service on `:8080`.

## File layout

```
posthorn/
├── README.md
├── LICENSE                       # Apache-2.0
├── CONTRIBUTING.md
├── SECURITY.md
├── CODE_OF_CONDUCT.md
├── CHANGELOG.md
├── .github/
│   ├── workflows/
│   │   ├── ci.yml                # go vet + go test -race
│   │   └── release.yml           # tag → multi-arch Docker image to GHCR
│   ├── PULL_REQUEST_TEMPLATE.md
│   └── ISSUE_TEMPLATE/
├── docs/
│   ├── manual-test.md            # end-to-end test procedure
│   └── release-checklist.md      # tag-day procedure
├── site/                         # Astro + Starlight source for posthorn.dev
├── spec/
│   ├── 01-project-brief.md
│   ├── 02-prd.md
│   └── 03-architecture.md
└── core/
    ├── go.mod                    # github.com/craigmccaskill/posthorn
    ├── go.sum
    ├── Dockerfile                # multi-stage; produces slim posthorn image
    ├── cmd/
    │   └── posthorn/
    │       └── main.go           # CLI entry: serve, validate
    ├── config/
    │   ├── config.go             # Config struct, TOML parser, env resolution
    │   └── config_test.go
    ├── gateway/
    │   ├── handler.go            # gateway.Handler (http.Handler implementor)
    │   ├── pipeline.go           # ordered pipeline: spam → validate → render → send
    │   └── handler_test.go
    ├── transport/
    │   ├── transport.go          # Transport interface, Message, ErrorClass, TransportError
    │   ├── postmark.go           # Postmark HTTP API implementation
    │   ├── resend.go             # v1.0 block C: Resend HTTP API (FR47)
    │   ├── mailgun.go            # v1.0 block C: Mailgun HTTP API (FR48)
    │   ├── awssigv4.go           # v1.0 block C: AWS SigV4 signing primitive (ADR-14)
    │   ├── ses.go                # v1.0 block C: AWS SES via SigV4 (FR49)
    │   ├── smtpout.go            # v1.0 block C: outbound SMTP transport (FR50, ADR-17)
    │   ├── transport_test.go
    │   ├── postmark_test.go
    │   ├── resend_test.go
    │   ├── mailgun_test.go
    │   ├── awssigv4_test.go
    │   ├── ses_test.go
    │   └── smtpout_test.go
    ├── spam/
    │   ├── spam.go               # Honeypot, Origin/Referer, body size
    │   └── spam_test.go
    ├── ratelimit/
    │   ├── ratelimit.go          # Token bucket, LRU-bounded
    │   ├── clientip.go           # X-Forwarded-For with trusted_proxies
    │   └── ratelimit_test.go
    ├── validate/
    │   ├── validate.go           # Required-fields, email format
    │   └── validate_test.go
    ├── template/
    │   ├── template.go           # Subject/body rendering, custom-fields passthrough
    │   └── template_test.go
    ├── response/
    │   ├── response.go           # JSON builder, content negotiation, redirects
    │   └── response_test.go
    ├── log/
    │   ├── log.go                # Structured logging helpers, submission ID
    │   └── log_test.go
    ├── idempotency/              # v1.0 block B: per-endpoint LRU cache + in-flight tracker
    │   ├── cache.go              # LRU + TTL + in-flight tracker (FR40-FR44, NFR20)
    │   └── cache_test.go
    ├── ingress/                  # v1.0 block D: Ingress interface (FR60, ADR-12)
    │   ├── ingress.go            # interface definition; HTTP gateway adapts to it
    │   └── ingress_test.go
    ├── smtp/                     # v1.0 block D: inbound SMTP listener (FR62-FR68)
    │   ├── listener.go           # TCP accept loop, STARTTLS upgrade, session state
    │   ├── session.go            # per-connection SMTP state machine
    │   ├── parse.go              # MIME → transport.Message (NFR22)
    │   ├── auth.go               # SMTP AUTH PLAIN/LOGIN + client-cert (FR63)
    │   ├── listener_test.go
    │   ├── session_test.go
    │   ├── parse_test.go
    │   └── auth_test.go
    ├── csrf/                     # v1.0 block C: HMAC-signed CSRF tokens (FR57, ADR-16)
    │   ├── csrf.go               # Issue/Verify helpers
    │   └── csrf_test.go
    └── metrics/                  # v1.0 block C: hand-rolled Prometheus exposition (FR55, ADR-15)
        ├── metrics.go            # counters, histograms, exposition writer
        ├── healthz.go            # /healthz handler (FR54)
        └── metrics_test.go
```

Sub-packages are organized one-per-concern. The original `caddy-formward` design used a flat package; Posthorn's broader scope (standalone binary, multi-ingress) justifies the package boundaries.

`core/idempotency/` lands in v1.0 feature block B (Epic 8 Story 8.4). `core/ingress/`, `core/smtp/`, `core/csrf/`, and `core/metrics/` land in feature blocks C and D (Epics 9-11). API-mode authentication is implemented in `core/gateway/` rather than a separate `core/auth/` package — auth state is per-request and tightly coupled to per-endpoint config, so the package boundary doesn't earn its keep.

## Lifecycle

```
1. main.go: parse CLI flags (subcommand: serve | validate)
2. config.Load(path): read TOML, resolve ${env.VAR}, validate schema
3. For each endpoint in config:
     a. Construct transport (Postmark client with API key)
     b. Compile templates (subject, body)
     c. Construct rate limiter
     d. Construct core.Handler with all of the above
4. Build http.ServeMux mapping each endpoint path to its core.Handler
5. http.Server.ListenAndServe()
6. On SIGTERM/SIGINT:
     a. Stop accepting new connections (server.Shutdown)
     b. Drain in-flight requests up to 10s per-request timeout
     c. Exit 0
   On second signal: forced exit
```

## Request flow

The `core/http.Handler` processes each request through an ordered pipeline. Order is intentional — cheaper checks first, header-only checks before body-parsing checks, security checks before processing.

```
┌──────────────────────────────────────────────────────────────┐
│ 1. body size cap        → http.MaxBytesReader wraps r.Body   │  413
│ 2. method check         → POST only                          │  405
│ 3. content-type check   → form-encoded only                  │  400
│ 4. origin/referer check → fail-closed if allowed_origins set │  403
│ 5. rate limit check     → token bucket, proxy-aware IP       │  429
│ 6. parse form           → r.ParseForm() reads body           │  413/400
│ 7. honeypot check       → silent 200 if field non-empty      │  200 (silent)
│ 8. required fields      → all listed fields present + non-empty │ 422
│ 9. email format         → submitter email field syntactic    │  422
│ 10. generate submission ID (UUIDv4), log "submission_received"│
│ 11. render subject template                                  │
│ 12. render body template + custom-fields passthrough         │
│ 13. transport.Send() with retry policy (FR19-22)             │
│ 14. log outcome, write response (JSON or redirect)           │  200/502
└──────────────────────────────────────────────────────────────┘
```

Ordering rationale carries over from the prior architecture (cheaper-first, header-before-body, security-first). Identical in both deployment shapes.

## Component design

### Configuration model (`core/config`)

A single top-level `Config` struct holds the parsed configuration, loaded from the TOML file.

```go
type Config struct {
    Endpoints []EndpointConfig `toml:"endpoints"`
    Logging   LoggingConfig    `toml:"logging"`
}

type EndpointConfig struct {
    Path                 string             `toml:"path"`
    To                   []string           `toml:"to"`
    From                 string             `toml:"from"`
    Transport            TransportConfig    `toml:"transport"`
    RateLimit            *RateLimitConfig   `toml:"rate_limit"`
    TrustedProxies       []string           `toml:"trusted_proxies"`
    Honeypot             string             `toml:"honeypot"`
    AllowedOrigins       []string           `toml:"allowed_origins"`
    MaxBodySize          string             `toml:"max_body_size"` // "32KB", "1MB"
    Required             []string           `toml:"required"`
    EmailField           string             `toml:"email_field"`
    Subject              string             `toml:"subject"`
    Body                 string             `toml:"body"`
    LogFailedSubmissions *bool              `toml:"log_failed_submissions"`
    RedirectSuccess      string             `toml:"redirect_success"`
    RedirectError        string             `toml:"redirect_error"`
}

type TransportConfig struct {
    Type     string                 `toml:"type"`     // "postmark"
    Settings map[string]any         `toml:"settings"` // transport-specific (api_key for postmark)
}
```

`*bool` for `LogFailedSubmissions` allows distinguishing unset (default true) from explicitly false (see ADR-4).

Env-var resolution (`${env.VAR}`) runs as a post-parse pass over all string fields recursively. Missing env vars are config-validation errors, not runtime errors.

### Transport interface (`core/transport`)

The transport layer is one interface with one implementation in v1.0. The interface is intentionally narrow so future transports (Resend, Mailgun, SMTP outbound, SMTP-ingress shared backend) can implement it without changes.

```go
type Message struct {
    From     string
    To       []string
    ReplyTo  string
    Subject  string
    BodyText string
    // BodyHTML is reserved for v2 markdown body support.
}

type Transport interface {
    Send(ctx context.Context, msg Message) error
}

type ErrorClass int

const (
    ErrUnknown      ErrorClass = iota
    ErrTransient                // network/5xx, retry once after 1s
    ErrRateLimited              // 429, retry once after 5s
    ErrTerminal                 // 4xx (non-429), no retry
)

type TransportError struct {
    Class   ErrorClass
    Status  int
    Cause   error
    Message string
}
```

The interface and its support types are unchanged from the prior `caddy-formward` design (see brief Status log; the existing `transport.go` and `transport_postmark.go` migrate into `core/transport/` with no semantic changes — only import path updates).

### Postmark transport (`core/transport/postmark.go`)

Unchanged in behavior from the prior design:

- No third-party Postmark SDK (ADR-1)
- Headers passed as JSON struct fields (NFR1 enforcement)
- API key in `X-Postmark-Server-Token` header, never logged (NFR3)
- Status mapping: 200/202 → success; 429 → ErrRateLimited; 5xx/network → ErrTransient; 4xx (non-429) → ErrTerminal
- Package-level `*http.Client` with 5s per-request timeout (caller's context handles overall deadline)

### Gateway handler (`core/gateway`)

`core.Handler` is a plain `http.Handler` that wraps the per-endpoint pipeline:

```go
type Handler struct {
    cfg          EndpointConfig
    transport    Transport
    limiter      *ratelimit.Limiter
    subjectTpl   *template.Template
    bodyTpl      *template.Template
    namedFields  map[string]bool
    logger       *slog.Logger
}

func New(cfg EndpointConfig, opts ...Option) (*Handler, error) { ... }
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { ... }
```

Construction (`New`) does the equivalent of the prior `Provision` — resolves placeholders, compiles templates, builds the rate limiter, instantiates the transport. Errors at construction time are fatal config errors; the binary surfaces them via the `validate` subcommand's exit code.

### Spam protection (`core/spam`)

Three independent check functions, each pure (no logging, no response writing):

```go
type Result int
const (
    Pass Result = iota
    SilentReject    // honeypot — return 200 OK silently
    HardReject      // origin — return appropriate status
)

func CheckHoneypot(form url.Values, fieldName string) Result
func CheckOrigin(r *http.Request, allowed []string) (Result, string /* reason */)
// max-body-size is enforced via http.MaxBytesReader at the handler entry
```

The handler runs these in sequence and translates results to HTTP responses.

### Rate limiter (`core/ratelimit`)

Token-bucket limiter keyed by client IP. Implementation unchanged from the prior design (see ADR-3 — token bucket with LRU, not `golang.org/x/time/rate`):

```go
type Limiter struct {
    capacity     float64
    refillPerSec float64
    mu           sync.Mutex
    buckets      *lru.Cache[string, *bucket]
}

func (l *Limiter) Allow(clientIP string) bool { ... }
```

Default LRU capacity 10K IPs (NFR6). `clientip.go` extracts the keying IP from RemoteAddr or X-Forwarded-For per the trusted_proxies config.

### Validation (`core/validate`)

Pure functions:

```go
func RequiredFields(form url.Values, required []string) []string
func EmailFormat(value string) bool
```

No logging, no response writing — the handler uses returns to construct 422 responses.

### Templating (`core/template`)

Subject and body rendering with custom-fields passthrough. The "named fields" set (required + email field + honeypot + fields referenced in templates) is computed at config-load time via Go template `Tree.Root.Nodes` walk, cached in the Handler.

Custom-fields passthrough format unchanged from prior design:
```
[rendered body template output]

Additional fields:
  company: Acme Corp
  source: HN
```

### Response handling (`core/response`)

```go
type ErrorResponse struct {
    Error  string            `json:"error"`
    Code   string            `json:"code"`
    Fields map[string]string `json:"fields,omitempty"`
}

func WriteJSON(w http.ResponseWriter, status int, body any) error
func WriteRedirect(w http.ResponseWriter, r *http.Request, url string) error
func Negotiate(r *http.Request, hasRedirects bool) Mode
```

Negotiation logic: if no redirect URLs configured → JSON; else if `Accept: application/json` preferred → JSON; else redirect.

### Logging (`core/log`)

Wraps `log/slog` (stdlib) with structured-field helpers. The binary configures slog with a JSON handler at INFO by default.

```go
type Ctx struct {
    SubmissionID uuid.UUID
    Endpoint     string
    Transport    string
    StartTime    time.Time
}

func (l *Logger) Received(ctx Ctx, form url.Values)
func (l *Logger) Sent(ctx Ctx)
func (l *Logger) Retry(ctx Ctx, err error)
func (l *Logger) Failed(ctx Ctx, err error, payload url.Values)
func (l *Logger) SpamBlocked(ctx Ctx, reason string)
func (l *Logger) RateLimited(ctx Ctx, clientIP string)
func (l *Logger) ValidationFailed(ctx Ctx, fields []string)
```

Every log line includes `submission_id`, `endpoint`, `transport`, `latency_ms`. API keys never appear in any log path.

`Failed` includes the full payload only if `log_failed_submissions` is true; otherwise omits form values.

## Threat → defense → code mapping

This is the load-bearing artifact for security review. Every in-scope threat from [the project brief](./01-project-brief.md) §"Threat Model" maps to a concrete defense in concrete code with concrete tests.

| Threat | Defense | Code | Tests |
|---|---|---|---|
| Drive-by scraper bots | Honeypot field | `core/spam/spam.go::CheckHoneypot` | `core/spam/spam_test.go::TestHoneypot_*` |
| Direct-POST bots that skip the form page | Origin/Referer with fail-closed | `core/spam/spam.go::CheckOrigin` | `core/spam/spam_test.go::TestOrigin_*` |
| Basic targeted abuse | Token bucket rate limit, proxy-aware | `core/ratelimit/ratelimit.go::Allow`, `core/ratelimit/clientip.go` | `core/ratelimit/ratelimit_test.go` |
| Postmark quota burn | Rate limit + max body size | `core/ratelimit/` + `http.MaxBytesReader` in handler | as above + `core/gateway/handler_test.go::TestBodySizeLimit` |
| Email header injection | Header fields passed as JSON struct fields, never string-concat | `core/transport/postmark.go::Send` (struct → `json.Marshal`) | `core/transport/postmark_test.go::TestNoHeaderInjection_*` |
| API key theft from logs | Key set in HTTP header at construction time, never passed to logger | `core/transport/postmark.go::Send`, `core/log/*` | `core/log/log_test.go::TestNoAPIKeyInLogs` |
| **v1.1: Leaked API-mode key** (operator-side compromise) | Constant-time comparison; per-key rate limit (not per-IP); key never logged | `core/gateway/handler.go` (auth path), `core/ratelimit/` (key extraction) | `core/gateway/handler_test.go::TestAPIAuth_*`, `TestAPIAuth_KeyNotInLogs` |
| **v1.1: Replay attacks on API-mode endpoints** | Idempotency-Key cache returns byte-identical response on replay; in-flight collision returns 409 | `core/idempotency/cache.go`, `core/gateway/handler.go` (cache integration) | `core/idempotency/cache_test.go`, `core/gateway/handler_test.go::TestIdempotency_*` |
| **v1.1: Timing attack on API-key comparison** | `crypto/subtle.ConstantTimeCompare` for all key checks | `core/gateway/handler.go` (auth path) | `core/gateway/handler_test.go::TestAPIAuth_ConstantTimeCompare` (source-level assertion) |
| **v1.1: Mode misconfiguration** (operator sets form-mode defenses on API-mode endpoint, thinks they're protected) | Parse-time rejection with named-field error | `core/config/config.go::Validate` | `core/config/config_test.go::TestAPIMode_RejectsFormFields` |
| **v1.0 D: SMTP open relay** (attacker connects to listener and tries to send to arbitrary recipients) | Auth required after EHLO (530); sender allowlist (550); recipient allowlist OR cap (550); STARTTLS required by default (530) | `core/smtp/session.go`, `core/smtp/auth.go` | `core/smtp/session_test.go::TestUnauth*`, `TestSenderAllowlist*`, `TestRecipientCap*`, `TestSTARTTLSRequired*` |
| **v1.0 D: SMTP header injection via MIME** (attacker sends `Subject:` followed by `\r\nBcc:`) | MIME parsed by `net/mail`, not string-split; headers stored as a structured map; outbound Message construction uses struct fields | `core/smtp/parse.go` | `core/smtp/parse_test.go::TestNoHeaderInjection_MIME*` (NFR22) |
| **v1.0 D: SMTP credential leak** (attacker captures AUTH PLAIN bytes off the wire) | STARTTLS required before AUTH; plain-text AUTH path explicitly disabled | `core/smtp/session.go` | `core/smtp/session_test.go::TestAuthBlockedBeforeTLS` |
| **v1.0 C: `/metrics` cardinality explosion** (attacker triggers metric labels containing recipient emails) | Labels carry only operator-configured names (endpoint path, transport type, error class); submitter values never appear as labels | `core/metrics/metrics.go` | `core/metrics/metrics_test.go::TestNoSubmitterCardinality` (NFR24) |
| **v1.0 C: `csrf_secret` leak via logs** | Secret loaded from `${env.VAR}`, never logged; same pattern as NFR3 for API keys | `core/csrf/csrf.go`, `core/log/*` | `core/csrf/csrf_test.go::TestSecretNotInLogs` |

For threats explicitly out of scope, this table also documents the *non*-defense:

| Out-of-scope threat | Disposition |
|---|---|
| SMTP-ingress threats (open relay, MX spoofing, RCPT bombing) | v1.3 SMTP ingress will add: AUTH PLAIN/LOGIN, RCPT recipient cap, sender allowlist, body size cap. Architecture must not foreclose this — see "Forward compatibility" below. |
| Botnet spam (many low-rate IPs) | No v1.0 defense; LRU eviction at 10K IPs gracefully degrades but does not protect. v3 captcha/PoW is the planned response. |
| DDoS / Layer 7 attacks | CDN's responsibility; gateway trusts upstream proxy/load balancer to absorb. Documented in README. |
| API key theft from misconfigured deployment | Operator concern; mitigated by `${env.VAR}` config + NFR3 (key never logged). Documented in README. |

## Concurrency and state

### Per-request state

Each `ServeHTTP` invocation operates on:
- The request and response writer (request-scoped, no shared mutation)
- A locally-allocated `log.Ctx` with a fresh UUID
- Locally-rendered subject and body strings
- A local `Message` passed to `Transport.Send`

No request-scoped state is shared across requests.

### Shared state

Each `core.Handler` holds three pieces of shared state, all populated at construction time and immutable thereafter:

| State | Concurrency strategy |
|---|---|
| `*template.Template` (subject and body) | Go's `template.Template` is safe for concurrent execution after parse; no extra locking needed. |
| `Transport` (Postmark client) | Wraps a single `*http.Client`. Standard library guarantees `*http.Client` is safe for concurrent use. The transport itself is stateless beyond the client and API key. |
| `*ratelimit.Limiter` | Mutex-guarded; one mutex per limiter, held only during the per-IP bucket update. Contention is bounded by request rate. |

No goroutines are spawned per request. The standalone `cmd/posthorn` runs `http.Server` which spawns goroutines per connection; the rate limiter and template are the only shared state across those goroutines.

## Dependencies

### Core (production)

| Dependency | Purpose | Justification |
|---|---|---|
| `github.com/BurntSushi/toml` | TOML config parsing | The standard Go TOML library; mature, single-purpose |
| `github.com/google/uuid` | Submission ID generation | Standard, single-purpose, ~200 LOC |
| `github.com/hashicorp/golang-lru/v2` | LRU cache for rate limiter | Battle-tested, type-parameterized in v2 |

### Core (test-only)

| Dependency | Purpose |
|---|---|
| Standard library `testing` | Test framework |
| Standard library `net/http/httptest` | Mock Postmark API server |

### Explicitly NOT pulled in

- Postmark SDKs (ADR-1)
- Validation libraries — stdlib `net/mail` suffices for v1.0
- Rate-limiting libraries (ADR-3)
- Templating engines beyond stdlib `text/template`
- HTTP frameworks (gin, echo, chi, etc.) — stdlib `net/http` is sufficient
- Logging libraries beyond stdlib `log/slog`
- Cobra/urfave/cli for CLI — flag stdlib package suffices for `serve`/`validate`

This list is conservative on purpose: every dependency is a v1.1+ liability.

## Test architecture

Test files are co-located with the source they test (`spam.go` ↔ `spam_test.go`).

### Mock Postmark server

Transport tests use `httptest.NewServer` with handler stubs. The Postmark transport accepts a `BaseURL` config value (default `https://api.postmarkapp.com`) so tests can point it at the mock. Production config never sets `BaseURL`.

### Header injection tests (NFR2)

Required test cases as a table (carries over from prior architecture; the payloads are unchanged):

```go
var injectionPayloads = []struct {
    name  string
    field string
    value string
}{
    {"crlf_bcc_in_from",     "from",     "attacker@evil.com\r\nBcc: target@victim.com"},
    {"crlf_bcc_in_subject",  "subject",  "Hello\r\nBcc: target@victim.com"},
    {"crlf_in_replyto",      "reply_to", "x@x.com\r\nBcc: target@victim.com"},
    {"unicode_crlf",         "subject",  "Hello\u000ABcc: target@victim.com"},
    {"smuggled_header",      "name",     "Jane\r\nX-Spoof: yes"},
}
```

For each payload: send a submission, capture the outgoing JSON to the mock Postmark server, assert (1) the injected CRLF sequence does not appear as a JSON-level header in the marshaled request and (2) the literal string is preserved as data within the field it was injected into.

### CI

Single Linux + Go 1.25 job. `go vet ./...` and `go test -race -count=1 -timeout=2m ./...` from the `core/` working directory.

## Build and distribution

### Standalone binary

```bash
go install github.com/craigmccaskill/posthorn/cmd/posthorn@latest
```

Produces a single static binary. No CGO. Single-file deployment.

### Standalone Docker image

Multi-stage:

```dockerfile
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY . .
ENV CGO_ENABLED=0 GOWORK=off
RUN go build -trimpath -ldflags="-s -w" -o /out/posthorn ./cmd/posthorn

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/posthorn /usr/local/bin/posthorn
USER nonroot
ENTRYPOINT ["/usr/local/bin/posthorn", "serve"]
CMD ["--config", "/etc/posthorn/config.toml"]
```

Multi-arch via `docker buildx` in the release workflow: `linux/amd64` and `linux/arm64`. Image at `ghcr.io/craigmccaskill/posthorn:v1.0.0` and `:latest`.

### Release artifacts

For each tagged release (v1.0.0+):
- GitHub Release with hand-written notes
- Multi-arch Docker image at `ghcr.io/craigmccaskill/posthorn`
- No pre-built binaries — `go install` and Docker are the supported install paths

## v1.0 implementation plan + forward compatibility

Sections below describe both the v1.0 implementation plan (feature blocks A through D, originally numbered v1.0/v1.1/v1.2/v1.3 before the 2026-05-16 rescope) and the genuinely forward-looking v2 / v3 commitments. The four v1.0 blocks are sequenced as Epics 1–11 inside a single v1.0 release; the version sub-headings preserve historical traceability and let each block be reasoned about independently. v2 and v3 remain future scope and carry forward-compatibility implications for v1.0 code.

These commitments serve the project's [Design principles](./01-project-brief.md#design-principles) in the brief — especially "Gateway, not infrastructure" (principle #1) and "Integration layer, not mail-receiving layer" (principle #2). When adding a new forward-compat commitment, check both the principles and the existing ADRs (below); architecture decisions that contradict the principles need spec discussion first.

### v1.1 (API mode) — scoped 2026-05-16

A second auth + ingress shape alongside the v1.0 form-mode pipeline. None of the additions break the existing form-mode path (FR45 backwards-compat invariant). Three coherent features: API-key auth (FR31–FR35), JSON content type (FR36–FR39), idempotency (FR40–FR44). Batch send was originally listed here and dropped 2026-05-16; see brief's "Deliberately not on the roadmap" section.

**Mode dispatch (FR31, FR32).** New `EndpointConfig.Auth` field with values `"form"` (default; v1.0 behavior) and `"api-key"`. The handler routes per-mode at the top of `ServeHTTP`. Mode is fixed per endpoint — there is no mixed shape. Mode-incompatible config fields (`honeypot`, `allowed_origins`, `redirect_success`, `redirect_error` on API-mode endpoints) are rejected at config-parse time with a named-field error message, not silently ignored at request time — see ADR-10.

**API-key authentication (FR33, FR34, NFR19, NFR21).** API-mode endpoints require non-empty `api_keys`. The handler parses `Authorization: Bearer <key>` and compares against the list using `crypto/subtle.ConstantTimeCompare`. Multiple keys are supported per endpoint to enable rotation. Failed auth returns HTTP 401 with no detail in the body — the operator's logs carry the structured failure. The matched key never appears in log output (parallel to NFR3 for Postmark tokens).

**API-mode rate limiting (FR35).** On API-mode endpoints, the existing `core/ratelimit/` token bucket keys on the matched API key value, not the client IP. Server-to-server callers commonly NAT through shared egress IPs; IP-keyed rate limiting would conflate independent callers. `trusted_proxies` / `X-Forwarded-For` logic does not apply to API-mode endpoints. The underlying token-bucket + LRU eviction implementation is unchanged.

**JSON ingress (FR36–FR39).** API-mode endpoints accept `application/json` bodies only. JSON is parsed into the same flat-keyed `map[string][]string` shape the form parser produces (single-valued keys become a one-element slice), so template rendering, validation, custom-fields passthrough, and the transport layer require no v1.1 changes. Non-JSON bodies on API-mode endpoints return HTTP 415, never silently accepted.

**Idempotency (FR40–FR44, NFR20).** New package `core/idempotency/` provides per-endpoint LRU caches with TTL eviction (24h default) and an in-flight tracker. The cache key is the raw `Idempotency-Key` header value; per-endpoint scoping (FR41) is achieved by giving each API-mode endpoint its own cache instance — no key prefixing, no cross-endpoint collision possible by construction. The cache stores the *complete* response (status code, body bytes, submission_id, transport_message_id), so replays are byte-identical to the original (NFR20). In-flight tracker is a separate `map[string]chan struct{}` guarded by a mutex; a duplicate key arriving before the original completes returns HTTP 409 Conflict — not blocking, see ADR-9. v2 swaps the storage backend for SQLite while keeping the package interface stable.

**Per-request recipient override (FR46, ADR-11).** API-mode request bodies may include a `to_override` field (string or array of strings) that replaces the endpoint's configured recipients for that one request. Each entry passes the same email-syntax validation as form-mode submissions (FR11); failures return 422 with field detail. `from` is intentionally not overridable — see ADR-11 for the safety-asymmetry reasoning. The handler inserts the override between validation and `transport.Message` construction; absent override falls back to `cfg.To` (no behavior change for callers that don't use it).

### v1.0 block C (multi-transport + operational maturity) — scoped 2026-05-16

Originally v1.2; folded into v1.0 alongside blocks B and D. Four new transports plus operational features.

**Transport registry (FR47–FR53, Story 9.1).** Today's `config.TransportConfig.Validate` switches hard-coded on `type`. The v1.0-C refactor introduces a registry pattern where each transport file (`resend.go`, `mailgun.go`, `ses.go`, `smtpout.go`) calls `transport.Register(type string, validator func(map[string]any) error, factory func(cfg map[string]any) (Transport, error))` in an `init()`. The Postmark transport adapts to the same registration pattern. Tests pass unchanged because the public `Transport` interface and `TransportError` contract don't move.

**Resend transport (FR47, Story 9.2).** Bespoke HTTP client (~150 LOC) against `https://api.resend.com/emails`. Bearer auth via `Authorization` header. Response shape includes a JSON `id` field which becomes `SendResult.MessageID`. Error mapping: 422 → `ErrTerminal`, 429 → `ErrRateLimited`, 5xx → `ErrTransient`. NFR1 enforcement via `json.Marshal` of a struct (parallel to Postmark).

**Mailgun transport (FR48, Story 9.3).** Bespoke HTTP client (~180 LOC). Auth via HTTP Basic `api:<key>`. Request body uses `mime/multipart.Writer` — never hand-craft boundary string concatenation (NFR1 lives in the multipart writer, which escapes the structured fields properly). Endpoint: `https://api.mailgun.net/v3/<domain>/messages`. Response `id` field → `SendResult.MessageID`.

**AWS SES transport (FR49, ADR-14, Story 9.4).** Bespoke SigV4 implementation in `core/transport/awssigv4.go` (~200 LOC), SES request shape in `ses.go` (~100 LOC). SigV4 signing is the canonical AWS signature-v4 algorithm: build the canonical request, hash, build the string-to-sign, derive the signing key via four rounds of HMAC-SHA256, sign. Tests round-trip against AWS's published canonical-request examples. SES request shape uses the SESv2 `SendEmail` endpoint (JSON body, returns a `MessageId`). **The ADR-14 tripwire is real:** if total LOC including tests exceeds 500, we stop and surface the deviation rather than ship bloated bespoke code.

**Outbound-SMTP transport (FR50, ADR-17, Story 9.5).** Stdlib `net/smtp.PlainAuth` + `net/smtp.NewClient` + `STARTTLS`. Required config: `host`, `port`, `username`, `password`, `require_tls` (default true). The transport is stateful per-Send (open connection, AUTH, STARTTLS, MAIL FROM, RCPT TO, DATA, QUIT, close) — no connection reuse in v1.0; each Send is a fresh TCP+TLS handshake. Reuse is a v2 optimization. Error classification: 421 / 450 / 451 (greylisting) → `ErrTransient`; 530 (auth) / 550-552 (relay rejection, size) → `ErrTerminal`; TLS handshake failure → `ErrTerminal` (config bug, not transient).

**`/healthz` and `/metrics` (FR54, FR55, ADR-15, Story 10.1).** New `core/metrics/` package. `/healthz` returns 200 + `ok`. `/metrics` writes Prometheus text exposition format manually — `# HELP`, `# TYPE`, then `<metric_name>{<labels>} <value>` lines. Counter primitive: `atomic.Int64` + label map. Histogram primitive: fixed buckets ([1ms, 5ms, 10ms, 50ms, 100ms, 500ms, 1s, 5s, 10s]) + `atomic.Int64` per bucket. The metrics handler walks the registered metrics and emits in alphabetical order for deterministic output. NFR24 enforcement: label values come from a known operator-controlled set (endpoint paths from config, transport types from registry, error classes from `ErrorClass.String()`) — no submitter-controlled strings ever enter the label space.

**Dry-run mode (FR56, Story 10.2).** New `EndpointConfig.DryRun bool` field. Handler short-circuits before `sendWithRetry`: skip transport, build the `transport.Message` as normal, write `200 OK` with JSON body `{"status":"dry_run","prepared_message":{...}}`. Idempotency cache treats dry-run replies as cacheable (the operator may legitimately replay a dry-run to inspect template state). Form-mode and api-mode both supported.

**CSRF tokens (FR57, ADR-16, Story 10.3).** New `core/csrf/` package exposing `Issue(secret []byte, ttl time.Duration) string` and `Verify(secret []byte, token string, maxAge time.Duration) error`. Token format: `<unix-timestamp>.<base64-hmac>` where `hmac = HMAC-SHA256(secret, timestamp)`. `Issue` is for operator-side embedding at form-render time; `Verify` runs in the handler when `csrf_secret` is configured. Form-mode only — api-mode endpoints reject `csrf_secret` at config parse (parallels FR32's mode-mutex). The handler integrates the check after honeypot and before required-fields validation: missing/malformed/expired → 403.

**`trusted_proxies` named presets (FR58, Story 10.4).** Embedded static map at package init time:

```go
var trustedProxiesPresets = map[string][]string{
    "cloudflare":        {"173.245.48.0/20", "103.21.244.0/22", /* full list */},
    "aws-elb":           {/* AWS published ELB CIDR ranges */},
    "gcp-lb":            {/* Google Cloud LB CIDRs */},
    "azure-front-door":  {/* Azure Front Door CIDRs */},
}
```

`ratelimit.ParsePrefixes` expands preset names at parse time; mixing presets with explicit CIDRs in one list is allowed. Updates to a provider's published ranges require a Posthorn release. We don't refresh dynamically — the brief's "no runtime mutation surface" principle applies.

**IP stripping (FR59, Story 10.5).** New `EndpointConfig.StripClientIP bool`. Handler check at each log site that includes `client_ip` — omit the field entirely when `strip_client_ip = true`. The IP is still computed (rate limiter needs it) but not persisted to log lines. Rate-limit keying is unaffected.

### v1.0 block D (SMTP ingress) — scoped 2026-05-16

Originally v1.3; folded into v1.0 alongside blocks B and C. The strategic feature that completes the gateway thesis — internal apps that speak SMTP get a relay that exists and works.

**`Ingress` interface (FR60, FR61, ADR-12, Stories 11.1, 11.2).** New `core/ingress/` package defining:

```go
// Ingress is "thing that produces a transport.Message and dispatches it."
// HTTP form/api-mode handler and SMTP listener both implement it.
type Ingress interface {
    // Lifecycle methods (Start/Stop) match cmd/posthorn's listener-loop
    // shape. Each ingress owns its own listener goroutine.
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
}
```

The interface itself is intentionally minimal — Messages flow internally to each ingress and reach `transport.Send` via the ingress-specific path. The interface is for `cmd/posthorn`'s lifecycle management, not for cross-ingress code sharing. **HTTP-specific concepts (templates, content negotiation, redirects, idempotency keys, CSRF tokens) do not appear in the interface** — they're internal to the HTTP ingress.

The existing `gateway.Handler` adapts to satisfy `Ingress` by wrapping itself in a Start/Stop pair that runs an `http.Server`. No behavior change; the refactor is mechanical.

**SMTP listener architecture (FR62–FR67, Stories 11.3, 11.4, 11.6, 11.7).** New `core/smtp/` package with three concerns:

```
core/smtp/
  listener.go   — TCP accept loop, per-connection goroutine spawn
  session.go    — SMTP state machine (EHLO → STARTTLS → AUTH → MAIL → RCPT → DATA → QUIT)
  parse.go      — MIME → transport.Message
  auth.go       — SMTP AUTH PLAIN/LOGIN + client-cert verification
```

State machine states: `Greeted`, `TLSPending`, `TLSEstablished`, `Authed`, `InTransaction`, `InData`, `Quit`. Transitions are validated — sending RCPT before MAIL FROM is rejected with `503 5.5.1 Bad sequence of commands`. STARTTLS upgrade replaces the underlying `net.Conn` with `*tls.Conn` and resets EHLO state. AUTH PLAIN decodes `\x00<user>\x00<pass>` and constant-time compares (NFR3 + NFR21 carry over).

Per-session state lives on a `session` struct, garbage-collected when the connection closes. Idle timeout (default 60s) closes hanging connections. Max message size enforced by `io.LimitReader` on the DATA path.

**MIME ingest (FR68, NFR22, Story 11.5).** `core/smtp/parse.go` uses `net/mail.ReadMessage` to parse the DATA blob into headers + body. For multipart bodies, `mime/multipart.NewReader` walks parts; we extract `text/plain` (preferred) or reject HTML-only with `554 5.6.0 No plain-text part` (HTML body is v2 scope). Resulting `transport.Message`: From (header), To (RCPT TO list — overrides MIME `To:` per NFR22 to prevent header injection), Subject (header, validated for CRLF), BodyText.

Key NFR22 guarantee: **MIME `To:`/`Cc:`/`Bcc:` headers in the inbound message are ignored.** Recipients are taken exclusively from the SMTP transaction's RCPT TO commands, which we've already validated against the allowlist. This is the structural defense against `Subject: hi\r\nBcc: victim@target.com` style injection — the smuggled `Bcc:` header lands in the parsed headers map but never affects recipients.

**SMTP-specific defenses (FR64, FR65, FR66, FR67, Story 11.6).** Sender allowlist matching (exact email or `*@domain.com` wildcard). Recipient allowlist OR `max_recipients_per_session` (one must be set; both can coexist with allowlist applied first). Message size cap via `io.LimitReader`. STARTTLS enforcement: when `require_tls=true`, plain-text AUTH and MAIL commands are rejected with `530 5.7.0 Must issue STARTTLS first`.

**Binary integration (Story 11.7).** `cmd/posthorn/main.go` reads top-level `[smtp_listener]` config (parallel to `[[endpoints]]`); when present, constructs the SMTP ingress and adds it to a list of `[]Ingress` to start. Each ingress runs in its own goroutine. SIGTERM signals all, drains all (each ingress has its own drain timeout — 10s for HTTP, longer for SMTP since transactions may be mid-flight). Forced exit on second signal.

**Config shape:**

```toml
[smtp_listener]
listen = ":2525"
require_tls = true
tls_cert = "/etc/posthorn/cert.pem"
tls_key = "/etc/posthorn/key.pem"

# Auth: smtp-auth | client-cert | either
auth_required = "smtp-auth"
[[smtp_listener.smtp_users]]
username = "ghost"
password = "${env.GHOST_SMTP_PASSWORD}"
[[smtp_listener.smtp_users]]
username = "gitea"
password = "${env.GITEA_SMTP_PASSWORD}"

allowed_senders = ["noreply@yourdomain.com", "*@internal.yourdomain.com"]
allowed_recipients = ["*"]  # or list specific allowed RCPT TO patterns
max_recipients_per_session = 10
max_message_size = "1MB"
idle_timeout = "60s"

[smtp_listener.transport]
type = "postmark"

[smtp_listener.transport.settings]
api_key = "${env.POSTMARK_API_KEY}"
```

### v2 (platform maturity — persistent state + mail-platform features)

Adds `core/storage/` for SQLite. The storage layer underpins five separate user-visible features that all need durability:

- **Submission log + retry queue.** Every submission persisted; failed sends survive restart and retry later. The `Transport.Send` interface stays as the synchronous primitive; the queue wraps it.
- **Suppression list.** Auto-populated on hard bounces and spam complaints (via the lifecycle webhook plumbing below). Refuses to send to suppressed addresses.
- **Durable idempotency.** Replaces v1.1's in-memory cache. Same package interface, persistent backing.
- **Lifecycle event callbacks.** Posthorn receives Postmark's bounce/delivery/click webhooks at a registered URL, looks up the originating endpoint by message ID, and forwards to the caller's `webhook_url` with an HMAC-SHA256 signature. Pairs with suppression — hard bounces auto-suppress AND fire the callback.
- **Automatic unsubscribe link injection.** Per-recipient signed tokens, hosted unsubscribe endpoint, RFC 8058 one-click headers. Depends on the suppression list. Opt-in per endpoint.

Plus: HTML body, file attachments, multiple outputs per endpoint (fan-out). Existing v1.x code paths continue to work unchanged.

## Architectural decisions log

**ADR-1: No Postmark SDK.** A third-party SDK adds a dependency we'd have to track and update for every Postmark API change. The bespoke client is ~80 lines, has zero runtime overhead, and gives complete control over error classification. Reconsider in v1.1+ only if 3+ HTTP API transports duplicate enough code to warrant a shared SDK abstraction.

**ADR-2 (revised twice): Internal sub-packages within a single `core/` module.** The original `caddy-formward` design used a flat package (small Caddy-module surface). Posthorn briefly adopted a two-module workspace (`core/` + `caddy/`) when the Caddy adapter was in scope; that workspace was collapsed back to the single `core/` module on 2026-05-15 when the adapter was cut. The sub-package layout (`core/gateway`, `core/transport`, `core/spam`, etc.) is retained because it still earns its keep — `cmd/posthorn` consumes it as a library, and the v1.3 SMTP ingress needs a place to land that doesn't disturb HTTP-form code.

**ADR-3: Token bucket with LRU, not `golang.org/x/time/rate`.** `x/time/rate` is excellent but doesn't bound memory — every distinct key (IP) holds a `Limiter` forever. We need LRU eviction at 10K IPs (NFR6) which requires rolling our own. The implementation is ~30 lines and well-tested. Unchanged from prior design.

**ADR-4: `*bool` for `LogFailedSubmissions`.** A pointer lets us distinguish "operator omitted the field" (default true) from "operator explicitly set false." Pointer-bool is a common Go config pattern despite the awkwardness. Unchanged from prior design.

**ADR-5: Synchronous send, not async with queue.** v1.0 has no persistent storage, so an async queue would be lost on restart. The brief commits to "log on terminal failure" as the recovery mechanism, which works only if the failure is observed in the request log. v2 brings SQLite + async queue together. Unchanged from prior design.

**ADR-6 (retired 2026-05-15): Core has zero Caddy dependency.** Originally the load-bearing decision that made the standalone-plus-adapter architecture work. Trivially true after the Caddy adapter was cut; preserved as a numbered slot for historical traceability.

**ADR-7 (retired 2026-05-15): Standalone is the primary deployment shape; Caddy adapter is optional.** Originally framed the standalone Docker image as the headline distribution with the Caddy adapter as a secondary sibling. After the 2026-05-15 cut, the standalone is the *only* deployment shape; the primary/secondary distinction is gone. Preserved as a numbered slot for historical traceability.

**ADR-8: Per-endpoint in-memory LRU cache for idempotency, not a shared store.** The v1.1 idempotency cache is local-process state: one LRU cache instance per API-mode endpoint, configurable capacity, 24h TTL. We considered three alternatives — a single global cache with prefixed keys, an external Redis dependency, or SQLite-backed durability — and rejected all three. Global-with-prefixes shares the LRU cap across endpoints (one chatty endpoint can evict another's keys), violating the FR41 per-endpoint scoping guarantee by construction risk. Redis adds an operational dependency that contradicts the "single binary, single TOML config" thesis. SQLite-backed durability is the v2 design — exposing it in v1.1 means committing to schema migrations and corruption-handling pre-v2, which the brief explicitly defers. Per-endpoint LRU keeps the v1.1 contract simple, the failure modes obvious (cache evicts → cache miss → caller retries → fresh send), and leaves the v2 swap point at the package interface boundary.

**ADR-9: HTTP 409 on in-flight idempotency-key collision, not blocking.** When two requests arrive concurrently with the same `Idempotency-Key` and the first hasn't completed, the second returns HTTP 409 Conflict immediately rather than waiting for the first to finish. Alternatives considered: block-and-return-same-result (AWS / PayPal style) and reject-with-429-and-retry-after. Block-and-return adds a coordination primitive (per-key wait channel, fairness, timeout interaction with the 10s hard request limit per FR22) for marginal caller convenience; the only callers hitting this race in practice are workers retrying without backoff, which is a caller bug we want to surface rather than mask. 429 implies "try again later because of capacity," misleading when the real signal is "your retry is already in flight." 409 Conflict is the semantically correct status and matches Stripe's documented behavior. Decided during v1.1 FR drafting, 2026-05-16.

**ADR-10: Mode/defense mutex enforced at config-parse time, not request time.** When an operator configures `auth = "api-key"` together with `honeypot`, `allowed_origins`, or any `redirect_*` field, the config fails to parse with an error naming the offending field. Alternative considered: silently ignore the inapplicable fields at request time. Silent ignore creates a footgun — an operator configuring `honeypot = "_gotcha"` on an API-mode endpoint mentally checks "spam-protected" without realizing the field is dead, then thinks the endpoint is defended when it isn't. Parse-time rejection forces the operator to learn the mode model and acts as live documentation. This parallels NFR4's strict treatment of explicitly-empty `allowed_origins` — fail closed and loud, not silent. The narrow cost (operators flipping a single endpoint between modes have to also delete the inapplicable fields) is worth the safety. Decided during v1.1 FR drafting, 2026-05-16.

**ADR-11: Per-request `to_override` is in scope; per-request `from` is not.** API-mode JSON bodies may include a `to_override` field (string or array of strings) that replaces the endpoint's configured recipients for that request. `from` stays endpoint-configured and cannot be overridden per request. The asymmetry follows from where leaked-key blast radius lands:

- **Per-request `to`:** an attacker with a leaked API key can send email *from* the operator's verified domain *to* arbitrary addresses. The operator's domain reputation isn't directly damaged (recipients see legitimate-looking mail from a real domain; provider abuse systems catch volume-based spam patterns). Rate limit + idempotency + Postmark's own abuse detection cap the damage. The alternative — "one endpoint per recipient" — doesn't scale to user-shaped recipients (password resets, receipts, notifications all go to a different user each time). Per-request `to` is the only practical shape for the named v1.1 audience.

- **Per-request `from`:** an attacker with a leaked API key could spoof email *from* arbitrary identities (`from = "ceo@victim.com"`) through the operator's Postmark account. Postmark would reject the send if `from` doesn't match a verified sender signature, but a sophisticated attacker who knows what's verified could spoof within those constraints. Worse, even rejected sends consume rate-limit budget and pollute logs. Endpoint-configured `from` makes spoofing structurally impossible: the operator's TOML names the exact sender identity for each endpoint, and the JSON body cannot override it.

The compromise lets v1.1 actually serve its named audience (Workers doing transactional sends to end users) while preserving the "operator owns sender identity" invariant. Originally deferred under D5 during FR drafting; reversed (partially) the same day after the Cloudflare Worker recipe surfaced that the deferred shape blocked the named audience. Decided 2026-05-16.

**ADR-12: `Ingress` interface produces a complete `transport.Message`.** With SMTP listener landing as a second ingress shape, the existing HTTP handler is generalized behind a new `Ingress` interface in `core/ingress/`. The contract: an Ingress produces a complete, ready-to-send `transport.Message` (From, To, Subject, BodyText, ReplyTo all populated). HTTP form/api-mode ingress finishes template rendering before yielding the Message; SMTP listener parses MIME directly into one. **HTTP-specific concepts (templates, content negotiation, redirects, idempotency keys, CSRF tokens) do not cross the Ingress boundary** — they're handled inside the HTTP ingress and have no SMTP equivalent. The egress side (`Transport`, retry policy, structured logging) is ingress-agnostic and unchanged. Rationale: keeping the Ingress→Message→Transport pipeline narrow (Message is the only data type that crosses both boundaries) lets v2 add new ingress shapes (programmatic/SDK ingress, webhook-triggered ingress, etc.) without touching transport code. Decided 2026-05-16 during v1.3 spec.

**ADR-13: One SMTP listener has one outbound transport.** The SMTP listener config has a single `[smtp_listener.transport]` block — the same shape as `[endpoints.transport]` — and every accepted message routes through it. Multi-tenant routing-by-RCPT (RCPT TO `@brand-a.com` → Postmark account A; RCPT TO `@brand-b.com` → SES account B) is deferred to v2 alongside the per-tenant config isolation story. Rationale: v1.0 SMTP ingress's named use case is "internal app on a homelab speaks SMTP to a single relay" — Ghost admin login, Gitea notifications, a backup script. None of these need multi-tenant routing; they need "a relay that exists and works." Multi-tenant SMTP routing requires per-tenant `from` (sender identity per branded domain), DKIM signing keys per domain, per-tenant suppression — all of which is v2 platform shape. Adding multi-tenant routing in v1.0 would commit us to those follow-ons. Decided 2026-05-16 during v1.3 spec.

**ADR-14: AWS SES SigV4 implemented bespoke (noted exception to ADR-1).** SES transport requires AWS SigV4 request signing — the most complex auth scheme of any v1.0 transport. We implement SigV4 bespoke (`core/transport/awssigv4.go`) following AWS's published canonical-request specification, then SES on top (`core/transport/ses.go`). Decided 2026-05-16 during v1.2 spec; the original wording set a "stop and surface if total LOC > 500" tripwire. Tripwire fired at implementation time — final LOC: 229 (awssigv4.go) + 264 (ses.go) = 493 implementation, plus 220 + 445 = 665 tests, for 1,158 total.

After the tripwire fired the deviation was accepted (option (a)) for these reasons:

- The "total LOC including tests" tripwire was overly conservative. Test bulk in this codebase is uniformly 400-500 LOC per transport (Postmark, Resend, Mailgun all hit similar bulk) because of the table-driven header-injection suite, status-code-mapping suite, and registry-integration suite each transport gets. Tests scale with quality bar, not transport complexity.
- The honest binding constraint is implementation LOC (493) which is 2.5× ADR-1's "~200 lines suffices" heuristic but comfortably under its "1000+ → use SDK" line.
- Bringing in `github.com/aws/aws-sdk-go-v2/service/sesv2` trades the ~400 LOC of bespoke for a 30+ package transitive dependency tree — net negative against ADR-1's spirit.
- The SigV4 primitive is **reusable** for any future AWS-signed transport (S3-backed storage in v2, SNS lifecycle webhooks in v2). The 229 LOC of SigV4 is forward-value, not single-use cost.
- The implementation is well-tested: NFR1 header-injection table, NFR3 secret-not-in-headers, deterministic signature output across runs, key derivation matches AWS's published example hex value (`c4afb1cc...4a4b9` for the canonical inputs).

The tripwire as written was a useful forcing function — it made us look at the numbers before letting bespoke creep go unchallenged. Future AWS-shaped transports (S3, etc.) inherit the SigV4 primitive at near-zero cost; the 493-LOC budget covers the lifetime cost of all AWS-signed transports in the project, not just SES.

**ADR-15: Prometheus `/metrics` exposition is hand-rolled.** The standard `github.com/prometheus/client_golang` library would pull in `procfs`, `expvar` bridges, descriptor types — likely 30+ transitive packages. The text exposition format itself is stable, plaintext, and ~50 LOC to emit by hand for the v1.0 metrics surface (counters, histograms with fixed buckets, labeled by `endpoint`/`transport`/`error_class`). Parallel to ADR-1: bespoke when the surface is small. v2's broader observability story may justify the dep when histogram buckets become user-configurable or when push-based remote-write enters scope; not v1.0. Decided 2026-05-16 during v1.2 spec.

**ADR-16: CSRF tokens are HMAC-signed timestamps, issued by the operator at form-render time.** Posthorn does not issue cookies (it has no UI surface) and cannot issue tokens at form-load time (it doesn't render forms). The CSRF design instead: operator configures `csrf_secret` in TOML; operator's static-site builder or server-rendered template code computes `_csrf_token = <timestamp>.<HMAC-SHA256(csrf_secret, timestamp)>` at form-render time and embeds it as a hidden input; Posthorn verifies the HMAC and TTL on submit. The secret never crosses to the client. Alternatives rejected: (a) Posthorn-issued tokens via cookie — adds cookie state and a token-fetch round-trip, contradicts "stateless gateway" identity; (b) double-submit cookie pattern — same cookie issue; (c) no CSRF at all — leaves operators with only Origin/Referer + honeypot for defense, weaker than industry baseline. Operators using static-site generators (Hugo, Jekyll, Astro) can compute tokens at build time; SaaS operators with dynamic forms compute them in their own server-rendered template. Form-mode only; api-mode endpoints reject `csrf_secret` at parse time (no browser form rendering involved). Decided 2026-05-16 during v1.2 spec.

**ADR-17: Outbound-SMTP and inbound-SMTP both use stdlib `net/smtp` primitives.** Go's `net/smtp` is marked "frozen, no new features" but is functionally complete for the operations Posthorn needs (DIAL, AUTH PLAIN/LOGIN, STARTTLS, MAIL/RCPT/DATA, QUIT). Outbound-SMTP transport uses `smtp.PlainAuth` + `smtp.NewClient`. Inbound SMTP listener is hand-rolled (no Go stdlib SMTP server exists — `net/smtp` is client-only) but uses `crypto/tls` and `net` directly — same posture as `net/http` is for HTTP. Parallel to ADR-1 / ADR-15: stdlib-first when the surface is small enough. If we hit a real limitation (e.g., XCLIENT for proxy chains, PROXY protocol on the listener), reconsider with an ADR amendment and possibly adopt `github.com/emersion/go-smtp`. Decided 2026-05-16 during v1.2 and v1.3 spec.

If you find yourself wanting to deviate from any ADR, update this document with the new decision and rationale before changing code.

## Open architectural questions

These are implementation decisions deferred from the brief and PRD that affect code organization. Each has a recommended answer; final decision is made during the relevant story.

1. **Logging library: `log/slog` (stdlib) vs `zap`.** Recommendation: `slog` (zero deps, stdlib). Decided during Story 4.2.

2. **`trusted_proxies` syntax in v1.0.** Decision: CIDR-only. Named presets (`cloudflare`, etc.) are planned for v1.2 (see brief §"Post-MVP Vision"). Confirmed during Story 3.2.

3. **Body template — file path vs inline detection.** Recommendation: heuristic — if the value contains `{{` it's inline; otherwise it's a file path. Reject ambiguity at validation time. Decided during Story 2.4.

4. **Response body for 502 terminal failures.** Recommendation: 502 response body says "Submission could not be delivered. Please try again later." with no detail. Detail is in the operator's logs. Avoiding leaking whether the failure was config (4xx from upstream) vs runtime (network) to a potential attacker. Decided during Story 4.1.

5. **JSON body type coercion in API mode (v1.1).** The form-mode pipeline produces `map[string][]string` and templates render strings. JSON request bodies can contain numbers, booleans, arrays, and nested objects. Recommendation: coerce primitive types to strings (`42` → `"42"`, `true` → `"true"`) using `fmt.Sprintf` and reject nested objects with HTTP 400 ("nested JSON objects are not supported in v1.1"). Top-level arrays of primitives serialize as comma-joined strings (parallel to form-mode multi-value fields). Decided during Story 8.3.

## Appendix: TOML grammar reference

For Story 2.1 implementation. Single-endpoint shape:

```toml
[[endpoints]]
path = "/api/contact"
to = ["craig@example.com"]
from = "Contact Form <noreply@example.com>"

trusted_proxies = ["10.0.0.0/8", "192.168.0.0/16"]
honeypot = "_gotcha"
allowed_origins = ["https://example.com"]
max_body_size = "32KB"

required = ["name", "email", "message"]
email_field = "email"     # default: "email"

subject = "Contact from {{.name}}"
body = "templates/contact.txt"   # file path; inline allowed if value contains "{{"

log_failed_submissions = true

redirect_success = "/thank-you"
redirect_error = "/contact?error=true"

[endpoints.transport]
type = "postmark"

[endpoints.transport.settings]
api_key = "${env.POSTMARK_API_KEY}"
# base_url = "https://api.postmarkapp.com"  # test-only, undocumented

[endpoints.rate_limit]
count = 5
interval = "1m"

[logging]
level = "info"
format = "json"   # only json supported in v1.0
```

For multiple endpoints, repeat the `[[endpoints]]` block. Each subsequent `[endpoints.transport]`, `[endpoints.transport.settings]`, and `[endpoints.rate_limit]` table applies to the most recent `[[endpoints]]` entry.

