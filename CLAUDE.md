# CLAUDE.md — Posthorn

Auto-loaded by Claude Code at session start. Captures the durable project context, current status, and the guardrails that need to be in front of every code change. Build/test commands, commit conventions, and contributor scope policy live in [CONTRIBUTING.md](./CONTRIBUTING.md) — read both at the start of any code session.

## Project context

Posthorn is the **unified outbound mail layer for self-hosted projects** — the gateway between an operator's apps and a transactional mail provider they already chose. v1.0 ships three ingress shapes (HTTP form, HTTP API mode with Bearer auth and idempotency, SMTP listener with AUTH PLAIN + STARTTLS), five transports (Postmark, Resend, Mailgun, AWS SES with bespoke SigV4, outbound-SMTP relay), and an operational surface (`/healthz`, `/metrics` Prometheus exposition, dry-run, CSRF tokens, IP-stripping, named `trusted_proxies` presets). v2 adds platform features (durable storage, suppression, lifecycle webhooks, attachments).

Originally sequenced as v1.0 → v1.1 (API mode) → v1.2 (multi-transport + ops) → v1.3 (SMTP ingress) themed releases. Folded into a single v1.0 release on 2026-05-16 after evaluation concluded the surface area was small enough that splitting them into four releases produced more carve-outs than coherent product moments. v2 remains the next planned milestone.

The original wedge — cloud-blocks-SMTP — is preserved as the canonical discovery entry point. The broader value is the unified-layer pattern (the same outbound concern duplicated across N self-hosted apps now centralizes through one Posthorn gateway), which applies even where SMTP is unblocked.

The full project history (initial scope as a Caddy form handler called `caddy-formward`, the 2026-04-27 scope expansion, the 2026-05-15 positioning sharpening, the 2026-05-16 v1.x-fold-into-v1.0) is in [`spec/01-project-brief.md`](./spec/01-project-brief.md) §"Status log". Don't re-derive — read the spec.

## Design principles (short)

These pin the shape of Posthorn across versions and override new feature requests that conflict with them. Full reasoning in [`spec/01-project-brief.md`](./spec/01-project-brief.md) §"Design principles".

1. **Gateway, not infrastructure.** Posthorn sits between apps and a provider — it doesn't replace the provider, doesn't run its own outbound SMTP, doesn't manage IP reputation, doesn't host mailboxes.
2. **Integration layer, not mail-receiving layer.** Posthorn unifies outbound (many ingress shapes → one transport surface). It does not unify inbound (MX hosting / receive-side filtering / mailbox storage); those are mail-server concerns.
3. **No feature-count competition with category leaders.** Stalwart owns mail-server territory; Postal owns outbound-platform territory; Listmonk owns marketing. We don't try to match them on feature count in their lanes.
4. **Config files over admin UIs.** A single TOML file is the source of truth. No runtime mutation surface that could drift from the config file. v3+ may add read-only UIs for browsing logs; not for configuration.
5. **Bespoke before SDK, when the surface is small.** Postmark / Resend / Mailgun / SES integrations are written directly (stdlib + minimal deps), not via vendor SDKs. The rule of thumb: bespoke when ~200 lines suffices; SDK when bespoke would be 1000+. [ADR-1](./spec/03-architecture.md#architectural-decisions-log) elevated project-wide.

When a feature request or implementation proposal conflicts with one of these, the principle wins. Take it back to spec discussion before changing code.

## Status (as of 2026-05-16, end of day)

**Phase:** v1.0 release prep complete. All implementation landed. Tag pending two outstanding items: operator validation (External) and tag-day decision (Decision).

**Repo state:** Single Go module at `core/` with 14 packages plus `cmd/posthorn/`:
- v1.0 originals: `config`, `gateway`, `transport`, `spam`, `ratelimit`, `validate`, `template`, `response`, `log`
- Added 2026-05-16 (block B+C+D): `idempotency`, `csrf`, `metrics`, `ingress`, `smtp`

Public docs site at [posthorn.dev](https://posthorn.dev) (Astro + Starlight, deployed via GH Pages from `site/`); 41 pages.

**Major scope expansion (2026-05-16): v1.1 + v1.2 + v1.3 all folded into v1.0.** Originally locked v1.0 scope was form ingress + Postmark transport (Epics 1-7). Through the day's session the scope rescoped twice:
1. v1.1 amendment (API mode) added Epic 8: API-key auth, JSON ingress, idempotency, `to_override`. FR31-FR46, NFR19-NFR21, ADRs 8-11.
2. Audit + decision to fold v1.2 and v1.3 into the same v1.0 release: multi-transport (Resend/Mailgun/SES/SMTP-out), ops features (healthz/metrics/dry-run/CSRF/presets/IP-strip), SMTP ingress with Ingress interface. Epics 9-11 added. FR47-FR68, NFR22-NFR24, ADRs 12-17.

The "originally v1.x" labels are preserved as block A/B/C/D inside v1.0 for historical traceability. v2 remains future scope (stateful platform: durable storage, suppression, lifecycle webhooks, attachments).

**Completed Epics:**
- ✅ Epic 1 (1.1-1.3) — rename, code reorganization into `core/`
- ✅ Epic 2 (2.1-2.5) — TOML config, HTTP handler, validation, templating, cmd/posthorn
- ✅ Epic 3 (3.1-3.2) — spam protection, rate limiting
- ✅ Epic 4 (4.1-4.2) — retry policy, structured JSON logging
- ✅ Epic 5 (5.1-5.3) — multi-stage Dockerfile, CI workflow, multi-arch release workflow (validated end-to-end via `v0.0.1-test` tag)
- ❌ Epic 6 (Caddy adapter) — **retired 2026-05-15**
- ✅ Epic 7 (7.1-7.2) — README, OSS hygiene files. (7.3 tag itself is part of Epic 12 now.)
- ✅ Epic 8 (8.1-8.5) — API mode block: config schema, auth + per-key rate-limit, JSON ingress, idempotency cache, `to_override` (added later same day as FR46/ADR-11)
- ✅ Epic 9 (9.1-9.6) — Transport registry + Resend + Mailgun + SES (bespoke SigV4, ADR-14 tripwire fired and deviation accepted) + outbound-SMTP + per-transport docs pages
- ✅ Epic 10 (10.1-10.5) — `/healthz` + `/metrics` (hand-rolled Prometheus exposition + Recorder), dry-run, CSRF, `trusted_proxies` presets (`cloudflare` shipped in full; aws-elb/gcp-lb/azure-front-door reserved empty slots), IP-stripping
- ✅ Epic 11 (11.2-11.8) — `Ingress` interface + HTTPIngress wrapper, SMTP listener (TCP accept loop, full state machine, AUTH PLAIN + client-cert, STARTTLS, sender + recipient allowlists, MIME → `transport.Message` with NFR22 envelope-only invariant, binary integration, docs + manual-test extension)
- ✅ Epic 12 (12.1-12.2) — doc rescope sweep across PRD/architecture/site/CHANGELOG, README rewrite

**Outstanding (Epic 12 tail):**
- ⏳ **Story 12.3 — Operator validation** [External]. Manual-test procedures exist for all paths (form mode + API mode + each non-Postmark transport + SMTP ingress) in [docs/manual-test.md](docs/manual-test.md). Needs real provider accounts:
  - Postmark — already validated 2026-05-16 morning
  - Resend, Mailgun (US or EU domain), AWS SES (out-of-sandbox), and an SMTP relay (Mailgun SMTP, Mailtrap, or a Postfix smarthost) — not yet validated
  - SMTP listener validation needs a self-signed cert + `swaks` against a reachable address
- ⏳ **Story 12.4 — Tag v1.0.0** [Decision]. Operator-owned timing.

**Architecture deviations from original spec (cumulative):**
- `core/http/` → `core/gateway/` to avoid shadowing stdlib `net/http`.
- Retry timing constants declared as package vars (not consts) so tests can override via `gateway.SetRetryDelaysForTest`.
- [`site/`](./site/) Astro + Starlight directory; deploys via [`.github/workflows/site-deploy.yml`](./.github/workflows/site-deploy.yml). Build: `cd site && npm ci && npm run build`.
- **2026-05-15:** Caddy v2 adapter module cut. FR27–FR30, NFR10 deleted; ADR-6, ADR-7 retired in-place.
- **2026-05-15:** Honeypot 200 response shape parity (commit `0f27f4c`).
- **2026-05-15:** v1.0 recipes shipped (commit `250f460`).
- **2026-05-16 (morning):** `transport.Transport.Send` signature changed from `error` to `(SendResult, error)`. `SendResult.MessageID` captures the upstream's message ID; logged as `transport_message_id` in `submission_sent`. Forward-compatible for all transports added later that day.
- **2026-05-16 (during day):** Batch send dropped from v1.1 scope entirely. Documented in brief's "Deliberately not on the roadmap" section. The user pushed the "ships when an operator surfaces with concrete batch volume" framing.
- **2026-05-16 (later day):** Per-request `to_override` added to api-mode (FR46, ADR-11). Originally deferred (D5 in design discussion) but the Cloudflare Worker recipe surfaced that the named v1.1 audience can't pre-declare recipients. `from` stays endpoint-configured to prevent spoofing.
- **2026-05-16 (end of day):** v1.x scope collapsed into a single v1.0 release (see "Major scope expansion" above). PRD amendment headings reorganized as block B/C/D. CHANGELOG consolidated.
- **2026-05-16:** ADR-14 tripwire fired during SES SigV4 implementation. Total LOC (including tests) reached 1,158; implementation-only 493. Deviation accepted (option a in the ADR) — tests are 400-500 LOC per transport uniformly across the codebase, so the "include tests in tripwire" wording was overly conservative.

**Deps in module:** `github.com/BurntSushi/toml`, `github.com/google/uuid`, `github.com/hashicorp/golang-lru/v2`. Three external deps for the whole codebase. Every transport bespoke.

**Launch artifacts in the vault (not in this repo):**
- [`~/vaults/cmcc/Areas/Blog/Posthorn Launch.md`](~/vaults/cmcc/Areas/Blog/Posthorn Launch.md) — launch blog post, written 2026-05-15 in post-v1.3 tense. After the v1.0=everything-through-v1.3 rescope, this is now publishable as the v1.0 announcement (no rewrite needed; the prose was always describing the full surface).
- [`~/vaults/cmcc/Projects/Posthorn/Distribution playbook.md`](~/vaults/cmcc/Projects/Posthorn/Distribution playbook.md) — HN Show post draft, awesome-self-hosted PR draft, Docker Hub README, blog cross-posting plan, Reddit, SEO, awesome-postmark check, pre-staged GitHub Release notes. Some pre-staged text was written assuming the v1.x ordering — the HN body in particular may need a pass before posting since it referenced v1.0 = form-only.

**Test count check (as of 2026-05-16):** all packages pass `go vet ./... && go test -race -count=1 ./...` from `core/`. Site builds 41 pages. SMTP package added 21 tests; idempotency package 16 tests; metrics package 14 tests; csrf package 9 tests. Total project test count roughly doubled from morning.

## Read the spec before touching code

The v1.0 specification is across three documents in [`spec/`](./spec/):

1. **[`spec/01-project-brief.md`](./spec/01-project-brief.md)** — problem, users, scope, success metrics, threat model, risks, constraints. Status log captures the major scope decisions (2026-04-27 initial, 2026-04-27 SMTP-broader scope, 2026-05-15 Caddy cut, 2026-05-16 v1.1 amendment + batch drop + `to_override` add + v1.x-fold-into-v1.0).
2. **[`spec/02-prd.md`](./spec/02-prd.md)** — FR1–FR68 and NFR1–NFR24 organized as four blocks (A: form ingress, B: API mode, C: multi-transport + ops, D: SMTP ingress). FR27–FR30 / NFR10 / NFR14 are deleted slots from the 2026-05-15 Caddy adapter cut. Epics 1–12. Traceability table maps every FR to a brief section.
3. **[`spec/03-architecture.md`](./spec/03-architecture.md)** — file layout, lifecycle, request flow, component design, threat→defense mapping (now covers HTTP form + API mode + SMTP threats), dependencies, ADRs 1–17, forward-compatibility commitments for v2.

The PRD has the canonical FR/NFR list with "must"-level requirements; the architecture doc has the implementation blueprint, including the target file layout under §"File layout".

## Hard guardrails

These derive from the spec. Do not violate without an explicit conversation that updates the spec first.

1. **Scope is v1.0** (where v1.0 = blocks A + B + C + D, the full scope of the consolidated 2026-05-16 release). Do not implement v2 features: SQLite storage, durable retry queue, suppression list, lifecycle webhooks, HTML body, file attachments, automatic unsubscribe injection, multi-tenant SMTP routing, multiple outputs per endpoint. v3 features (admin UI, proof-of-work, PGP) are even further out.

2. **Header injection prevention is mandatory (NFR1, NFR2, NFR22).** Submitter-controlled fields **must never** be interpolated as raw strings into email headers, at any layer. Every transport must pass the header-injection test suite (CRLF in name/email/subject/recipients, `\r\nBcc:`, header smuggling). The SMTP listener's specific NFR22 invariant: outbound recipients come from the SMTP envelope (`RCPT TO`), never from inbound MIME `To`/`Cc`/`Bcc` headers. Non-negotiable.

3. **API keys must never be logged (NFR3, NFR21).** Set them as HTTP headers / Basic auth / SMTP AUTH at construction time; never pass to the logger. Tests must verify by triggering failure paths and asserting the sentinel-key string does not appear in captured log output. This applies to every transport (Postmark, Resend, Mailgun, SES, outbound-SMTP) and to API-mode endpoint `api_keys`, CSRF `csrf_secret`, and SMTP `smtp_users` passwords.

4. **Origin/Referer fail-closed (FR6, NFR4).** When `allowed_origins` is configured and both `Origin` and `Referer` headers are missing, reject the request. Explicitly-empty `allowed_origins = []` is a config parse error.

5. **Mode-mutex parse-time rejection (ADR-10).** API-mode endpoints reject `honeypot`, `allowed_origins`, `redirect_success`, `redirect_error`, `csrf_secret` at parse time. The footgun this prevents is operators configuring a defense they think is active but isn't.

6. **NFR24: no submitter content in metric labels.** `/metrics` label values come only from operator-configured names (endpoint paths, transport types, error class enum). High-cardinality submitter content (recipient addresses, subjects, body fragments) must never enter the label space. This is structurally true because the `metrics.Recorder` API doesn't accept request-side values, but tests verify it.

7. **Every active FR/NFR traces back to the brief.** If you find yourself writing something not traceable to a spec requirement, stop and check the spec rather than improvising. (FR27–FR30, NFR10, NFR14 are deleted slots from the 2026-05-15 Caddy adapter cut; don't resurrect them.)

8. **Follow the architecture doc's file layout.** Single Go module at `core/` with internal sub-packages (per [`spec/03-architecture.md`](./spec/03-architecture.md) §"File layout"). No second module; no Caddy adapter.

9. **Don't reintroduce the Caddy adapter.** Cut 2026-05-15 after a deliberate product-level conversation about single-shape simplicity. If a future request asks to bring it back, treat it as scope discussion, not implementation — re-open the brief's status log before any code change.

10. **Don't add batch send unless a concrete operator workload demands it.** Deferred 2026-05-16; see brief's "Deliberately not on the roadmap" section. The reasoning is in the status log — read it before reconsidering.

11. **`from` is endpoint-configured; per-request override forbidden (ADR-11).** Per-request `to_override` is fine and shipped. Per-request `from` enables spoofing via leaked API keys; don't add it.

## Architectural decisions worth knowing

ADRs are recorded in [`spec/03-architecture.md`](./spec/03-architecture.md) §"Architectural decisions log". Active ADRs and what they mean:

- **ADR-1:** No third-party transport SDKs. Bespoke clients ~80-300 LOC each. Five transports ship this way (Postmark, Resend, Mailgun, SES, outbound-SMTP).
- **ADR-2 (revised twice):** Single Go module at `core/` with internal sub-packages.
- **ADR-3:** Hand-rolled token-bucket rate limiter, not `golang.org/x/time/rate` (need LRU eviction at 10K IPs / NFR6).
- **ADR-4:** `*bool` for `LogFailedSubmissions` to distinguish unset from explicitly-false.
- **ADR-5:** Synchronous send, not async-with-queue. v2's SQLite + queue is the swap point.
- **ADR-6, ADR-7 (retired 2026-05-15):** Caddy-adapter-era decisions, no longer meaningful.
- **ADR-8 (2026-05-16):** Per-endpoint in-memory LRU cache for idempotency, not Redis/SQLite/global-with-prefixes. v2 swaps backend behind same package interface.
- **ADR-9 (2026-05-16):** HTTP 409 on in-flight idempotency-key collision, not blocking. Surfaces retry-without-backoff caller bugs rather than hiding them.
- **ADR-10 (2026-05-16):** Mode/defense mutex enforced at config-parse time. API-mode rejects `honeypot`/`allowed_origins`/`redirect_*`/`csrf_secret`.
- **ADR-11 (2026-05-16):** Per-request `to_override` allowed; per-request `from` forbidden. Asymmetry follows from leaked-key blast radius — `to` is recoverable, `from` enables irreversible spoofing.
- **ADR-12 (2026-05-16):** `Ingress` interface produces a complete `transport.Message`. HTTP-specific concepts (templates, content negotiation, idempotency keys, CSRF) do not cross the boundary. Egress (transport.Send) is ingress-agnostic.
- **ADR-13 (2026-05-16):** One SMTP listener has one outbound transport. No per-RCPT routing in v1.0 (v2 territory).
- **ADR-14 (2026-05-16):** AWS SES SigV4 implemented bespoke. Tripwire fired at impl time (1158 total LOC with tests, 493 impl-only); deviation accepted because test bulk is uniform across transports, SigV4 is reusable for future AWS-signed transports (S3 storage in v2, SNS in v2), and pulling in aws-sdk-go-v2 would import a 30+ package transitive dep tree.
- **ADR-15 (2026-05-16):** Prometheus `/metrics` exposition is hand-rolled (~50 LOC). No `prometheus/client_golang` dep.
- **ADR-16 (2026-05-16):** CSRF tokens are HMAC-signed timestamps, issued by operator at form-render time (`csrf_secret` never crosses to client). No cookies, no token-fetch round-trip.
- **ADR-17 (2026-05-16):** Outbound-SMTP and inbound-SMTP both use stdlib `net/smtp` + `crypto/tls` directly. Stdlib-first when the surface is small.

If you find yourself wanting to deviate from an active ADR, update the architecture doc with the new decision and rationale before changing code.

## When in doubt

1. Re-read the relevant spec section
2. If the spec is silent or ambiguous, the architecture doc's "Open architectural questions" section may already have a recommended answer
3. If neither helps, ask the author before improvising

The cost of asking is small. The cost of building the wrong thing is real LOC + test surface to walk back.
