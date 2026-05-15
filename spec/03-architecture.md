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
    │   ├── transport_test.go
    │   └── postmark_test.go
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
    └── log/
        ├── log.go                # Structured logging helpers, submission ID
        └── log_test.go
```

Sub-packages are organized one-per-concern. The original `caddy-formward` design used a flat package; Posthorn's broader scope (standalone binary, future SMTP ingress) justifies the package boundaries even without the adapter as a second consumer.

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

## Forward compatibility (v1.x roadmap)

Architectural commitments that protect future scope. Version numbers updated 2026-05-15 to match the restructured roadmap (see brief §"Post-MVP Vision").

These commitments serve the project's [Design principles](./01-project-brief.md#design-principles) in the brief — especially "Gateway, not infrastructure" (principle #1) and "Integration layer, not mail-receiving layer" (principle #2). When adding a new forward-compat commitment, check both the principles and the existing ADRs (below); architecture decisions that contradict the principles need spec discussion first.

### v1.1 (API mode)

A second auth + ingress shape alongside the v1.0 form-mode pipeline. None of the additions break the existing form-mode path.

- New `EndpointConfig.Auth` field with values `"form"` (default; v1.0 behavior) and `"api-key"`. The handler routes per-mode: `"api-key"` mode skips Origin/Referer + honeypot checks, requires `Authorization: Bearer <key>`, accepts `application/json` body, and applies the same rate limit + transport pipeline.
- JSON parsing on API-mode endpoints produces the same `url.Values`-shaped map the form parser produces, so template rendering, validation, and the transport layer are unchanged.
- Idempotency keys: new package `core/idempotency/` providing an LRU cache keyed on `(api_key, idempotency_key)`. v1.1 is in-memory; v2 swaps the implementation for SQLite-backed without changing the interface. 24-hour TTL.
- Batch send: new endpoint config field `batch = true`. Handler reads a JSON array of recipients, loops template render per recipient, calls `Transport.SendBatch` (new optional interface method — falls back to looping `Send` for transports that don't implement it; Postmark transport implements it via `/email/batch`).

### v1.2 (multi-transport + operational maturity)

The `Transport` interface accepts arbitrary configurations via `TransportConfig.Settings map[string]any`. Adding Resend, Mailgun, SES, outbound-SMTP requires new files in `core/transport/` (e.g., `resend.go`, `mailgun.go`, `ses.go`, `smtp.go`) and a registration step in the config loader. Zero changes to handler logic, zero breaking config changes.

Each new transport must:
- Implement `Transport.Send`
- Return `*TransportError` with correct `ErrorClass`
- Pass the header-injection test suite (NFR2 applies to every transport)
- Pass the no-key-in-logs test suite (NFR3 applies to every transport)

Operational additions:
- `/healthz` and `/metrics` endpoints registered on the main HTTP listener at fixed paths (not as configurable endpoints). Metrics exposed in Prometheus exposition format — submission count, latency histograms, error class breakdown, per-transport split.
- Dry run mode: handler-level flag (per endpoint or global) that runs the full pipeline up to `Transport.Send` and short-circuits with a 200 response containing the prepared `Message`.
- CSRF + time-based form tokens: new `spam` subpackage additions, opt-in per endpoint. Form-mode only.

### v1.3 (SMTP ingress)

The architectural fork that v1.0 has been protecting against. The commitment:

- The `Message` struct is the boundary between ingress and egress. It does not change. SMTP ingress parses a MIME message into a `Message`; HTTP form ingress builds a `Message` from form fields + templates. Egress doesn't care which.
- An `Ingress` interface will be defined in v1.3 to abstract over "thing that produces Messages." HTTP form ingress is the implicit first instance in v1.0; SMTP ingress is the second in v1.3.
- Config gets a new top-level section: `smtp_listener:` (parallel to `endpoints:`). Existing `endpoints:` config remains valid and unchanged.
- The `cmd/posthorn` binary gains a new code path that starts both an HTTP listener (if `endpoints` are configured) and an SMTP listener (if `smtp_listener` is configured). Both share the same logger, transport pool, and graceful-shutdown machinery.

The threat model expansion for v1.3 (open relay, RCPT bombing, etc.) is deferred to that version's spec rewrite. The architectural commitment here is that v1.0 does not foreclose those defenses: the spam package (`core/spam`) is HTTP-form-specific and a separate `core/smtpspam` package can land in v1.3 without disturbing it.

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

If you find yourself wanting to deviate from any ADR, update this document with the new decision and rationale before changing code.

## Open architectural questions

These are implementation decisions deferred from the brief and PRD that affect code organization. Each has a recommended answer; final decision is made during the relevant story.

1. **Logging library: `log/slog` (stdlib) vs `zap`.** Recommendation: `slog` (zero deps, stdlib). Decided during Story 4.2.

2. **`trusted_proxies` syntax in v1.0.** Decision: CIDR-only. Named presets (`cloudflare`, etc.) are planned for v1.2 (see brief §"Post-MVP Vision"). Confirmed during Story 3.2.

3. **Body template — file path vs inline detection.** Recommendation: heuristic — if the value contains `{{` it's inline; otherwise it's a file path. Reject ambiguity at validation time. Decided during Story 2.4.

4. **Response body for 502 terminal failures.** Recommendation: 502 response body says "Submission could not be delivered. Please try again later." with no detail. Detail is in the operator's logs. Avoiding leaking whether the failure was config (4xx from upstream) vs runtime (network) to a potential attacker. Decided during Story 4.1.

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

