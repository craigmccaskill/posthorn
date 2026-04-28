---
title: "PRD: caddy-formward v1.0"
status: locked
created: 2026-04-27
synced_from_obsidian: 2026-04-27
---

# PRD: caddy-formward v1.0

This document translates the locked decisions in [the project brief](./01-project-brief.md) into formal functional requirements (FRs), non-functional requirements (NFRs), and an epic-and-story breakdown sized for implementation. Every FR and NFR derives from a commitment in the brief; nothing here introduces new scope.

Scope is strictly v1.0. v1.1+ items are tracked in the project's Obsidian dashboard (not included in this repository).

## Goals

Defer to [the project brief](./01-project-brief.md) §"Goals and Success Metrics". Two reminders that shape decisions in this document:

- **Done:** Ghost blog runs 30 days with zero dropped submissions; README has copy-pasteable Caddyfile examples verified on clean Caddy build; tagged v1.0.0 with Docker image
- **Worked:** caddyserver.com modules-page listing within 60 days

## Functional requirements

Each FR is atomic, testable, and traceable to a section of the brief. FRs are not in priority order — that comes via the epic structure below.

### Transport

**FR1.** The module **must** ship a Postmark HTTP API transport that accepts a configurable API key (via `{env.VAR}` substitution) and sends form submissions as email through `https://api.postmarkapp.com/email`.

**FR2.** The module **must** expose a pluggable transport interface such that additional transports (SMTP, Resend, etc.) can be added in v1.1+ without modifying core handler logic or breaking Caddyfile syntax.

### Spam protection

**FR3.** The module **must** support a configurable honeypot field. When the configured field name has a non-empty value on submission, the module silently rejects the request with HTTP 200 (so bots cannot distinguish honeypot rejection from success).

**FR4.** The module **must** validate the `Origin` and `Referer` headers against a configurable `allowed_origins` list. If `allowed_origins` is configured and *both* headers are missing, the module **must** reject the request (fail-closed). If `allowed_origins` is not configured, the module **must** allow any origin (fail-open by absence of config).

**FR5.** The module **must** enforce a configurable maximum request body size, applied via `http.MaxBytesReader`. Submissions exceeding the limit **must** receive HTTP 413.

**FR6.** The module **must** implement a token-bucket rate limiter per endpoint, configurable as `rate_limit <count> <interval>` (e.g., `rate_limit 5 1m`). Requests exceeding the limit **must** receive HTTP 429.

**FR7.** The rate limiter **must** key on the client IP. The module **must** support a `trusted_proxies` configuration that, when set, causes the limiter to read the client IP from `X-Forwarded-For` (rightmost untrusted IP) instead of the connection's RemoteAddr.

### Validation

**FR8.** The module **must** support a configurable list of required fields. Submissions missing or empty for any required field **must** receive HTTP 422 with a JSON response body identifying the specific field(s) that failed validation.

**FR9.** The module **must** validate the format of the submitter's email field (the field designated as the email address) using a basic syntactic check. Submissions with malformed email addresses **must** receive HTTP 422.

### Email rendering

**FR10.** The module **must** render the email subject and body using Go's `text/template`, with form fields available as template variables. Operators **must** be able to specify the body template via a file path or inline string.

**FR11.** Form fields not explicitly named in the configuration (i.e., not in the required list, not the email field, not the honeypot) **must** be appended to the rendered email body in a structured "Additional fields" block. The module **must not** require operators to pre-declare every form field.

### Response handling

**FR12.** The module **must** return JSON responses with these HTTP status codes:
  - `200 OK` on successful send
  - `400 Bad Request` on malformed request (e.g., not form-encoded)
  - `413 Payload Too Large` on body-size exceeded
  - `422 Unprocessable Entity` on validation failure
  - `429 Too Many Requests` on rate limit exceeded
  - `502 Bad Gateway` on terminal transport failure

**FR13.** The module **must** detect the request's `Accept` header. If `application/json` is preferred, return JSON. If `text/html` is preferred (default browser form submit), return a 303 See Other redirect to the configured `redirect_success` / `redirect_error` URL.

**FR14.** The module **must** support `redirect_success` and `redirect_error` URL configuration. If unconfigured, content negotiation **must** fall back to JSON regardless of `Accept` header.

### Operational

**FR15.** The module **must** emit Caddy-structured (zap) log entries for: submission received, submission accepted, submission sent, send retried, send failed (terminal), spam blocked (with reason), rate limit exceeded, validation failed (with field). Each log entry **must** include the endpoint path and a unique submission ID.

**FR16.** The module **must** support a `log_failed_submissions` boolean configuration (default `true`). When `true`, terminal send failures log the full submission payload (form fields and headers) at ERROR level so the operator can recover the data from logs. When `false`, only metadata is logged.

**FR17.** The module **must** support multiple independent endpoint configurations. Each `formward` directive in the Caddyfile **must** have its own recipients, transport, rate limit, templates, and spam-protection settings without interaction between endpoints.

### Failure handling

**FR18.** On a transient transport error (network failure, transport API HTTP 5xx), the module **must** retry the send exactly once after a 1-second delay.

**FR19.** On a transport `429 Too Many Requests` response, the module **must** retry the send exactly once after a 5-second delay.

**FR20.** On a transport `4xx` response other than `429` (e.g., `401 Unauthorized` for a bad API key, `422` for an invalid recipient), the module **must not** retry. The module **must** treat this as a terminal failure.

**FR21.** The module **must** enforce a hard 10-second timeout on the entire request, including any retry attempts. If the timeout is reached, any in-flight retry **must** be cancelled and the request **must** terminate with a terminal failure.

**FR22.** On terminal failure, the module **must** log the failure (and full payload if `log_failed_submissions=true`) at ERROR level and return HTTP 502 to the client.

## Non-functional requirements

### Security

**NFR1.** Submitter-controlled fields **must never** be interpolated as raw strings into email headers. All header construction **must** pass through transport library APIs that handle headers as structured data. Specifically: the Postmark transport **must** use Postmark's JSON API field for `From`, `To`, `Reply-To`, and `Subject`, never constructing headers via string concatenation.

**NFR2.** The test suite **must** include explicit cases verifying that header injection payloads (CRLF in name/email/subject fields, `\r\nBcc:` injection attempts, header smuggling sequences) do not produce unintended headers in outbound mail.

**NFR3.** API keys configured via `{env.VAR}` placeholders **must not** appear in any log output, including error logs. Tests **must** verify this by triggering transport failures and asserting the API key string does not appear in captured log output.

**NFR4.** The Caddyfile parser **must** reject configurations where `allowed_origins` is unset *and* the operator has explicitly opted out (no fail-open default for an explicitly empty allowlist — only for the absent-config case). This prevents a misconfiguration where the operator thinks they're restricting origins but typed the directive wrong.

### Performance

**NFR5.** Total request latency **must** be bounded by the 10-second hard timeout (FR21). The module **must not** introduce unbounded waits.

**NFR6.** The module **must not** maintain unbounded in-memory state. The rate limiter's per-IP token buckets **must** have a configurable maximum number of tracked IPs, with an LRU eviction policy when full. Default: 10,000 tracked IPs.

### Observability

**NFR7.** All log events **must** include structured fields: `submission_id`, `endpoint`, `transport`, `latency_ms`, `error_class` (where applicable). No free-text-only log lines for production events.

**NFR8.** Submission IDs **must** be UUIDs (v4) generated on receipt and propagated through every log line for that request.

### Compatibility

**NFR9.** The module **must** build and run on Go 1.25+ and Caddy 2.9+. The `go.mod` file **must** declare these as minimum versions. Note: the Go floor was raised from 1.23 to 1.25 during Story 1.1 implementation because Caddy v2.11.2's transitive dependencies require it. Future Caddy minor releases may push this floor higher.

**NFR10.** Caddyfile syntax **must** remain stable within the v1 major version. Adding optional new directives is permitted; removing or renaming existing ones is not.

### Distribution & build

**NFR11.** The module **must** be installable via `xcaddy build --with github.com/craigmccaskill/caddy-formward`.

**NFR12.** The repository **must** publish a Dockerfile that produces a Caddy image with the module pre-built.

**NFR13.** The module **must** be licensed Apache-2.0, with a `LICENSE` file in the repository root.

### Documentation

**NFR14.** The README **must** include a complete, copy-pasteable Caddyfile example for the Postmark transport, with all v1.0 directives demonstrated.

**NFR15.** The README **must** document DNS prerequisites for production use: SPF, DKIM, and DMARC records for the sending domain.

**NFR16.** Every Caddyfile directive **must** be documented in the README with at least one example value and a description of its behavior.

## Testing strategy

The brief commits to test coverage for header injection (NFR2) and to a Ghost blog production trial. Beyond those, the v1 testing strategy is:

| Layer | Approach |
|-------|----------|
| Unit | Table-driven Go tests for each component (validation, rate limiter, templating, content negotiation, error classification) |
| Transport integration | `httptest.NewServer` mock standing in for Postmark API; covers retry behavior, error classification, timeout enforcement |
| End-to-end | Manual against real Postmark account during development; CI does not require a live API key |
| Security | Explicit table tests against known injection payloads; assertions on outbound mail structure (mock-captured) |
| CI | GitHub Actions: `go test ./...` on push to main and on PRs |

## Epic and story breakdown

Seven epics, sized for implementation in sequence. Each story is intended to be completable in a single 1-2 hour session with passing tests at the end.

### Epic 1: Module scaffolding (~2h)

**Definition of done:** A working Caddy module that registers, parses a minimal Caddyfile directive, and responds to HTTP requests with a hardcoded 200 OK. No business logic yet.

- **Story 1.1:** Initialize Go module at `github.com/craigmccaskill/caddy-formward`. Create `go.mod` with Go 1.23 and Caddy 2.9 dependencies. Implement the minimal `caddy.Module` interface (`CaddyModule()` returning `caddy.ModuleInfo` with ID `http.handlers.formward`).
  - Acceptance: `go build` succeeds; `xcaddy build --with .` produces a binary that lists `http.handlers.formward` in `caddy list-modules`.

- **Story 1.2:** Implement the Caddyfile unmarshaler for the top-level `formward` directive with a path matcher and an empty sub-block. Parse and store the path. Implement `caddyhttp.MiddlewareHandler` returning 200 OK with body "OK" for matching requests.
  - Acceptance: A Caddyfile with `formward /test` routes `POST /test` to the handler; other paths fall through.

- **Story 1.3:** Add JSON config support via the standard Caddy module pattern (struct fields with `json:` tags). Verify the same config can be expressed in either Caddyfile or JSON form and produces identical behavior.
  - Acceptance: Config round-trips Caddyfile → JSON via `caddy adapt`; both forms produce the same module behavior.

### Epic 2: Postmark transport (~3h)

**Definition of done:** The module can receive a form submission and successfully send it as email through Postmark, configured via Caddyfile.

- **Story 2.1:** Define the `Transport` interface in `transport.go`: a `Send(ctx context.Context, msg Message) error` method, with a `Message` struct carrying recipient, sender, subject, body, and reply-to. Define error types: `TransportError` with classification (transient, terminal, rate-limited).
  - Acceptance: Interface compiles; documented with godoc; sample mock implementation in tests.

- **Story 2.2:** Implement `transport_postmark.go`. Use Postmark's JSON HTTP API. Construct request via `encoding/json` (no string-concat for headers — NFR1). Parse response, classify errors per FR18-20.
  - Acceptance: Unit tests with `httptest` mock cover success, 5xx retry, 429 retry, 4xx terminal, network timeout. NFR1 test asserts CRLF in fields does not produce extra headers.

- **Story 2.3:** Add Caddyfile parsing for `transport postmark { api_key {env.POSTMARK_API_KEY} }` sub-block. Wire the parsed config into the Postmark transport instantiation during module provisioning.
  - Acceptance: Caddyfile with the directive provisions a working transport; missing API key returns a clear validation error during `caddy validate`.

- **Story 2.4:** Manual end-to-end test: configure against a real Postmark account, POST a form submission, verify email lands. Document the test procedure in `docs/manual-test.md`.
  - Acceptance: Test email received in author's inbox via Postmark; test procedure reproducible by another developer with their own Postmark account.

### Epic 3: Spam protection and rate limiting (~3h)

**Definition of done:** Honeypot, Origin/Referer, max-body-size, and token-bucket rate limit are all enforced; tests cover positive and negative cases including header-injection and proxy-aware IP extraction.

- **Story 3.1:** Implement honeypot, Origin/Referer fail-closed, and max-body-size checks in `spam.go`. Wire Caddyfile directives `honeypot <name>`, `allowed_origins <url>...`, `max_body_size <size>`. Apply in handler order: body size first (rejects oversized before parsing), then honeypot (silent 200), then Origin/Referer.
  - Acceptance: Tests cover honeypot triggered/not-triggered, Origin allowed/denied/missing-both-headers (with and without `allowed_origins` configured per NFR4), body size enforced. Header-injection payload tests pass (NFR2).

- **Story 3.2:** Implement token-bucket rate limiter in `ratelimit.go` with per-endpoint configuration and per-IP keying. Implement `trusted_proxies` config to read X-Forwarded-For from listed proxy IPs. Add LRU eviction at 10,000 tracked IPs (NFR6).
  - Acceptance: Tests cover token-bucket math (burst, refill, exceeded), client-IP extraction with and without trusted proxies, LRU eviction at the cap. Concurrent test verifies thread safety.

### Epic 4: Validation and templating (~2.5h)

**Definition of done:** Required-fields and email-format validation produce structured 422 responses; subject/body templates render with form data; unrecognized fields appear in the structured passthrough block.

- **Story 4.1:** Implement required-fields validation and email-format validation in `validate.go`. Wire `required <field>...` and identify the email field via convention (the field named `email`) or explicit config (`email_field <name>` if non-default).
  - Acceptance: Tests cover all-fields-present, missing-required-field (single, multiple), empty-required-field, malformed email. JSON 422 responses match the documented schema.

- **Story 4.2:** Implement Go template rendering for subject and body in `template.go`. Support template specified via inline string OR file path (`subject "Contact from {{.name}}"` or `body templates/contact.txt`).
  - Acceptance: Tests cover successful rendering, template parse error (returns config validation error at provision time, not runtime), missing template variable (renders as empty string per Go template defaults).

- **Story 4.3:** Implement custom-fields passthrough in the templating step. Identify "named" fields (required + email + honeypot + any field referenced in templates) vs "unrecognized" fields, and render the latter as a sorted "Additional fields:" block at the end of the body.
  - Acceptance: Tests cover all-fields-named (no passthrough block), some-unnamed (passthrough block present, sorted), only-unnamed (passthrough block is the entire body content).

### Epic 5: Response handling (~1.5h)

**Definition of done:** All response status codes are returned correctly; content negotiation between JSON and redirect works; redirect URLs are honored.

- **Story 5.1:** Implement the JSON response builder in `response.go` with structured response types for each status code per FR12. Document the response schema in the README.
  - Acceptance: Tests assert response body and status for each error class (validation, rate limit, transport failure, success).

- **Story 5.2:** Implement content negotiation: parse `Accept` header, prefer JSON if `application/json` is acceptable, otherwise prefer redirect if redirects are configured. Fall back to JSON if no redirect URL is set, regardless of Accept header.
  - Acceptance: Tests cover JSON-preferred, HTML-preferred-with-redirects-set, HTML-preferred-without-redirects, malformed Accept header (defaults to JSON).

### Epic 6: Failure handling and logging (~2h)

**Definition of done:** Retry behavior matches FR18-21; terminal failures are logged with full payload (when configured); all event types use Caddy structured logging.

- **Story 6.1:** Implement the send-with-retry logic in the handler. Encode FR18 (1 retry on transient/5xx with 1s backoff), FR19 (1 retry on 429 with 5s backoff), FR20 (no retry on 4xx-non-429), FR21 (10s hard timeout via `context.WithTimeout`).
  - Acceptance: Tests using mock transport cover each retry path; timeout test verifies request terminates at exactly 10s with terminal failure status.

- **Story 6.2:** Wire Caddy structured (zap) logging throughout. Implement `log_failed_submissions` config with default true. On terminal failure, emit ERROR-level log with full submission payload (form fields, headers); on `false`, emit ERROR with metadata only. Generate UUIDv4 submission IDs and propagate through all logs for the request (NFR7, NFR8).
  - Acceptance: Tests assert presence of structured fields in log output, presence/absence of payload based on config, propagation of submission_id across log lines for one request. NFR3 test asserts API key never appears in any log output.

### Epic 7: Documentation and release (~1.5–2h)

**Definition of done:** Repository has a complete README, OSS-hygiene docs (CONTRIBUTING, SECURITY, code of conduct, changelog, GitHub templates), working Dockerfile, basic CI, and a tagged v1.0.0 release with the modules-page submission filed.

- **Story 7.1:** Write README with: project description, "why" framing (DigitalOcean port-587 motivation), copy-pasteable Caddyfile example, complete directive reference, DNS prerequisites (SPF/DKIM/DMARC), build instructions (xcaddy + Docker), badges (CI status, license, latest release), license note. Set GitHub repo metadata: description, topics (`caddy`, `caddy-module`, `contact-form`, `postmark`). NFR14, NFR15, NFR16 satisfied.
  - Acceptance: README example builds and runs against a real Postmark account on a clean Caddy install. Reviewed for completeness against NFR14-16 checklist. GitHub repo description and topics set.

- **Story 7.1a:** Add OSS hygiene files. (`CONTRIBUTING.md` already exists from initial setup — verify it's still accurate at release time.) Add `SECURITY.md` documenting the vulnerability disclosure process and the explicit security guarantees (NFR1 header injection prevention, NFR3 API key handling, NFR4 fail-closed origin checks). Add `CODE_OF_CONDUCT.md` adopting Contributor Covenant 2.1. Add `CHANGELOG.md` in [Keep a Changelog](https://keepachangelog.com) format with the v1.0.0 entry seeded from prior story commits. Add `.github/PULL_REQUEST_TEMPLATE.md` and `.github/ISSUE_TEMPLATE/` (bug report + feature request).
  - Acceptance: All listed files present and reviewed against standard OSS templates. SECURITY.md gives a clear reporting channel and references the relevant NFRs. CHANGELOG.md is current as of the v1.0.0 tag.

- **Story 7.2:** Add `Dockerfile` using `xcaddy` builder pattern. Verify image builds, runs Caddy with the module loaded, and accepts form submissions.
  - Acceptance: `docker build` succeeds; running container with sample Caddyfile + Postmark API key sends test email successfully.

- **Story 7.3:** Add basic GitHub Actions CI: `go test ./...` and `go vet` on push and PR. No matrix builds; just a single Linux + Go 1.25 job.
  - Acceptance: CI passes on a clean main branch.

- **Story 7.4:** Tag v1.0.0 release with release notes summarizing v1.0 scope. Build and push Docker image to GitHub Container Registry. File the modules-page submission PR against `caddyserver/website` per the modules contribution process.
  - Acceptance: GitHub release published; Docker image pullable; modules-page PR submitted within 7 days of tag (R3 mitigation).

## Out of scope (re-stated for clarity)

Defer to [the project brief](./01-project-brief.md) §"MVP Scope > Out of scope" for the full list. Key v1.0 exclusions worth re-stating in PRD context because they're tempting to slip in:

- **SMTP transport** — moved to v1.1. Implementation **must not** start during v1.0 even if time remains.
- **CSRF / time-based tokens** — v1.1.
- **Persistent submission storage / retry queue** — v2. v1.0 retry is in-request only.
- **File attachments** — v2.
- **Webhook transport** — v2.
- **Health check endpoint, dry run mode, Prometheus metrics** — v1.1+.

If implementation goes faster than budgeted, additional v1.0 polish (better error messages, more validator coverage) is the right place to invest, not v1.1 features.

## Open questions (implementation-level)

These do not block the brief but need answers during implementation. None should change v1.0 scope.

1. **`trusted_proxies` syntax.** Does it accept a CIDR list, named presets (`cloudflare`), both? Recommend: CIDR list initially, with optional named presets if 30 minutes is available. Decide during Story 3.2.

2. **Body parsing — multipart vs urlencoded.** *(Decided in Story 1.2.)* v1.0 accepts both `application/x-www-form-urlencoded` and `multipart/form-data`. Multipart submissions with non-text parts (file uploads) fail with 400; file attachments are deferred to v2. Implementation lands when validation/templating reads form fields (Epic 4). Story 1.2's handler stub does not parse bodies.

3. **JSON request bodies.** Out of scope for v1.0 — only form-encoded bodies are accepted. Document explicitly.

4. **`from` field handling.** Postmark requires the `from` to be a verified Sender Signature on the account. The module should accept the `from` config value verbatim and let Postmark return a 4xx if it's unverified (which is then a terminal config error per FR20). Document this in the README under DNS/Postmark prerequisites.

5. **Subject template safety.** If the subject template references a missing field, Go templates render it as empty. Should this be a 422 instead of a degraded subject? Recommend: render as empty (matches Go template defaults; operators control template content; fail loud only on parse errors). Decide during Story 4.2.

6. **Reply-To handling.** When the form has an email field, does the email's `Reply-To` get set to that address? Recommend: yes by default, configurable via `reply_to_email_field <fieldname>` directive (default: the email field). Decide during Story 4.2.

## Traceability

Every FR and NFR maps back to a brief commitment. Quick reference:

| Source in [project brief](./01-project-brief.md) | Maps to |
|---|---|
| MVP > Transports | FR1, FR2, NFR9 |
| MVP > Spam protection | FR3, FR4, FR5, FR6, FR7, NFR4, NFR6 |
| MVP > Validation | FR8, FR9 |
| MVP > Email features | FR10, FR11 |
| MVP > Response handling | FR12, FR13, FR14 |
| MVP > Operational | FR15, FR16, FR17, NFR7, NFR8 |
| MVP > Failure handling | FR18, FR19, FR20, FR21, FR22, NFR5 |
| MVP > Security NFR | NFR1, NFR2 |
| Constraints | NFR9, NFR10, NFR11, NFR12, NFR13 |
| Done criteria | NFR14, NFR15, NFR16, Epic 7 |
| R4 mitigation | NFR2 |
| R5 mitigation | NFR15 |
