# CLAUDE.md — Posthorn

Auto-loaded by Claude Code at session start. Captures the durable project context, current status, and the guardrails that need to be in front of every code change. Build/test commands, commit conventions, and contributor scope policy live in [CONTRIBUTING.md](./CONTRIBUTING.md) — read both at the start of any code session.

## Project context

Posthorn is a self-hosted email gateway for cloud platforms that block outbound SMTP. v1.0 ships an HTTP form ingress and a Postmark HTTP API transport, deployed as a standalone Go binary (Docker primary) or as an optional Caddy module adapter.

The full project history (initial scope as a Caddy form handler called `caddy-formward`, the 2026-04-27 scope expansion to email gateway, the rename to Posthorn) is in [`spec/01-project-brief.md`](./spec/01-project-brief.md) §"Status log". Don't re-derive — read the spec.

## Status (as of 2026-04-27)

**Phase:** pre-v1.0 implementation. Spec is locked. Epics 1-4 complete; Epics 5-7 remain.

**Repo state:** Two-module workspace with the standalone gateway functionally complete. Core has 9 packages: `transport/`, `config/`, `gateway/`, `validate/`, `template/`, `response/`, `spam/`, `ratelimit/`, `log/`, plus `cmd/posthorn/` for the binary. The `caddy/` adapter module is still a stub awaiting Epic 6. **2,240 source lines, 3,922 test lines, 212 tests, all passing.**

The full request pipeline is wired end-to-end: body cap → method → content-type → origin → rate limit → parse → honeypot → required fields → email format → template render → transport send (with FR19-22 retry under 10s hard timeout) → JSON 200 or 502. Every decision point logs structured JSON with a per-request UUID submission_id.

**Completed stories (12 of 21):**
- ✅ Epic 1 (Stories 1.1-1.3) — rename, workspace restructure, zero-Caddy-dep enforcement
- ✅ Epic 2 (Stories 2.1-2.5) — TOML config, HTTP handler, validation, templating, cmd/posthorn
- ✅ Epic 3 (Stories 3.1-3.2) — spam protection, rate limiting
- ✅ Epic 4 (Stories 4.1-4.2) — retry policy, structured JSON logging

**Remaining stories (9 of 21):**
- ⏳ Epic 5 (Stories 5.1-5.3, ~2.5h) — Dockerfile, GitHub Actions CI, multi-arch release workflow
- ⏳ Epic 6 (Stories 6.1-6.3, ~2.5h) — Caddy adapter module, Caddyfile unmarshaler, parity test
- ⏳ Epic 7 (Stories 7.1-7.3, ~3h) — README, OSS hygiene files, v1.0.0 tag, modules-page submission

**Current story:** Epic 5 Story 5.1 — `core/Dockerfile` using multi-stage build (golang:1.25 builder → gcr.io/distroless/static runtime). Image entrypoint runs `posthorn serve --config /etc/posthorn/config.toml`.

**Budget:** ~13.5h burned of 25h v1.0 budget. Tracking on plan.

After each story ships, update this "Current story" pointer.

**Architecture deviations from original spec:**
- `core/http/` → `core/gateway/` (package `gateway`) to avoid shadowing stdlib `net/http`. Architecture and PRD updated.
- Retry timing constants (`requestTimeout`, `transientRetryDelay`, `rateLimitedRetryDelay`) declared as package vars, not consts, so tests can override via the test-only helper `gateway.SetRetryDelaysForTest` (in `core/gateway/export_test.go`). Production never mutates them.
- [`site/`](./site/) Astro + Starlight directory added at repo root for the posthorn.dev marketing/docs site (2026-05-14). Not in original v1.0 spec scope — treated as launch infrastructure outside the 25h budget. Deploys to GitHub Pages via [`.github/workflows/site-deploy.yml`](./.github/workflows/site-deploy.yml). Custom domain in [`site/public/CNAME`](./site/public/CNAME). Build: `cd site && npm ci && npm run build`. Sidebar config and theming live in [`site/astro.config.mjs`](./site/astro.config.mjs).

**Deps added during implementation:** `github.com/BurntSushi/toml` (config), `github.com/hashicorp/golang-lru/v2` (rate limiter), `github.com/google/uuid` (submission IDs). All three were named in the architecture doc's allowed-deps list.

## Read the spec before touching code

The v1.0 specification is locked across three documents in [`spec/`](./spec/):

1. **[`spec/01-project-brief.md`](./spec/01-project-brief.md)** — problem, users, scope, success metrics, threat model, risks, constraints
2. **[`spec/02-prd.md`](./spec/02-prd.md)** — 30 functional requirements, 18 non-functional requirements, 7-epic breakdown with 22 stories and acceptance criteria
3. **[`spec/03-architecture.md`](./spec/03-architecture.md)** — file layout, lifecycles for both deployment shapes, request flow, component design, threat-to-defense mapping, dependencies, ADRs, forward-compatibility commitments for v1.x

The PRD has the canonical FR/NFR list with "must"-level requirements; the architecture doc has the implementation blueprint, including the target file layout under §"File layout".

## Hard guardrails

These derive from the locked spec. Do not violate without an explicit conversation that updates the spec first.

1. **Scope is v1.0 only.** Do not implement SMTP ingress, additional transports beyond Postmark, CSRF tokens, file attachments, webhook transport, SQLite storage, admin UI, or any feature listed in the brief's §"Out of scope". Even if implementation goes faster than the 25-hour budget, additional time goes to v1.0 polish (better errors, more validator coverage), never to v1.1+ features.

2. **Budget tripwires.**
   - 25-hour total implementation budget for v1.0
   - 90-day calendar tripwire from project rename (2026-04-27 → 2026-07-26) to v1.0.0 tag
   - If 25h hits with no end in sight: cut Caddy adapter from v1.0 release first (ship as v1.1), then cut polish features. The standalone gateway core is non-cuttable — it's the whole product.

3. **Core has zero Caddy dependency (ADR-6).** The `core/go.mod` file **must not** import `github.com/caddyserver/caddy/v2` or any Caddy sub-package. This is structurally enforced: the two-module workspace layout means a Caddy import in core fails compilation. Any code that needs Caddy types (e.g., `caddy.Context`, `caddyhttp.Handler`) belongs in the `caddy/` adapter module, not core.

4. **Header injection prevention is mandatory (NFR1, NFR2).** Submitter-controlled fields **must never** be interpolated as raw strings into email headers. The Postmark transport must use Postmark's structured JSON API fields. The test suite must include explicit injection-payload coverage (CRLF in name/email/subject, `\r\nBcc:`, header smuggling). This is non-negotiable — see Risk R4.

5. **API keys must never be logged (NFR3).** Set them as HTTP headers during request construction, never log them in error or debug output. Tests must verify by triggering transport failures and asserting the key string does not appear in captured log output.

6. **Origin/Referer fail-closed (FR6, NFR4).** When `allowed_origins` is configured and both `Origin` and `Referer` headers are missing, reject the request. When `allowed_origins` is configured as an explicitly empty list, the parser must reject the configuration — no fail-open default for an explicitly empty allowlist.

7. **Every FR/NFR traces back to the brief.** If you find yourself writing something not traceable to a spec requirement, stop and check the spec rather than improvising.

8. **Follow the architecture doc's file layout exactly.** Two-module workspace (`core/` + `caddy/`) with `go.work` joining them. Internal sub-packages within `core/` per the layout in [`spec/03-architecture.md`](./spec/03-architecture.md) §"File layout". The `caddy/` adapter module is thin (~150 LOC) — all business logic lives in core.

9. **Standalone is the primary deployment shape (ADR-7).** Documentation, examples, and CI workflows put the standalone Docker path first. The Caddy adapter is correct, tested, and discoverable, but it is not the headline. Don't accidentally elevate the adapter to primary in any new docs.

## Architectural decisions worth knowing

Seven ADRs are recorded in [`spec/03-architecture.md`](./spec/03-architecture.md) §"Architectural decisions log". The most likely to come up during implementation:

- **ADR-1:** No third-party Postmark SDK. ~80 lines of bespoke HTTP client.
- **ADR-2 (revised):** Two-module workspace, internal sub-packages within `core/`. Replaces the prior flat-package decision from `caddy-formward` — the expanded scope justifies the structure.
- **ADR-3:** Hand-rolled token-bucket rate limiter, not `golang.org/x/time/rate`. We need LRU eviction at 10K IPs (NFR6) which `x/time/rate` doesn't provide.
- **ADR-6:** Core has zero Caddy dependency; adapter imports core, never the reverse. The load-bearing decision that makes the standalone-with-adapter architecture work.
- **ADR-7:** Standalone is primary, Caddy adapter is optional. Distribution emphasis on Docker; adapter gets equal correctness attention but secondary marketing attention.

If you find yourself wanting to deviate from any ADR, update the architecture doc with the new decision and rationale before changing code.

## When in doubt

1. Re-read the relevant spec section
2. If the spec is silent or ambiguous, the architecture doc's open questions section ([`spec/03-architecture.md`](./spec/03-architecture.md) §"Open architectural questions") may already have a recommended answer
3. If neither helps, ask the author before improvising

The cost of asking is small. The cost of building the wrong thing is the entire 25-hour budget.
