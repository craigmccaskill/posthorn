# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

_Nothing yet — next entry will become v1.0.1 or v1.1.0._

## [1.0.0] — 2026-05-16

Initial public release. The v1.0 spec is in [`spec/`](./spec/). Four feature blocks ship in a single release: form-mode HTTP ingress, API-mode HTTP ingress, multi-transport + operational maturity, and an SMTP listener. Originally sequenced as v1.0 → v1.1 → v1.2 → v1.3 themed releases; consolidated into v1.0 before tag.

### Block A — HTTP form ingress

- HTTP form ingress with multiple independent endpoints per config (FR1, FR2)
- Postmark HTTP API transport with bespoke ~80-line client (FR3, FR4, ADR-1)
- Honeypot field, Origin/Referer fail-closed check, max body size, token-bucket rate limit with LRU eviction at 10K IPs (FR5–FR9, NFR4, NFR6)
- Required-field and email-format validation returning structured 422 (FR10, FR11)
- Go `text/template` rendering for subject and body with custom-fields passthrough block (FR12, FR13)
- JSON responses, content negotiation, `redirect_success` / `redirect_error` (FR14–FR16)
- Per-request UUIDv4 `submission_id` in the 200 JSON body on both real-success and silent-honeypot paths — byte-identical body shape so a bot inspecting the response cannot distinguish honeypot rejection from success (FR5, NFR5)
- Retry policy: one retry on transient/5xx (1s), one retry on 429 (5s), no retry on 4xx config errors, 10s hard request timeout (FR19–FR22)
- Structured JSON logging with UUIDv4 submission IDs propagated through every log line; `submission_sent` carries `transport_message_id` so operators jump straight from Posthorn logs to the provider's UI (FR17, FR18, NFR7, NFR8)
- Standalone binary `cmd/posthorn` with `serve` and `validate` subcommands, SIGTERM/SIGINT graceful shutdown (FR24–FR26)
- Multi-stage Dockerfile producing a distroless static image for `linux/amd64` and `linux/arm64`, published to `ghcr.io/craigmccaskill/posthorn` on tag push (NFR12, NFR13)
- GitHub Actions CI: `go vet` and `go test -race -count=1`
- Public documentation site at [posthorn.dev](https://posthorn.dev) (Astro + Starlight)

### Block B — API mode

- API-mode endpoints via per-endpoint `auth = "api-key"` config; default `auth = "form"` preserves form-mode behavior (FR31, FR45)
- API-key authentication via `Authorization: Bearer <key>` with constant-time comparison (`crypto/subtle.ConstantTimeCompare`); multiple keys per endpoint for rotation (FR33, FR34, NFR19)
- Per-API-key rate limiting on API-mode endpoints — workers sharing egress IPs (Cloudflare Workers, etc.) get independent buckets (FR35)
- JSON content type on API-mode endpoints (`application/json`); flat-object body shape with primitive type coercion to template variables; nested objects rejected with 400 (FR36, FR37, FR38, FR39)
- Idempotency keys via standard `Idempotency-Key` header; in-memory per-endpoint LRU cache, 24-hour TTL, byte-identical response replay; HTTP 409 on concurrent in-flight collisions (FR40–FR44, NFR20)
- New `core/idempotency/` package; new `idempotency_cache_size` endpoint config (default 10000)
- Per-request `to_override` in api-mode JSON bodies — string or array of email addresses replaces the endpoint's `to` list for the request. Each address validated as syntactic email; empty array or any invalid address returns 422. `from` is intentionally not overridable to prevent spoofing via leaked keys (FR46, ADR-11)
- API keys configured via `${env.VAR}` are subject to NFR3's "never appear in log output" invariant; tests verify with sentinel-key assertions (NFR21)

### Block C — Multi-transport + operational maturity

- **Resend** HTTP API transport — bespoke client (~150 LOC); Bearer auth; JSON body; ErrorClass mapping (FR47)
- **Mailgun** HTTP API transport — bespoke client (~180 LOC); HTTP Basic auth; multipart/form-data body via `mime/multipart.Writer`; US + EU regions (FR48)
- **AWS SES** transport — bespoke SigV4 implementation (~230 LOC) shared in `core/transport/awssigv4.go` for any future AWS-signed transport (S3, SNS) + SESv2 `SendEmail` request shape (~265 LOC) (FR49, ADR-14)
- **Outbound SMTP** transport — stdlib `net/smtp.PlainAuth` + STARTTLS; per-Send connect/AUTH/MAIL/RCPT/DATA/QUIT cycle; 30s per-Send timeout (FR50, ADR-17)
- Transport registry pattern — new file in `core/transport/` calls `Register()` in `init()`; config + cmd/posthorn dispatch through `Lookup()`. Adding a sixth transport requires zero edits to config or main (FR4, FR51, FR52, FR53)
- `/healthz` endpoint returning `200 OK` with body `ok`; auth-free, fixed path (FR54)
- `/metrics` endpoint with hand-rolled Prometheus text exposition (FR55, ADR-15) — counters for submissions received / sent / failed (with `error_class`), rate-limit hits, auth failures, spam blocks, idempotent replays, validation failures; latency histogram with operator-meaningful buckets. Submitter content never enters the label space (NFR24)
- New `core/metrics/` package: `Registry`, `Counter`, `Histogram`, exposition writer; nil-safe `Recorder` for gateway integration
- Dry-run mode via per-endpoint `dry_run = true` — runs full pipeline up to but not including `transport.Send`, returns 200 with `{"status":"dry_run","submission_id":"...","prepared_message":{...}}` (FR56)
- CSRF tokens via HMAC-SHA256 over Unix timestamp — operator-issued at form-render time using `csrf_secret`; Posthorn verifies on submit; form-mode only (api-mode rejects `csrf_secret` at parse time) (FR57, ADR-16)
- New `core/csrf/` package with `Issue` / `Verify` helpers; `csrf_secret` and `csrf_token_ttl` endpoint config; `_csrf_token` reserved form field name
- Named `trusted_proxies` presets — `cloudflare` shipped in full; `aws-elb`, `gcp-lb`, `azure-front-door` reserved as empty slots awaiting maintained ranges. Mix presets and explicit CIDRs in one list (FR58)
- `strip_client_ip` endpoint option — omits the resolved client IP from log lines (rate-limited, etc.) for GDPR-conscious deployments; rate-limit keying unaffected (FR59)

### Block D — SMTP ingress

- New `core/ingress/` package: minimal `Ingress` interface (Start/Stop/Name); `HTTPIngress` wraps `http.Server` for the v1.0 lifecycle (FR60, FR61, ADR-12)
- New `core/smtp/` package: TCP listener accepting SMTP from internal clients; full state machine for EHLO/STARTTLS/AUTH/MAIL/RCPT/DATA/QUIT/RSET/NOOP (FR62)
- SMTP AUTH PLAIN with constant-time password compare; client-cert auth as alternative (`auth_required = "either"`) (FR63)
- STARTTLS required by default (`require_tls = true`); rejected before AUTH/MAIL when plaintext (FR67)
- Sender allowlist (`allowed_senders`, exact-or-`*@domain` syntax) — required, non-empty (FR64)
- Recipient cap OR allowlist (default cap 10 per session) preventing RCPT bombing (FR65)
- Message size cap (`max_message_size`, default 1MB) — DATA exceeding returns `552 5.3.4` (FR66)
- MIME → `transport.Message` conversion (FR68): recipients come from SMTP envelope (`RCPT TO`), never from MIME `To`/`Cc`/`Bcc` headers — a malicious DATA blob with smuggled `Bcc:` cannot add recipients (NFR22)
- Per-session structured logging with `session_id` UUID; password never logged on auth failures (NFR23)
- `cmd/posthorn` starts both HTTP and SMTP ingresses in their own goroutines when `[smtp_listener]` is configured; SIGTERM drains both with a 15s deadline

### Removed (before tag)

- The Caddy v2 adapter module (previously planned as a secondary deployment shape under FR27–FR30 / NFR10 / ADR-6 / ADR-7) was cut before tagging v1.0.0. The single-shape standalone-behind-any-reverse-proxy story keeps the product thesis cleaner and avoids ongoing per-feature carve-outs. Caddy users keep first-class support as a reverse proxy — see [posthorn.dev/deployment/reverse-proxy](https://posthorn.dev/deployment/reverse-proxy/).
- Batch send API (originally listed as a v1.1 feature) was dropped 2026-05-16 — see [spec/01-project-brief.md](spec/01-project-brief.md) §"Deliberately not on the roadmap" for the reasoning. Reconsidered if a concrete operator workload surfaces with the need.

### Dependencies

Three external Go dependencies in the entire module: `github.com/BurntSushi/toml`, `github.com/google/uuid`, `github.com/hashicorp/golang-lru/v2`. Every transport (Postmark, Resend, Mailgun, SES, outbound-SMTP) is bespoke — no vendor SDK in transport code per [ADR-1](spec/03-architecture.md).
