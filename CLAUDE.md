# CLAUDE.md — Posthorn

Auto-loaded by Claude Code at session start. Captures the durable project context, current status, and the guardrails that need to be in front of every code change. Build/test commands, commit conventions, and contributor scope policy live in [CONTRIBUTING.md](./CONTRIBUTING.md) — read both at the start of any code session.

## Project context

Posthorn is a self-hosted email gateway for cloud platforms that block outbound SMTP. v1.0 ships an HTTP form ingress and a Postmark HTTP API transport, deployed as a standalone Go binary (Docker primary) or as an optional Caddy module adapter.

The full project history (initial scope as a Caddy form handler called `caddy-formward`, the 2026-04-27 scope expansion to email gateway, the rename to Posthorn) is in [`spec/01-project-brief.md`](./spec/01-project-brief.md) §"Status log". Don't re-derive — read the spec.

## Status (as of 2026-04-27)

**Phase:** pre-v1.0 implementation. Spec is locked.

**Repo state:** Two-module workspace established. Core has three packages so far — `transport/` (Transport interface + Postmark client, NFR1/2/3 covered), `config/` (TOML loader with env-var resolution and full validation), `gateway/` (HTTP form handler skeleton). The `caddy/` adapter module is a stub awaiting Epic 6.

**Completed stories:**
- ✅ Epic 1 Story 1.1 — repo renamed `caddy-formward` → `posthorn`
- ✅ Epic 1 Story 1.2 — two-module workspace + `go.work` + transport migration
- ✅ Epic 1 Story 1.3 — core has zero Caddy dependency
- ✅ Epic 2 Story 2.1 — TOML config loader (`core/config/`)
- ✅ Epic 2 Story 2.2 — HTTP form handler skeleton (`core/gateway/`)

**Current story:** Epic 2 Story 2.3 — required-fields and email-format validation in `core/validate/`. Pure functions, returns lists of failed fields. Wires into the gateway handler in Epic 3.

After each story ships, update this "Current story" pointer.

**Architecture deviation:** original spec said `core/http/`; implementation uses `core/gateway/` (package `gateway`) to avoid shadowing stdlib `net/http`. Architecture and PRD updated for consistency.

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
