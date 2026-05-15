---
title: "PRD: Posthorn v1.0"
status: locked
created: 2026-04-27
synced_from_obsidian: 2026-04-27
---

# PRD: Posthorn v1.0

This document translates the locked decisions in [the project brief](./01-project-brief.md) into formal functional requirements (FRs), non-functional requirements (NFRs), and an epic-and-story breakdown sized for implementation. Every FR and NFR derives from a commitment in the brief; nothing here introduces new scope.

Scope is strictly v1.0. v1.1+ items are tracked in the project's Obsidian dashboard (not included in this repository).

> **2026-05-15 amendment:** The Caddy v2 adapter module was cut from v1.0 pre-tag (see the brief's status log for the product reasoning). Original FR27–FR30 and NFR10 are deleted in-place below; Epic 6 (Caddy adapter) is retired; Stories 1.2 and 1.3 (workspace restructure) and Story 7.3 (modules-page PR) are noted with the cut. The standalone-behind-any-reverse-proxy deployment shape is now the only one in scope.

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

### Epic 1: Project restructure (~3h)

**Definition of done:** Repo renamed to `posthorn`. The standalone-core source tree at `core/` is buildable, existing transport code migrated. No new functionality yet.

> **2026-05-15 amendment:** Stories 1.2 and 1.3 originally split the codebase into a two-module workspace (`core/` + `caddy/`) joined by `go.work`. When the Caddy adapter was cut pre-tag, the `caddy/` module and `go.work` were removed; the repo is now a single Go module rooted at `core/`. Story 1.1 (rename) and the spirit of Stories 1.2–1.3 (move transport code into `core/`) still describe what happened.

- **Story 1.1:** Rename GitHub repo from `caddy-formward` to `posthorn`. Update CONTRIBUTING.md, CLAUDE.md, README.md to use new project identity. Verify `git clone` still works via auto-redirect for at least one external clone.
  - Acceptance: New URL resolves; old URL redirects; CI still passes.

- **Story 1.2:** Move existing `transport.go`, `transport_postmark.go`, and their tests into `core/transport/`.
  - Acceptance: `go build ./...` succeeds in `core/`; existing transport tests pass after import-path updates.

- **Story 1.3:** Strip Caddy-specific scaffolding from migrated code. Remove `caddy.Module` registration, `caddy.Provisioner` / `caddy.Validator` interface implementations, and Caddyfile-specific parsing from the core.
  - Acceptance: `core/` module's `go.mod` does not import `github.com/caddyserver/caddy/v2`; `core` builds standalone.

### Epic 2: Standalone gateway core (~5h)

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

### Epic 3: Spam protection and rate limiting (~3h)

**Definition of done:** Honeypot, Origin/Referer, max-body-size, and token-bucket rate limit are all enforced; tests cover positive and negative cases including header-injection and proxy-aware IP extraction.

- **Story 3.1:** Implement honeypot, Origin/Referer fail-closed, and max-body-size checks in `core/spam/`. Apply in handler order: body size first, then honeypot (silent 200), then Origin/Referer.
  - Acceptance: Tests cover honeypot triggered/not-triggered, Origin allowed/denied/missing-both-headers (with and without `allowed_origins` configured per NFR4), body size enforced. Header-injection payload tests pass (NFR2).

- **Story 3.2:** Implement token-bucket rate limiter in `core/ratelimit/` with per-endpoint configuration and per-IP keying. Implement `trusted_proxies` config to read X-Forwarded-For from listed proxy IPs. Add LRU eviction at 10,000 tracked IPs (NFR6).
  - Acceptance: Tests cover token-bucket math (burst, refill, exceeded), client-IP extraction with and without trusted proxies, LRU eviction at the cap. Concurrent test verifies thread safety.

### Epic 4: Failure handling and structured logging (~2.5h)

**Definition of done:** Retry behavior matches FR19-22; terminal failures are logged with full payload (when configured); all event types use structured logging with submission IDs.

- **Story 4.1:** Implement send-with-retry logic in the handler. Encode FR19 (1 retry on transient/5xx with 1s backoff), FR20 (1 retry on 429 with 5s backoff), FR21 (no retry on 4xx-non-429), FR22 (10s hard timeout via `context.WithTimeout`).
  - Acceptance: Tests using mock transport cover each retry path; timeout test verifies request terminates at exactly 10s with terminal failure status.

- **Story 4.2:** Wire structured (JSON) logging throughout `core/log/`. Implement `log_failed_submissions` config with default true. On terminal failure, emit ERROR-level log with full submission payload (form fields, headers); on `false`, emit ERROR with metadata only. Generate UUIDv4 submission IDs and propagate through all logs for the request (NFR7, NFR8).
  - Acceptance: Tests assert presence of structured fields in log output, presence/absence of payload based on config, propagation of submission_id across log lines for one request. NFR3 test asserts API key never appears in any log output.

### Epic 5: Distribution (~2.5h)

**Definition of done:** Working Dockerfile, multi-arch image, GHCR publish via GitHub Actions, basic CI.

- **Story 5.1:** Add `core/Dockerfile` using multi-stage build (golang:1.25 builder → gcr.io/distroless/static runtime). Image entrypoint runs `posthorn serve --config /etc/posthorn/config.toml`. Single-arch local build first.
  - Acceptance: `docker build` succeeds; running container with sample config + Postmark API key sends test email successfully.

- **Story 5.2:** Add GitHub Actions workflow for CI: `go test -race -count=1 ./...` and `go vet ./...` on push to main and PRs.
  - Acceptance: CI passes on a clean main branch; PR with deliberately broken test fails.

- **Story 5.3:** Add release workflow: on tag push (`v*.*.*`), build multi-arch Docker image (amd64 + arm64) and push to GHCR. Tag `:latest` only on stable releases.
  - Acceptance: Tagging `v0.0.1-test` produces both arch images at `ghcr.io/craigmccaskill/posthorn:v0.0.1-test`. Pulling on each architecture works.

### Epic 6: Caddy adapter — **retired 2026-05-15**

Original definition of done was a Caddy v2 adapter module wrapping the core handler as `http.handlers.posthorn`. Stories 6.1–6.3 were implemented and shipped during development; on 2026-05-15, on tag eve, the adapter was cut for the product reasons recorded in the brief's status log. The `caddy/` directory, the workspace file, the parity test, and the manual parity procedure were removed. The `core/gateway.Handler` interface is preserved so a community module against it remains possible without the project carrying the maintenance.

### Epic 7: Documentation and release (~3h)

**Definition of done:** Repository has a complete README, OSS-hygiene docs, working examples for both deployment shapes, tagged v1.0.0, modules-page submission filed.

- **Story 7.1:** Write README with: project description, "why" framing (cloud-blocks-SMTP motivation), copy-pasteable Docker Compose example, complete TOML config reference, DNS prerequisites (SPF/DKIM/DMARC), build instructions, badges, license note. Set GitHub repo metadata: description, topics (`email-gateway`, `postmark`, `self-hosted`).
  - Acceptance: README example builds and runs against a real Postmark account. NFR16-18 satisfied.

- **Story 7.2:** Add OSS hygiene files. `CONTRIBUTING.md` updated for Posthorn; `SECURITY.md` documenting the vulnerability disclosure process and explicit security guarantees (NFR1-3); `CODE_OF_CONDUCT.md` adopting Contributor Covenant 2.1; `CHANGELOG.md` in Keep a Changelog format with v1.0.0 entry; `.github/PULL_REQUEST_TEMPLATE.md` and `.github/ISSUE_TEMPLATE/` (bug + feature).
  - Acceptance: All listed files present. SECURITY.md gives a clear reporting channel.

- **Story 7.3:** Tag v1.0.0 release with release notes summarizing v1.0 scope. Verify Docker images publish to GHCR.
  - Acceptance: GitHub release published; Docker image pullable on both architectures.

## Out of scope (re-stated for clarity)

Defer to [the project brief](./01-project-brief.md) §"MVP Scope > Out of scope" for the full list. Key v1.0 exclusions worth re-stating in PRD context because they're tempting to slip in:

- **SMTP ingress** — v1.3. Implementation **must not** start during v1.0 even if time remains.
- **API-mode endpoints (API-key auth, JSON, batch, idempotency)** — v1.1.
- **Resend, Mailgun, SES, outbound-SMTP transports** — v1.2.
- **CSRF / time-based tokens** — v1.2.
- **Persistent submission storage / retry queue** — v2. v1.0 retry is in-request only.
- **File attachments** — v2.
- **Webhook transport (outbound + lifecycle event forwarding)** — v2.
- **Suppression list, automatic unsubscribe injection** — v2.
- **Health check endpoint, dry run mode, Prometheus metrics** — v1.2.
- **Multi-tenant / per-tenant config isolation** — post-v1.

If implementation goes faster than budgeted, additional v1.0 polish (better error messages, more validator coverage, more documentation) is the right place to invest, not v1.1 features.

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
