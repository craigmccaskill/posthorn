---
title: "PRD: Posthorn v1.0"
status: locked
created: 2026-04-27
synced_from_obsidian: 2026-04-27
---

# PRD: Posthorn v1.0

This document translates the locked decisions in [the project brief](./01-project-brief.md) into formal functional requirements (FRs), non-functional requirements (NFRs), and an epic-and-story breakdown sized for implementation. Every FR and NFR derives from a commitment in the brief; nothing here introduces new scope.

Sections below are grouped into four feature blocks (A, B, C, D) inside a single v1.0 release. Block A (FR1–FR26, NFR1–NFR18, Epics 1–7) is the originally locked v1.0 scope. Blocks B (API mode), C (multi-transport + ops), and D (SMTP ingress) were originally scoped as v1.1 / v1.2 / v1.3 themed releases and folded into v1.0 on 2026-05-16; see the brief's status log for the reasoning. v2 (stateful platform features — durable storage, suppression, lifecycle webhooks, attachments) remains future scope.

> **2026-05-15 amendment:** The Caddy v2 adapter module was cut from v1.0 pre-tag (see the brief's status log for the product reasoning). Original FR27–FR30 and NFR10 are deleted in-place below; Epic 6 (Caddy adapter) is retired; Stories 1.2 and 1.3 (workspace restructure) and Story 7.3 (modules-page PR) are noted with the cut. The standalone-behind-any-reverse-proxy deployment shape is now the only one in scope.

> **2026-05-16 amendment:** v1.1 scope added. After v1.0 implementation completed and operator validation began, the v1.1 "API mode" features were spec'd into this document as a coherent amendment: FR31–FR46, NFR19–NFR21, and a new Epic 8. Batch send was originally listed in the brief as a fourth v1.1 feature; it was dropped on 2026-05-16 while drafting these FRs after determining no named v1.1 audience workload required it. The deferral is recorded in the brief's status log and the "Deliberately not on the roadmap" section. v1.1 scope is now: API-key auth per endpoint, JSON content type on API-mode endpoints, idempotency keys, and per-request recipient override (FR46).
>
> **2026-05-16 (later same day):** FR46 added after the Cloudflare Worker recipe surfaced that the named v1.1 audience (Workers sending transactional email to different end users) needs per-request recipient routing. The original design decision D5 (defer per-request `to`/`from` overrides) was correct about `from` (spoofing risk via leaked keys) but wrong about `to` (the audience cannot pre-declare every recipient in config). `from` stays endpoint-configured; `to_override` is added.

## Goals

Defer to [the project brief](./01-project-brief.md) §"Goals and Success Metrics". Two reminders that shape decisions in this document:

- **Done:** Author's blog runs on Posthorn for the contact form for 30 days with zero dropped submissions; README has a copy-pasteable Docker Compose example verified end-to-end; tagged v1.0.0 with Docker image
- **Worked:** External user in production within 90 days + Ghost roundtrip via v1.3 SMTP ingress within 6 months

## Functional requirements

Each FR is atomic, testable, and traceable to a section of the brief. FRs are grouped by concern, not priority — that comes via the epic structure below.

### Ingress (HTTP form)

**FR1.** The gateway **must** accept form-encoded POST submissions (`application/x-www-form-urlencoded` and `multipart/form-data`) on configured paths.

**FR2.** The gateway **must** support multiple independent endpoint configurations. Each endpoint **must** have its own recipients, transport, rate limit, templates, and spam-protection settings without state interaction with other endpoints.

### Egress (transports)

**FR3.** The gateway **must** ship a Postmark HTTP API transport that accepts a configurable API key (via `{env.VAR}` substitution) and sends submissions as email through `https://api.postmarkapp.com/email`.

**FR4.** The gateway **must** expose a pluggable `Transport` interface such that additional transports (Resend, Mailgun, SES, outbound SMTP) can be added in v1.2+ without modifying handler logic.

### Spam protection

**FR5.** The gateway **must** support a configurable honeypot field. When the configured field name has a non-empty value on submission, the gateway silently rejects the request with HTTP 200 (so bots cannot distinguish honeypot rejection from success).

**FR6.** The gateway **must** validate the `Origin` and `Referer` headers against a configurable `allowed_origins` list. If `allowed_origins` is configured and *both* headers are missing, the gateway **must** reject the request (fail-closed). If `allowed_origins` is not configured, the gateway **must** allow any origin (fail-open by absence of config).

**FR7.** The gateway **must** enforce a configurable maximum request body size, applied via `http.MaxBytesReader`. Submissions exceeding the limit **must** receive HTTP 413.

**FR8.** The gateway **must** implement a token-bucket rate limiter per endpoint, configurable as `rate_limit: <count> per <interval>` (e.g., `5 per 1m`). Requests exceeding the limit **must** receive HTTP 429.

**FR9.** The rate limiter **must** key on the client IP. The gateway **must** support a `trusted_proxies` configuration that, when set, causes the limiter to read the client IP from `X-Forwarded-For` (rightmost untrusted IP) instead of the connection's RemoteAddr.

### Validation

**FR10.** The gateway **must** support a configurable list of required fields. Submissions missing or empty for any required field **must** receive HTTP 422 with a JSON response body identifying the specific field(s) that failed validation.

**FR11.** The gateway **must** validate the format of the submitter's email field (the field designated as the email address) using a basic syntactic check. Submissions with malformed email addresses **must** receive HTTP 422.

### Email rendering

**FR12.** The gateway **must** render the email subject and body using Go's `text/template`, with form fields available as template variables. Operators **must** be able to specify the body template via a file path or inline string.

**FR13.** Form fields not explicitly named in the configuration (i.e., not in the required list, not the email field, not the honeypot) **must** be appended to the rendered email body in a structured "Additional fields" block.

### Response handling

**FR14.** The gateway **must** return JSON responses with these HTTP status codes:
  - `200 OK` on successful send
  - `400 Bad Request` on malformed request (e.g., not form-encoded)
  - `413 Payload Too Large` on body-size exceeded
  - `422 Unprocessable Entity` on validation failure
  - `429 Too Many Requests` on rate limit exceeded
  - `502 Bad Gateway` on terminal transport failure

**FR15.** The gateway **must** detect the request's `Accept` header. If `application/json` is preferred, return JSON. If `text/html` is preferred (default browser form submit) and `redirect_success` / `redirect_error` are configured, return a 303 See Other redirect.

**FR16.** The gateway **must** support `redirect_success` and `redirect_error` URL configuration. If unconfigured, content negotiation **must** fall back to JSON regardless of `Accept` header.

### Operational

**FR17.** The gateway **must** emit structured (JSON) log entries for: submission received, submission accepted, submission sent, send retried, send failed (terminal), spam blocked (with reason), rate limit exceeded, validation failed (with field). Each log entry **must** include the endpoint path and a unique submission ID.

**FR18.** The gateway **must** support a `log_failed_submissions` boolean configuration (default `true`). When `true`, terminal send failures log the full submission payload (form fields and headers) at ERROR level so the operator can recover the data from logs. When `false`, only metadata is logged.

### Failure handling

**FR19.** On a transient transport error (network failure, transport API HTTP 5xx), the gateway **must** retry the send exactly once after a 1-second delay.

**FR20.** On a transport `429 Too Many Requests` response, the gateway **must** retry the send exactly once after a 5-second delay.

**FR21.** On a transport `4xx` response other than `429` (e.g., `401 Unauthorized` for a bad API key, `422` for an invalid recipient), the gateway **must not** retry. The gateway **must** treat this as a terminal failure.

**FR22.** The gateway **must** enforce a hard 10-second timeout on the entire request, including any retry attempts. If the timeout is reached, any in-flight retry **must** be cancelled and the request **must** terminate with a terminal failure.

**FR23.** On terminal failure, the gateway **must** log the failure (and full payload if `log_failed_submissions=true`) at ERROR level and return HTTP 502 to the client.

### Standalone deployment

**FR24.** The standalone binary **must** load configuration from a TOML file specified via `--config <path>` CLI flag. The config file **must** support `${env.VAR}` placeholder resolution for secrets.

**FR25.** The standalone binary **must** expose a single primary CLI subcommand: `posthorn serve --config <path>`. A `validate` subcommand (`posthorn validate --config <path>`) **must** parse and validate the config without starting the listener.

**FR26.** The standalone binary **must** handle SIGTERM and SIGINT for graceful shutdown: stop accepting new connections, drain in-flight requests up to the 10-second per-request timeout, then exit with code 0. Forced exit on second signal.

### Caddy adapter (deleted 2026-05-15)

FR27–FR30 originally specified a Caddy v2 adapter module. They were deleted pre-tag along with the adapter itself. Sequence numbers are retained for historical traceability — do not reuse them for new requirements.

## Non-functional requirements

### Security

**NFR1.** Submitter-controlled fields **must never** be interpolated as raw strings into email headers. All header construction **must** pass through transport library APIs that handle headers as structured data. The Postmark transport **must** use Postmark's JSON API field for `From`, `To`, `Reply-To`, and `Subject`, never constructing headers via string concatenation.

**NFR2.** The test suite **must** include explicit cases verifying that header injection payloads (CRLF in name/email/subject fields, `\r\nBcc:` injection attempts, header smuggling sequences) do not produce unintended headers in outbound mail.

**NFR3.** API keys configured via `${env.VAR}` placeholders **must not** appear in any log output, including error logs. Tests **must** verify this by triggering transport failures and asserting the API key string does not appear in captured log output.

**NFR4.** The config parser **must** reject configurations where `allowed_origins` is explicitly empty (an empty list, not unset). No fail-open default for an explicitly empty allowlist.

### Performance

**NFR5.** Total request latency **must** be bounded by the 10-second hard timeout (FR22). The gateway **must not** introduce unbounded waits.

**NFR6.** The rate limiter's per-IP token buckets **must** have a configurable maximum number of tracked IPs, with an LRU eviction policy when full. Default: 10,000 tracked IPs.

### Observability

**NFR7.** All log events **must** include structured fields: `submission_id`, `endpoint`, `transport`, `latency_ms`, `error_class` (where applicable). No free-text-only log lines for production events.

**NFR8.** Submission IDs **must** be UUIDs (v4) generated on receipt and propagated through every log line for that request.

### Compatibility

**NFR9.** The standalone binary **must** build and run on Go 1.25+. The `go.mod` file **must** declare this as the minimum version.

**NFR10.** *(Deleted 2026-05-15 along with FR27–FR30.)* Originally specified the standalone-core-zero-Caddy-dependency invariant.

**NFR11.** The TOML config syntax **must** remain stable within the v1 major version. Adding optional new fields is permitted; removing or renaming existing ones is not.

### Distribution & build

**NFR12.** The standalone binary **must** be installable via `go install github.com/craigmccaskill/posthorn/cmd/posthorn@latest` and as a Docker image at `ghcr.io/craigmccaskill/posthorn`.

**NFR13.** The Docker image **must** be multi-arch (linux/amd64 and linux/arm64).

**NFR14.** *(Deleted 2026-05-15 along with FR27–FR30.)* Originally specified the `xcaddy build` installability of the Caddy adapter.

**NFR15.** The repository **must** be licensed Apache-2.0, with a `LICENSE` file in the repository root.

### Documentation

**NFR16.** The README **must** include a complete, copy-pasteable Docker Compose example verified end-to-end on a clean install.

**NFR17.** The README **must** document DNS prerequisites for production use: SPF, DKIM, and DMARC records for the sending domain.

**NFR18.** Every TOML config field **must** be documented in the README (or the linked docs site) with at least one example value and a description of its behavior.

## Functional requirements — block B: API mode (added 2026-05-16)

Block B introduces "API mode" — endpoint configurations that authenticate server-to-server callers instead of relying on browser-shaped spam defenses. Block A form-mode endpoints are unaffected (FR45). Three coherent features: API-key auth (FR31–FR35), JSON content type (FR36–FR39), idempotency (FR40–FR44). Originally scoped as v1.1; folded into v1.0 on 2026-05-16 alongside blocks C and D (see brief status log).

### Mode selection

**FR31.** Each endpoint **must** support an `auth` field with values `"form"` (default) or `"api-key"`. Omitting the field **must** preserve v1.0 behavior. The field **must** be exclusive — no endpoint can be both modes.

**FR32.** API-mode endpoints **must** reject (at config-parse time) any of: `honeypot`, `allowed_origins`, `redirect_success`, `redirect_error`. Mixing browser defenses with API-mode auth must be a parse error with a clear message naming the offending field.

### API-key auth

**FR33.** API-mode endpoints **must** require a non-empty `api_keys` list (each value supporting `${env.VAR}` substitution). An API-mode endpoint without `api_keys`, or with an empty `api_keys = []`, **must** be rejected at config parse (fail-closed, analogous to NFR4).

**FR34.** API-mode endpoints **must** accept the API key via `Authorization: Bearer <key>` header. Comparison **must** be constant-time. Failed auth (missing header, wrong scheme, no matching key) **must** return HTTP 401.

**FR35.** On API-mode endpoints, the rate limiter **must** key on the matched API key value (not the client IP). The `trusted_proxies` / `X-Forwarded-For` logic in FR9 does not apply to API-mode endpoints.

### JSON content type

**FR36.** API-mode endpoints **must** accept `application/json` request bodies. The body **must** be a JSON object whose keys are template variables (same flat-key semantics as form-mode field names).

**FR37.** API-mode endpoints **must** reject non-JSON request bodies with HTTP 415 Unsupported Media Type. Form-encoded bodies on an API-mode endpoint return 415, not silent acceptance.

**FR38.** All v1.0 validation FRs (FR10 required fields, FR11 email format) **must** apply identically to API-mode JSON submissions.

**FR39.** API-mode JSON submissions **must** support the same custom-fields passthrough block (FR13) for keys not explicitly named in the template.

### Idempotency

**FR40.** API-mode endpoints **must** honor the `Idempotency-Key` request header. When present and well-formed, the gateway **must** serve a cached response for duplicate keys within a 24-hour TTL.

**FR41.** Idempotency keys **must** be scoped per-endpoint. The same key on different endpoints does not collide.

**FR42.** The idempotency cache **must** be in-memory with LRU eviction. Default capacity 10K entries per endpoint; configurable via `idempotency_cache_size`. Durable persistence across restarts is deferred to v2.

**FR43.** Idempotency keys **must** be 1–255 characters of printable ASCII. Malformed keys **must** return HTTP 400 with a clear error.

**FR44.** When a request arrives with an idempotency key that matches an in-flight (not-yet-completed) request, the gateway **must** return HTTP 409 Conflict. No blocking/queuing.

### Per-request recipient override

**FR46.** API-mode endpoints **must** honor an optional `to_override` field in the JSON request body. When present, the value (a JSON string or an array of strings) **must** replace the endpoint's configured `to` list for that request only. Each entry **must** pass the same email-syntax validation as FR11; any failure returns HTTP 422. An empty array (`"to_override": []`) **must** be rejected as invalid. When `to_override` is absent, the endpoint's configured `to` list applies (preserving FR45). The `from` field is **not** overridable per request — sender identity stays endpoint-configured to prevent spoofing via leaked API keys.

### Backwards compatibility

**FR45.** All v1.0 endpoint behavior (form-mode, the default) **must** be unchanged. v1.0 configurations **must** parse, run, and behave identically.

## Functional requirements — block C: Multi-transport + operational maturity (added 2026-05-16)

Block C adds the multi-transport surface and operational features that take Posthorn from "form gateway with one provider" to "outbound mail layer." Originally scoped as v1.2; folded into v1.0 on 2026-05-16 alongside blocks B and D (see brief status log). Two feature sub-blocks: transports (FR47–FR53) and operational maturity (FR54–FR59).

### Transports

**FR47.** Posthorn **must** ship a Resend HTTP API transport (`type = "resend"`). API key supplied via `Authorization: Bearer` header; request body is JSON to `https://api.resend.com/emails`. The transport **must** classify upstream errors per the existing `ErrorClass` taxonomy (transient/rate-limited/terminal) and populate `SendResult.MessageID` on success.

**FR48.** Posthorn **must** ship a Mailgun HTTP API transport (`type = "mailgun"`). API key supplied via HTTP Basic auth (`api:<key>`); request body is multipart/form-data to `https://api.mailgun.net/v3/<domain>/messages`. Required config: `api_key`, `domain`. Same `ErrorClass` + `MessageID` contract as FR47.

**FR49.** Posthorn **must** ship an AWS SES HTTP API transport (`type = "ses"`). AWS SigV4 authentication; JSON request body to the SESv2 `SendEmail` endpoint. Required config: `access_key_id`, `secret_access_key`, `region`. Same `ErrorClass` + `MessageID` contract as FR47.

**FR50.** Posthorn **must** ship an outbound-SMTP transport (`type = "smtp"`). STARTTLS-enabled (`require_tls = true` default); SMTP AUTH PLAIN or LOGIN. Required config: `host`, `port`, `username`, `password`. The transport's `SendResult.MessageID` is the SMTP server's response to the final `.` (e.g., `250 OK queued as <id>`); when the upstream doesn't return a parsable ID, MessageID stays empty.

**FR51.** Every transport added in v1.2 **must** honor the existing `Transport` contract — `Send` returns `(SendResult, error)`, populates `MessageID` where the provider exposes one, and classifies errors as `*TransportError` with the correct `ErrorClass`.

**FR52.** Every transport added in v1.2 **must** be subject to NFR1 (header injection prevention). Submitter-controlled fields **must** reach the upstream provider through API library calls (`json.Marshal`, `mime/multipart`, `net/smtp` writers), never via raw string concatenation into protocol-level headers. The header-injection test suite (NFR2) **must** include explicit cases for every transport.

**FR53.** Every transport added in v1.2 **must** be subject to NFR3 (API keys never logged). Each transport's tests **must** include the NFR3 sentinel-token pattern: a known-distinctive key value is configured, a failure path is triggered, and captured logs are asserted to not contain the sentinel.

### Operational features

**FR54.** Posthorn **must** expose a `/healthz` endpoint that returns `200 OK` with body `ok` when the listener is healthy. The path is fixed (not configurable as an endpoint) and lives on the same HTTP listener as configured endpoints. `/healthz` **must not** require authentication.

**FR55.** Posthorn **must** expose a `/metrics` endpoint in Prometheus text exposition format. Metrics **must** include: submissions received, submissions sent, submissions failed (by `error_class`), send latency histogram, rate-limit-hit count, and 401/403/409/429/422 response counts. Each metric **must** carry `endpoint` and `transport` labels (operator-configured names; never submitter-controlled values per NFR24). The endpoint is fixed-path; auth and rate-limit don't apply. Operators concerned about exposure can firewall the path at the reverse proxy.

**FR56.** Posthorn **must** support per-endpoint `dry_run` config (default `false`). When `true`, the handler runs the full pipeline up to (but not including) `transport.Send` and returns `200 OK` with a JSON body containing the prepared `transport.Message` (from, to, subject, body, reply-to). Operators use this to debug template rendering and recipient resolution without sending mail.

**FR57.** Posthorn **must** support CSRF token verification on form-mode endpoints via `csrf_secret` (a HMAC-SHA256 key supplied via `${env.VAR}` substitution) and `csrf_token_ttl` (default 1h). When configured, the endpoint **must** require a `_csrf_token` form field whose value decomposes as `<timestamp>.<hmac>` where `hmac = HMAC-SHA256(csrf_secret, timestamp)`. Missing, malformed, or expired tokens **must** return `403`. Operator issues tokens server-side at form-render time (the secret never crosses to the client); see ADR-16. Form-mode only — api-mode endpoints reject `csrf_secret` config at parse time.

**FR58.** Posthorn **must** support named presets for `trusted_proxies` in addition to CIDR ranges. v1.0 ships these presets: `cloudflare`, `aws-elb`, `gcp-lb`, `azure-front-door`. A preset expands to a maintained CIDR list inside Posthorn at parse time; updates to a provider's IP ranges require a Posthorn release. Operators mix presets and explicit CIDRs in one `trusted_proxies` list.

**FR59.** Each endpoint **must** support `strip_client_ip` (default `false`). When `true`, the resolved client IP **must** be omitted from all log lines emitted for that endpoint, including `rate_limited` and `submission_failed`. Other identifying fields (submission_id, transport, latency_ms) continue to log. The setting addresses GDPR-context operators who don't want submitter IPs persisted in logs; it does not affect rate-limiter keying.

## Functional requirements — block D: SMTP ingress (added 2026-05-16)

Block D adds SMTP ingress — the strategic feature that completes the gateway thesis. Internal apps that emit SMTP (Ghost's admin login, Gitea's notifications, legacy on-prem systems) can hit a Posthorn instance instead of a real SMTP server; Posthorn parses the MIME, builds a `transport.Message`, and forwards via the configured HTTP API transport. Posthorn is **not** a mail server — it doesn't host mailboxes, doesn't act as an MX, doesn't do inbound receive-side spam filtering. It's an authenticated relay for known internal clients only. Originally scoped as v1.3; folded into v1.0 on 2026-05-16.

### Ingress interface

**FR60.** Posthorn **must** define an `Ingress` interface in `core/ingress/` that abstracts over "thing that produces a `transport.Message`." Both the existing HTTP form/api-mode handler and the new SMTP listener (FR62+) implement this interface. See ADR-12 for the design.

**FR61.** Existing HTTP form-mode and api-mode handlers **must** continue to function unchanged after the `Ingress` interface extraction. The refactor is behavior-preserving; the existing test suite proves it.

### SMTP listener

**FR62.** Posthorn **must** support an SMTP listener as an alternative ingress alongside HTTP. The listener accepts SMTP from internal clients on a configured TCP port via a new top-level `[smtp_listener]` config section. When configured, `cmd/posthorn serve` starts both an HTTP listener (for `[[endpoints]]`) and an SMTP listener.

**FR63.** The SMTP listener **must** require either SMTP AUTH (PLAIN or LOGIN) or client-cert authentication. Unauthenticated connections **must** be rejected after EHLO/HELO but before MAIL FROM with SMTP code `530`. The required mode is selected via `smtp_listener.auth_required = "smtp-auth" | "client-cert" | "either"`.

**FR64.** The SMTP listener **must** enforce a sender allowlist via `smtp_listener.allowed_senders` (list of email addresses or `*@domain.com` wildcards). MAIL FROM values not matching the allowlist **must** be rejected with `550 5.7.1 Sender not authorized`.

**FR65.** The SMTP listener **must** enforce open-relay prevention via either `smtp_listener.allowed_recipients` (allowlist; same wildcard syntax as FR64) or `smtp_listener.max_recipients_per_session` (numeric cap, default 10). One of the two **must** be configured at parse time. RCPT TO values exceeding either bound are rejected with `550 5.7.1 Recipient not authorized` or `452 4.5.3 Too many recipients`.

**FR66.** The SMTP listener **must** enforce a maximum message size via `smtp_listener.max_message_size` (default 1MB). DATA blobs exceeding the size limit are rejected with `552 5.3.4 Message too big`.

**FR67.** The SMTP listener **must** enforce STARTTLS via `smtp_listener.require_tls` (default `true`). When `true`, the listener advertises `STARTTLS` in EHLO response; clients that proceed to MAIL FROM without upgrading the connection are rejected with `530 5.7.0 Must issue STARTTLS first`.

**FR68.** The SMTP listener **must** parse the DATA payload as MIME, construct a `transport.Message`, and pass it through the configured transport. The transport is configured via `smtp_listener.transport` (same shape as `endpoints.transport`). One SMTP listener has exactly one outbound transport; multi-tenant routing-by-RCPT is deferred to v2 (see ADR-13).

## Non-functional requirements — block B (added 2026-05-16)

**NFR19.** API-key comparison **must** use `crypto/subtle.ConstantTimeCompare`. Tests **must** verify the constant-time pattern is in source (not benchmarked).

**NFR20.** The idempotency cache **must** store the complete original response (status code, body, submission_id, transport_message_id). Replays **must** return byte-identical responses.

**NFR21.** API keys configured via `${env.VAR}` **must** be subject to NFR3 (never appear in log output). Tests **must** include explicit cases triggering auth failures and asserting the key string does not appear in captured logs.

## Non-functional requirements — blocks C and D (added 2026-05-16)

**NFR22.** The SMTP listener **must** be subject to NFR1 — MIME parsing **must not** allow inbound CRLF sequences to construct unintended outbound headers in the egress transport. Tests **must** include explicit cases for header smuggling via crafted MIME (`Bcc:` in a header value, `\r\n` in a subject, multipart boundary confusion).

**NFR23.** The SMTP listener **must** emit a per-session structured log line at session start (after EHLO/HELO) including the client IP, AUTH user (or `anonymous`), TLS state (or `plain`), and a session_id (UUIDv4). Each subsequent log line for the session **must** carry the session_id.

**NFR24.** The `/metrics` endpoint **must not** include any submitter-controlled values as label values — labels carry only operator-configured names (endpoint path, transport type, error class). High-cardinality submitter content (recipient addresses, subjects, body fragments) is forbidden as it enables cardinality-explosion attacks against the metrics scraper.

## Testing strategy

The brief commits to test coverage for header injection (NFR2) and to a 30-day production trial. Beyond those, the v1 testing strategy is:

| Layer | Approach |
|-------|----------|
| Unit | Table-driven Go tests for each component (validation, rate limiter, templating, content negotiation, error classification, config loader) |
| Transport integration | `httptest.NewServer` mock standing in for Postmark API; covers retry behavior, error classification, timeout enforcement |
| End-to-end | Manual against real Postmark account during development (see `docs/manual-test.md`); CI does not require a live API key |
| Security | Explicit table tests against known injection payloads; assertions on outbound mail structure (mock-captured) |
| CI | GitHub Actions: `go vet ./...` and `go test -race -count=1 ./...` on push to main and on PRs |

## Epic and story breakdown

Seven epics, sized for implementation in sequence. Each story is intended to be completable in a single 1-2 hour session with passing tests at the end.

### Epic 1: Project restructure [S, Exec]

**Definition of done:** Repo renamed to `posthorn`. The standalone-core source tree at `core/` is buildable, existing transport code migrated. No new functionality yet.

> **2026-05-15 amendment:** Stories 1.2 and 1.3 originally split the codebase into a two-module workspace (`core/` + `caddy/`) joined by `go.work`. When the Caddy adapter was cut pre-tag, the `caddy/` module and `go.work` were removed; the repo is now a single Go module rooted at `core/`. Story 1.1 (rename) and the spirit of Stories 1.2–1.3 (move transport code into `core/`) still describe what happened.

- **Story 1.1:** Rename GitHub repo from `caddy-formward` to `posthorn`. Update CONTRIBUTING.md, CLAUDE.md, README.md to use new project identity. Verify `git clone` still works via auto-redirect for at least one external clone.
  - Acceptance: New URL resolves; old URL redirects; CI still passes.

- **Story 1.2:** Move existing `transport.go`, `transport_postmark.go`, and their tests into `core/transport/`.
  - Acceptance: `go build ./...` succeeds in `core/`; existing transport tests pass after import-path updates.

- **Story 1.3:** Strip Caddy-specific scaffolding from migrated code. Remove `caddy.Module` registration, `caddy.Provisioner` / `caddy.Validator` interface implementations, and Caddyfile-specific parsing from the core.
  - Acceptance: `core/` module's `go.mod` does not import `github.com/caddyserver/caddy/v2`; `core` builds standalone.

### Epic 2: Standalone gateway core [L, Exec]

**Definition of done:** A working standalone HTTP server that loads TOML config, accepts form submissions on configured paths, validates them, and sends via the Postmark transport. No spam protection or rate limiting yet (Epic 3); no retry policy yet (Epic 4).

- **Story 2.1:** Implement TOML config loader in `core/config/`. Schema mirrors the architecture doc's config sketch: top-level `[[endpoints]]` array of tables, each with `path`, `to`, `from`, `[endpoints.transport]`, etc. Support `${env.VAR}` placeholder resolution.
  - Acceptance: Tests cover successful parse, missing required fields (returns clear error), env-var resolution, invalid TOML.

- **Story 2.2:** Implement HTTP form handler in `core/gateway/`. Accepts POST submissions on configured paths, parses the body, hands off to the configured transport. Standalone struct implements `http.Handler`.
  - Acceptance: Tests cover successful POST, non-POST methods (405), wrong content-type (400), unknown path (404 falls through).

- **Story 2.3:** Implement validation in `core/validate/`. Required-fields and email-format checks, returning structured 422 responses.
  - Acceptance: Tests cover all-fields-present, missing-required-field, malformed email. JSON 422 schema matches FR10/FR11.

- **Story 2.4:** Implement Go template rendering in `core/template/`. Subject and body templates, with custom-fields passthrough block.
  - Acceptance: Tests cover successful rendering, parse error at config load time, missing variable (renders empty), passthrough block sorting.

- **Story 2.5:** Implement `cmd/posthorn` binary entry point. CLI subcommands `serve` and `validate`. Signal handling (SIGTERM/SIGINT) with graceful shutdown.
  - Acceptance: `posthorn validate --config valid.toml` exits 0; `posthorn validate --config invalid.toml` exits non-zero with clear error. `posthorn serve` starts an HTTP listener; SIGTERM drains and exits cleanly.

### Epic 3: Spam protection and rate limiting [M, Exec]

**Definition of done:** Honeypot, Origin/Referer, max-body-size, and token-bucket rate limit are all enforced; tests cover positive and negative cases including header-injection and proxy-aware IP extraction.

- **Story 3.1:** Implement honeypot, Origin/Referer fail-closed, and max-body-size checks in `core/spam/`. Apply in handler order: body size first, then honeypot (silent 200), then Origin/Referer.
  - Acceptance: Tests cover honeypot triggered/not-triggered, Origin allowed/denied/missing-both-headers (with and without `allowed_origins` configured per NFR4), body size enforced. Header-injection payload tests pass (NFR2).

- **Story 3.2:** Implement token-bucket rate limiter in `core/ratelimit/` with per-endpoint configuration and per-IP keying. Implement `trusted_proxies` config to read X-Forwarded-For from listed proxy IPs. Add LRU eviction at 10,000 tracked IPs (NFR6).
  - Acceptance: Tests cover token-bucket math (burst, refill, exceeded), client-IP extraction with and without trusted proxies, LRU eviction at the cap. Concurrent test verifies thread safety.

### Epic 4: Failure handling and structured logging [S, Exec]

**Definition of done:** Retry behavior matches FR19-22; terminal failures are logged with full payload (when configured); all event types use structured logging with submission IDs.

- **Story 4.1:** Implement send-with-retry logic in the handler. Encode FR19 (1 retry on transient/5xx with 1s backoff), FR20 (1 retry on 429 with 5s backoff), FR21 (no retry on 4xx-non-429), FR22 (10s hard timeout via `context.WithTimeout`).
  - Acceptance: Tests using mock transport cover each retry path; timeout test verifies request terminates at exactly 10s with terminal failure status.

- **Story 4.2:** Wire structured (JSON) logging throughout `core/log/`. Implement `log_failed_submissions` config with default true. On terminal failure, emit ERROR-level log with full submission payload (form fields, headers); on `false`, emit ERROR with metadata only. Generate UUIDv4 submission IDs and propagate through all logs for the request (NFR7, NFR8).
  - Acceptance: Tests assert presence of structured fields in log output, presence/absence of payload based on config, propagation of submission_id across log lines for one request. NFR3 test asserts API key never appears in any log output.

### Epic 5: Distribution [M, Exec + External]

> External: first GHCR multi-arch publish needs verification against a real `docker pull` on each architecture; that verification time isn't compressible by working faster.

**Definition of done:** Working Dockerfile, multi-arch image, GHCR publish via GitHub Actions, basic CI.

- **Story 5.1:** Add `core/Dockerfile` using multi-stage build (golang:1.25 builder → gcr.io/distroless/static runtime). Image entrypoint runs `posthorn serve --config /etc/posthorn/config.toml`. Single-arch local build first.
  - Acceptance: `docker build` succeeds; running container with sample config + Postmark API key sends test email successfully.

- **Story 5.2:** Add GitHub Actions workflow for CI: `go test -race -count=1 ./...` and `go vet ./...` on push to main and PRs.
  - Acceptance: CI passes on a clean main branch; PR with deliberately broken test fails.

- **Story 5.3:** Add release workflow: on tag push (`v*.*.*`), build multi-arch Docker image (amd64 + arm64) and push to GHCR. Tag `:latest` only on stable releases.
  - Acceptance: Tagging `v0.0.1-test` produces both arch images at `ghcr.io/craigmccaskill/posthorn:v0.0.1-test`. Pulling on each architecture works.

### Epic 6: Caddy adapter — **retired 2026-05-15**

Original definition of done was a Caddy v2 adapter module wrapping the core handler as `http.handlers.posthorn`. Stories 6.1–6.3 were implemented and shipped during development; on 2026-05-15, on tag eve, the adapter was cut for the product reasons recorded in the brief's status log. The `caddy/` directory, the workspace file, the parity test, and the manual parity procedure were removed. The `core/gateway.Handler` interface is preserved so a community module against it remains possible without the project carrying the maintenance.

### Epic 7: Documentation and release [M, Exec + Decision]

> Decision: operator chooses tag-day timing and release-notes voice.

**Definition of done:** Repository has a complete README, OSS-hygiene docs, working examples for both deployment shapes, tagged v1.0.0, modules-page submission filed.

- **Story 7.1:** Write README with: project description, "why" framing (cloud-blocks-SMTP motivation), copy-pasteable Docker Compose example, complete TOML config reference, DNS prerequisites (SPF/DKIM/DMARC), build instructions, badges, license note. Set GitHub repo metadata: description, topics (`email-gateway`, `postmark`, `self-hosted`).
  - Acceptance: README example builds and runs against a real Postmark account. NFR16-18 satisfied.

- **Story 7.2:** Add OSS hygiene files. `CONTRIBUTING.md` updated for Posthorn; `SECURITY.md` documenting the vulnerability disclosure process and explicit security guarantees (NFR1-3); `CODE_OF_CONDUCT.md` adopting Contributor Covenant 2.1; `CHANGELOG.md` in Keep a Changelog format with v1.0.0 entry; `.github/PULL_REQUEST_TEMPLATE.md` and `.github/ISSUE_TEMPLATE/` (bug + feature).
  - Acceptance: All listed files present. SECURITY.md gives a clear reporting channel.

- **Story 7.3:** Tag v1.0.0 release with release notes summarizing v1.0 scope. Verify Docker images publish to GHCR.
  - Acceptance: GitHub release published; Docker image pullable on both architectures.

### Epic 8: API mode (block B; originally v1.1, folded into v1.0) [L, Exec + External]

> External: real Postmark account validation for the API-mode + idempotency + to_override paths.

**Definition of done:** v1.1 API mode is shippable: operators can configure `auth = "api-key"` endpoints, server-to-server callers can hit them with `Authorization: Bearer <key>` against `application/json` bodies, idempotent retries via `Idempotency-Key` work as specified, and all v1.0 form-mode behavior is unchanged.

- **Story 8.1:** Extend the config schema with `auth` mode and `api_keys` list. Add `Auth string` (default `"form"`) and `APIKeys []string` to `EndpointConfig`. Implement parse-time validation: `auth = "api-key"` requires non-empty `api_keys`; API-mode endpoints reject `honeypot`, `allowed_origins`, `redirect_success`, `redirect_error` fields with a clear named-field error message. `${env.VAR}` substitution honored on `api_keys` values (FR31, FR32, FR33, FR45).
  - Acceptance: Tests cover all four config permutations (form-mode default, form-mode explicit, api-mode valid, api-mode invalid). Each rejection path produces a parse error naming the offending field. Existing v1.0 configs parse identically.

- **Story 8.2:** Implement API-key authentication middleware and API-mode rate limiting. Parse `Authorization: Bearer <key>` header; compare against the endpoint's `api_keys` list using `crypto/subtle.ConstantTimeCompare`. Failed auth (missing header, wrong scheme, no matching key) returns HTTP 401. On API-mode endpoints, the rate limiter keys on the matched API key value instead of client IP; existing `core/ratelimit/` extended with a new key-extraction path. Auth failure logs **must not** contain the key string (FR34, FR35, NFR19, NFR21).
  - Acceptance: Tests cover valid key, wrong scheme, missing header, key not in list. NFR21 test triggers auth failure with a known sentinel API key and asserts the sentinel does not appear in captured logs. Rate limit test confirms two callers with different keys against the same endpoint have independent buckets.

- **Story 8.3:** Implement JSON ingress on API-mode endpoints. Accept `application/json` bodies and parse into the same flat-keyed map used by form-mode submissions; non-JSON request bodies return HTTP 415. Reuse `core/validate/` for required-field and email-format checks. Reuse the FR13 custom-fields passthrough block (FR36, FR37, FR38, FR39).
  - Acceptance: Tests cover well-formed JSON submission (200), malformed JSON (400), form-encoded body on API-mode endpoint (415), missing required field in JSON (422), malformed email in JSON (422). Custom-fields block renders identically to form-mode test.

- **Story 8.4:** Implement idempotency cache in a new `core/idempotency/` package. In-memory LRU cache keyed on `(endpoint path, Idempotency-Key)`, default capacity 10K entries per endpoint, 24h TTL. Cache stores complete original response (status, body bytes, submission_id, transport_message_id). Replay returns byte-identical original response. Validate key shape (1–255 printable ASCII; malformed returns 400). In-flight tracker returns HTTP 409 Conflict when a duplicate key arrives before the original completes (FR40, FR41, FR42, FR43, FR44, NFR20).
  - Acceptance: Tests cover first request caches, replay returns byte-identical response, TTL eviction, LRU eviction at cap, malformed key (400), in-flight collision (409). Per-endpoint scope test: same key on two endpoints does not collide.

- **Story 8.5:** Documentation and manual-test extension. Site docs: new reference pages for API mode (auth, JSON body shape, idempotency semantics, 409 behavior). Update [docs/manual-test.md](docs/manual-test.md) with an API-mode procedure (curl with `Authorization: Bearer`, `Content-Type: application/json`, `Idempotency-Key` header). CHANGELOG entry for v1.1.0 under `[Unreleased]`.
  - Acceptance: All listed docs present and rendered. Manual-test procedure verified end-to-end against real Postmark. CHANGELOG entry references each new FR.

### Epic 9: Multi-transport (originally v1.2, folded into v1.0) [XL, Exec + External]

> External: each new transport needs a real provider account for the validation pass. Resend, Mailgun, AWS SES (IAM-credentialed), and an outbound-SMTP relay (Mailtrap, or a real Postfix/Mailgun-SMTP target). Operator-side time on these isn't compressible.
>
> Per-story sizing: 9.1 registry [S], 9.2 Resend [S], 9.3 Mailgun [S], 9.4 SES SigV4 [L] (the marginal case; ADR-14 tripwire applies), 9.5 outbound-SMTP [M], 9.6 docs [S].

**Definition of done:** Posthorn ships four transports beyond Postmark — Resend, Mailgun, AWS SES, outbound-SMTP. Each honors the existing `Transport` contract, passes header-injection and key-never-in-logs test suites, and is exercised by the manual-test procedure against a real upstream account.

- **Story 9.1:** Transport registry / generalized config. Refactor `config.TransportConfig.Validate` from a hardcoded switch on `type` to a registry pattern where each transport file registers its config validation. Existing Postmark transport adapts to the new registration shape; no behavior change.
  - Acceptance: All v1.0 transport tests pass unchanged. Adding a new transport in a single new file (validator + Send) is enough to integrate it.

- **Story 9.2:** Resend transport. New `core/transport/resend.go` implementing `Transport.Send` against `https://api.resend.com/emails`. Bespoke HTTP client (ADR-1). Map error responses per the existing `ErrorClass` taxonomy.
  - Acceptance: Tests for success (200 + 202), 429 → ErrRateLimited, 5xx → ErrTransient, 4xx → ErrTerminal, network/timeout → ErrTransient. Header-injection test suite mirrors `TestNoHeaderInjection_*` (FR52). NFR21 sentinel-key test (FR53). Manual-test procedure appended to [docs/manual-test.md](docs/manual-test.md) and verified against a real Resend account.

- **Story 9.3:** Mailgun transport. New `core/transport/mailgun.go`. Basic auth (`api:<key>`), multipart/form-data body, `https://api.mailgun.net/v3/<domain>/messages`.
  - Acceptance: Same test shape as 9.2. Multipart construction uses `mime/multipart.Writer` to avoid hand-crafted boundary string concat.

- **Story 9.4:** AWS SES transport. New `core/transport/awssigv4.go` (SigV4 signing implementation) + `core/transport/ses.go` (SESv2 `SendEmail` request shape). Required config: `access_key_id`, `secret_access_key`, `region`.
  - Acceptance: SigV4 round-trip test against AWS's published signing examples (the canonical pre-baked request/signature pairs in their docs). Same transport tests as 9.2. **Tripwire:** if total LOC (sigv4 + ses + tests) exceeds 500, stop and surface — ADR-14 trigger for SDK reconsideration.

- **Story 9.5:** Outbound-SMTP transport. New `core/transport/smtpout.go` using stdlib `net/smtp` (ADR-17). STARTTLS-required by default; SMTP AUTH PLAIN/LOGIN. Required config: `host`, `port`, `username`, `password`.
  - Acceptance: Test against a local `smtpd` (e.g., `mailpit` in CI; real Mailtrap or local relay for manual-test). Connection failure → ErrTransient. Auth failure → ErrTerminal. 421/450 (greylisting) → ErrTransient. STARTTLS downgrade attempts → ErrTerminal.

- **Story 9.6:** Per-transport reference pages on the docs site. New pages under `configuration/transports/`: `resend.mdx`, `mailgun.mdx`, `ses.mdx`, `smtp.mdx`. Each documents required + optional config, common gotchas, manual-test invocation. Update the TOML reference's transport table.
  - Acceptance: All four pages render in the sidebar. Each has a copy-pasteable config block.

### Epic 10: Operational features (originally v1.2, folded into v1.0) [M, Exec]

> Per-story sizing: 10.1 healthz+metrics [S], 10.2 dry-run [XS], 10.3 CSRF [S], 10.4 trusted-proxies presets [XS], 10.5 IP-strip [XS].

**Definition of done:** `/healthz`, `/metrics`, dry-run mode, CSRF tokens, `trusted_proxies` presets, and IP-stripping all ship behind opt-in config (no v1.0 backwards-compat breakage). Each has unit tests and a documented operator-facing surface.

- **Story 10.1:** `/healthz` and `/metrics` endpoints. New `core/metrics/` package; hand-rolled Prometheus exposition (ADR-15). Wire into `cmd/posthorn` as fixed-path handlers alongside the ServeMux endpoint loop.
  - Acceptance: `/healthz` returns 200 with body `ok`. `/metrics` returns Prometheus text-format output; `promtool check metrics` parses it without errors. Metrics include the FR55-listed counters and histograms. NFR24 test: a request with a malicious endpoint-named-`<script>` does not appear in metric labels.

- **Story 10.2:** Dry-run mode. New `dry_run` field on `EndpointConfig`; handler short-circuits before `transport.Send` and returns 200 with JSON `{"status":"dry_run","prepared_message":{...}}`. Idempotency cache treats dry-run replies the same as live (cacheable).
  - Acceptance: Tests cover dry-run for form mode, api mode, and api mode with `to_override`. `transport.Send` not called.

- **Story 10.3:** CSRF tokens. New `core/csrf/` package with HMAC-SHA256 sign/verify helpers and a `IssueToken(secret []byte, ttl time.Duration)` utility for operator-side use. Handler verifies `_csrf_token` form field when `csrf_secret` is configured. Form-mode only; api-mode endpoints reject `csrf_secret` at config parse (mirror FR32).
  - Acceptance: Round-trip test (issue → verify) passes. Expired token → 403. Tampered token → 403. Missing token when configured → 403. NFR3 test: `csrf_secret` value never appears in any captured log line.

- **Story 10.4:** `trusted_proxies` named presets. Embed a static map of preset → CIDR list (Cloudflare's published IP ranges as of release time; `aws-elb`, `gcp-lb`, `azure-front-door` similarly). Parse-time expansion in `ratelimit.ParsePrefixes`.
  - Acceptance: Tests verify each preset expands to a non-empty list of valid CIDRs. Operator can mix preset names and explicit CIDRs in one `trusted_proxies` list.

- **Story 10.5:** IP-stripping. New `strip_client_ip` field. Handler check at each log site that currently emits `client_ip`; emit only when `strip_client_ip == false`.
  - Acceptance: Tests verify `client_ip` field absent from `rate_limited` and `submission_failed` log lines when `strip_client_ip = true`. Rate-limit keying unchanged.

### Epic 11: SMTP ingress (originally v1.3, folded into v1.0) [L, Exec + External]

> External: validation needs a real internal SMTP client (`swaks`, `s-nail`, or a real app like Ghost on another VM) and inbox check.
>
> Per-story sizing: 11.1 spec [XS, mostly done in this PRD amendment], 11.2 Ingress interface refactor [S], 11.3 listener + SMTP state machine [M], 11.4 AUTH [S], 11.5 MIME ingest [M], 11.6 SMTP defenses [S], 11.7 binary integration [S], 11.8 docs + manual-test [S].

**Definition of done:** Posthorn accepts SMTP from an internal client (`swaks`, `s-nail`, real app like Ghost), parses the MIME, builds a `transport.Message`, sends through the configured outbound transport. Open-relay defenses verified by hostile-payload tests. Existing HTTP ingress unchanged.

- **Story 11.1:** Spec work — finalize the `Ingress` interface design, SMTP threat model FRs (already FR62-FR68 above; this story is about implementation-side specifics), and lock the new ADRs (12-17, mostly done in this amendment but story 11.1 is the "do we still agree?" checkpoint before code lands).
  - Acceptance: No code change. Spec docs reviewed and confirmed against the implementation plan.

- **Story 11.2:** Extract `Ingress` interface. Move HTTP form/api-mode handler logic to implement `core/ingress.Ingress` while leaving the `gateway` package as the implementing type. Existing test suite passes unchanged (behavior preservation is the acceptance).
  - Acceptance: `go test -race ./...` green. No behavioral diff in form-mode or api-mode pipeline.

- **Story 11.3:** TCP listener + SMTP protocol parser. New `core/smtp/` package implementing the inbound SMTP state machine (EHLO/HELO → STARTTLS → AUTH → MAIL FROM → RCPT TO → DATA → QUIT). Hand-rolled per ADR-17 reasoning (minimal SMTP server is ~400 LOC; servers like `go-smtp` are 2K+).
  - Acceptance: Tests cover the happy path (full SMTP transaction), each rejection point (530 unauth, 550 sender, 550 recipient, 552 too big, 421 disconnect), and STARTTLS upgrade.

- **Story 11.4:** SMTP AUTH (PLAIN, LOGIN) + client-cert auth alternative. Each per FR63.
  - Acceptance: Tests for valid/invalid PLAIN, valid/invalid LOGIN, valid/invalid client cert (against a configured CA), and the `auth_required = "either"` case.

- **Story 11.5:** Inbound MIME → `transport.Message` conversion. Use stdlib `net/mail` + `mime/multipart`. Extract From, To, Subject, plain-text body. Multipart messages: prefer `text/plain` part; reject HTML-only for v1.0 (HTML body is v2 scope).
  - Acceptance: Tests cover plain MIME, multipart-alternative (text + html), unicode in subject (`=?utf-8?...?=`), multibyte body. NFR22 header-injection test: SMTP DATA with `Bcc: victim@target.com` in headers does not result in outbound mail to victim.

- **Story 11.6:** SMTP-specific defenses. Sender allowlist, recipient allowlist OR count cap, message size cap, STARTTLS enforcement. All per FR64-67.
  - Acceptance: Each rejection has an explicit test with the expected SMTP response code.

- **Story 11.7:** Binary integration. `cmd/posthorn serve` starts both HTTP and SMTP listeners when both are configured. Each runs in its own goroutine; SIGTERM drains both. Graceful shutdown timeout is the larger of the two ingresses' configured timeouts.
  - Acceptance: Integration test starts a `posthorn` process, fires HTTP and SMTP traffic, sends SIGTERM, observes clean shutdown for both ingresses.

- **Story 11.8:** Documentation and manual-test. New `features/smtp-ingress.mdx` on the docs site. New recipe `recipes/ghost-smtp.mdx` (Ghost admin login via Posthorn). Manual-test procedure extended with `swaks` invocation against the SMTP listener.
  - Acceptance: All listed docs present and rendered. Manual-test procedure verified end-to-end against a real SMTP client → real Postmark account.

### Epic 12: Final rescope + tag [M, Exec + External + Decision]

> External: final canonical manual-test pass against real providers for all paths. Decision: tag-day timing and the public-release moment.
>
> Per-story sizing: 12.1 doc rescope sweep [S], 12.2 README rewrite [S], 12.3 operator validation [S, External], 12.4 tag [XS, Decision].

**Definition of done:** All v1.0/v1.1/v1.2/v1.3 amendment labels in the spec collapse to "v1.0." README rewritten to reflect full scope. CHANGELOG single [1.0.0] block. Tag v1.0.0 published; Docker images on GHCR.

- **Story 12.1:** Spec rescope sweep. Brief, PRD, architecture doc all relabel version-numbered amendment sections to "v1.0 scope." Status logs preserved as historical record.
  - Acceptance: No "v1.1 amendment" / "v1.2 amendment" / "v1.3 amendment" headings remain in any spec doc.

- **Story 12.2:** README rewrite. Cover the full v1.0 surface — form mode + API mode + all four transports + SMTP ingress. Drop "currently form-only" framing.
  - Acceptance: README accurately describes everything in the binary.

- **Story 12.3:** Operator validation pass — final canonical manual-test procedure across all ingresses and transports.
  - Acceptance: Every section of `docs/manual-test.md` runs green.

- **Story 12.4:** Tag v1.0.0 + GHCR release.
  - Acceptance: GitHub release published with release notes summarizing the full v1.0 scope. Docker images pullable on amd64 and arm64.

## Out of scope (re-stated for clarity)

Defer to [the project brief](./01-project-brief.md) §"MVP Scope > Out of scope" for the full list. After the 2026-05-16 rescope folded v1.1 / v1.2 / v1.3 into v1.0, the deferred line is now between v1.0 (everything currently spec'd) and v2 (the stateful-platform boundary). Key v1.0 exclusions:

- **Persistent submission storage / retry queue (durable across restarts)** — v2. v1.0 retry is in-request only; the in-memory idempotency cache (FR42) is wiped on restart.
- **File attachments** — v2.
- **Webhook transport (outbound + lifecycle event forwarding)** — v2.
- **Suppression list, automatic unsubscribe injection (RFC 8058)** — v2.
- **Durable idempotency** — v2 (replaces v1.0's in-memory cache).
- **Multi-tenant / per-tenant config isolation, multiple outputs per endpoint (fan-out), per-RCPT routing on SMTP listener** — v2.
- **Batch send API** — deferred without a target version; see brief's "Deliberately not on the roadmap" section.
- **HTML body support** — v2.
- **Admin UI, proof-of-work spam challenge, PGP encryption** — v3.
- **Inbound mail parsing (MX target, IMAP polling)** — deliberately deferred to v3+; different threat model entirely.

If v1.0 implementation surfaces time savings, additional v1.0 polish (better error messages, more validator coverage, more documentation) is the right place to invest, not v2 features.

## Open questions (implementation-level)

These do not block the brief but need answers during implementation. None should change v1.0 scope.

1. **Logging library: `log/slog` (stdlib) or `zap`?** `slog` is stdlib, no dep, structured logging native. Recommendation: `slog`. Decided during Story 4.2.

2. **Body template — file path vs inline detection.** Recommendation: heuristic — if the value contains `{{` it's inline; otherwise it's a file path. Reject ambiguity at validation time. Decided during Story 2.4.

3. **Reply-To handling.** When the form has an email field, set the email's `Reply-To` to that address by default? Recommendation: yes, configurable via `reply_to_email_field <fieldname>` (default: the email field). Decided during Story 2.4.

## Traceability

Every FR and NFR maps back to a brief commitment. Quick reference:

| Source in [project brief](./01-project-brief.md) | Maps to |
|---|---|
| MVP > Ingress | FR1, FR2 |
| MVP > Egress | FR3, FR4 |
| MVP > Spam protection | FR5, FR6, FR7, FR8, FR9, NFR4, NFR6 |
| MVP > Validation | FR10, FR11 |
| MVP > Email features | FR12, FR13 |
| MVP > Response handling | FR14, FR15, FR16 |
| MVP > Operational | FR17, FR18, NFR7, NFR8 |
| MVP > Failure handling | FR19, FR20, FR21, FR22, FR23, NFR5 |
| MVP > Deployment shape | FR24, FR25, FR26, NFR12, NFR13 (FR27–FR30, NFR10, NFR14 deleted 2026-05-15 with the Caddy adapter cut) |
| MVP > Security NFR | NFR1, NFR2, NFR3 |
| Constraints | NFR9, NFR11, NFR15 |
| Done criteria | NFR16, NFR17, NFR18, Epic 7 |
| R4 mitigation | NFR2 |
| R5 mitigation | NFR17 |
| Post-MVP > v1.0 block B (API mode) | FR31–FR46, NFR19–NFR21, Epic 8 |
| Post-MVP > v1.0 block C (multi-transport) | FR47–FR53, Epic 9 |
| Post-MVP > v1.0 block C (operational features) | FR54–FR59, NFR24, Epic 10 |
| Post-MVP > v1.0 block D (SMTP ingress) | FR60–FR68, NFR22, NFR23, Epic 11 |
