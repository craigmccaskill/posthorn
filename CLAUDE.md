# CLAUDE.md — caddy-formward

Auto-loaded by Claude Code at session start. Captures the durable project context, current status, and the guardrails that need to be in front of every code change. Build/test commands, commit conventions, and contributor scope policy live in [CONTRIBUTING.md](./CONTRIBUTING.md) — read both at the start of any code session.

## Project context

caddy-formward is a Caddy v2 HTTP handler module that receives form submissions and delivers them via pluggable transports. v1.0 ships with a single Postmark HTTP API transport, designed for hosts where outbound SMTP is blocked (DigitalOcean, AWS, etc.).

The full project history (rename from "caddy-mailout", why v1.0 is Postmark-only) is in [`spec/01-project-brief.md`](./spec/01-project-brief.md) §"Status log" and §"Problem Statement". Don't re-derive — read the spec.

## Status (as of 2026-04-27)

**Phase:** pre-v1.0 implementation. Spec is locked.

**Repo state:** Stories 1.1 and 1.2 complete (module scaffolding, Caddyfile unmarshaler with path matcher, minimal handler returning 200 OK).

**Current story:** Story 1.3 — JSON config support and Caddyfile-to-JSON round-trip via `caddy adapt`. See [`spec/02-prd.md`](./spec/02-prd.md) §"Epic 1".

After each story ships, update this "Current story" pointer.

## Read the spec before touching code

The v1.0 specification is locked across three documents in [`spec/`](./spec/):

1. **[`spec/01-project-brief.md`](./spec/01-project-brief.md)** — problem, users, scope, success metrics, threat model, risks, constraints
2. **[`spec/02-prd.md`](./spec/02-prd.md)** — 22 functional requirements, 16 non-functional requirements, 7-epic breakdown with 22 stories and acceptance criteria
3. **[`spec/03-architecture.md`](./spec/03-architecture.md)** — file layout, Caddy lifecycle, request flow, component design, threat-to-defense mapping, dependencies, ADRs

The PRD has the canonical FR/NFR list with "must"-level requirements; the architecture doc has the implementation blueprint, including the target file layout under §"File layout".

## Hard guardrails

These derive from the locked spec. Do not violate without an explicit conversation that updates the spec first.

1. **Scope is v1.0 only.** Do not implement SMTP transport, CSRF tokens, file attachments, webhook transport, SQLite storage, admin UI, or any feature listed in the brief's §"Out of scope". Even if implementation goes faster than the 15-hour budget, additional time goes to v1.0 polish (better errors, more validator coverage), never to v1.1+ features.

2. **Budget tripwires.**
   - 15-hour total implementation budget for v1.0
   - 90-day calendar tripwire from project start (2026-04-27) to v1.0.0 tag
   - If 15h hits with no end in sight: cut polish features, keep core. The transport is non-cuttable — it's the whole product.

3. **Header injection prevention is mandatory (NFR1, NFR2).** Submitter-controlled fields **must never** be interpolated as raw strings into email headers. The Postmark transport must use Postmark's structured JSON API fields. The test suite must include explicit injection-payload coverage (CRLF in name/email/subject, `\r\nBcc:`, header smuggling). This is non-negotiable — see Risk R4.

4. **API keys must never be logged (NFR3).** Set them as HTTP headers during request construction, never log them in error or debug output. Tests must verify by triggering transport failures and asserting the key string does not appear in captured log output.

5. **Origin/Referer fail-closed (FR4, NFR4).** When `allowed_origins` is configured and both `Origin` and `Referer` headers are missing, reject the request. When `allowed_origins` is empty (explicitly `[]`, not unset), the parser must reject the configuration — no fail-open default for an empty allowlist.

6. **Every FR/NFR traces back to the brief.** If you find yourself writing something not traceable to a spec requirement, stop and check the spec rather than improvising.

7. **Follow the architecture doc's file layout exactly.** Flat package, file names match [`spec/03-architecture.md`](./spec/03-architecture.md) §"File layout". No sub-packages in v1.0.

## Architectural decisions worth knowing

Five ADRs are recorded in [`spec/03-architecture.md`](./spec/03-architecture.md) §"Architectural decisions log". The most likely to come up during implementation:

- **ADR-1:** No third-party Postmark SDK. ~80 lines of bespoke HTTP client.
- **ADR-3:** Hand-rolled token-bucket rate limiter, not `golang.org/x/time/rate`. We need LRU eviction at 10K IPs (NFR6) which `x/time/rate` doesn't provide.
- **ADR-4:** `*bool` (pointer) for `LogFailedSubmissions` to distinguish "operator omitted" from "operator explicitly set false."
- **ADR-5:** Synchronous send with one in-request retry. v2 brings async + SQLite together.

If you find yourself wanting to deviate from any ADR, update the architecture doc with the new decision and rationale before changing code.

## When in doubt

1. Re-read the relevant spec section
2. If the spec is silent or ambiguous, the architecture doc's open questions section ([`spec/03-architecture.md`](./spec/03-architecture.md) §"Open architectural questions") may already have a recommended answer
3. If neither helps, ask the author before improvising

The cost of asking is small. The cost of building the wrong thing is the entire 15-hour budget.
