# CLAUDE.md — Posthorn

Auto-loaded by Claude Code at session start. Captures the durable project context, current status, and the guardrails that need to be in front of every code change. Build/test commands, commit conventions, and contributor scope policy live in [CONTRIBUTING.md](./CONTRIBUTING.md) — read both at the start of any code session.

## Project context

Posthorn is the **unified outbound mail layer for self-hosted projects** — the gateway between an operator's apps and a transactional mail provider they already chose (Postmark, Resend, Mailgun, AWS SES, outbound SMTP). v1.0 ships HTTP form ingress + Postmark transport; v1.x grows to JSON API ingress (v1.1), multi-transport (v1.2), SMTP ingress for self-hosted-apps-that-emit-SMTP (v1.3), and platform features in v2 (suppression, lifecycle, attachments, durable retry).

The original wedge — cloud-blocks-SMTP — is preserved as the canonical discovery entry point. The broader value is the unified-layer pattern (the same outbound concern duplicated across N self-hosted apps now centralizes through one Posthorn gateway), which applies even where SMTP is unblocked.

The full project history (initial scope as a Caddy form handler called `caddy-formward`, the 2026-04-27 scope expansion, the 2026-05-15 positioning sharpening) is in [`spec/01-project-brief.md`](./spec/01-project-brief.md) §"Status log". Don't re-derive — read the spec.

## Design principles (short)

These pin the shape of Posthorn across versions and override new feature requests that conflict with them. Full reasoning in [`spec/01-project-brief.md`](./spec/01-project-brief.md) §"Design principles".

1. **Gateway, not infrastructure.** Posthorn sits between apps and a provider — it doesn't replace the provider, doesn't run its own outbound SMTP, doesn't manage IP reputation, doesn't host mailboxes.
2. **Integration layer, not mail-receiving layer.** Posthorn unifies outbound (many ingress shapes → one transport surface). It does not unify inbound (MX hosting / receive-side filtering / mailbox storage); those are mail-server concerns.
3. **No feature-count competition with category leaders.** Stalwart owns mail-server territory; Postal owns outbound-platform territory; Listmonk owns marketing. We don't try to match them on feature count in their lanes.
4. **Config files over admin UIs.** A single TOML file is the source of truth. No runtime mutation surface that could drift from the config file. v3+ may add read-only UIs for browsing logs; not for configuration.
5. **Bespoke before SDK, when the surface is small.** Postmark / Resend / Mailgun / SES integrations are written directly (stdlib + minimal deps), not via vendor SDKs. The rule of thumb: bespoke when ~200 lines suffices; SDK when bespoke would be 1000+. [ADR-1](./spec/03-architecture.md#architectural-decisions-log) elevated project-wide.

When a feature request or implementation proposal conflicts with one of these, the principle wins. Take it back to spec discussion before changing code.

## Status (as of 2026-05-15)

**Phase:** v1.0 release prep. All implementation work landed; tag pending operator validation.

**Repo state:** Single Go module at `core/` with 9 packages plus `cmd/posthorn/`. Public docs site at [posthorn.dev](https://posthorn.dev) (Astro + Starlight, deployed via GH Pages from `site/`).

The full request pipeline is wired end-to-end: body cap → method → content-type → origin → rate limit → parse → honeypot → required fields → email format → template render → transport send (with FR19-22 retry under 10s hard timeout) → JSON 200 or 502. Every decision point logs structured JSON with a per-request UUID submission_id. CI runs `go vet` + `go test -race` on every push.

**Completed stories (20 of 21):**
- ✅ Epic 1 (Stories 1.1-1.3) — rename, code reorganization into `core/`
- ✅ Epic 2 (Stories 2.1-2.5) — TOML config, HTTP handler, validation, templating, cmd/posthorn
- ✅ Epic 3 (Stories 3.1-3.2) — spam protection, rate limiting
- ✅ Epic 4 (Stories 4.1-4.2) — retry policy, structured JSON logging
- ✅ Epic 5 (Stories 5.1-5.3) — multi-stage Dockerfile, CI workflow, multi-arch release workflow (validated end-to-end via `v0.0.1-test` tag → `ghcr.io/craigmccaskill/posthorn:0.0.1-test` multi-arch publish)
- ❌ Epic 6 (Caddy adapter) — **retired 2026-05-15.** Originally implemented as Stories 6.1–6.3 (adapter module, Caddyfile unmarshaler with parity test, manual parity test), the entire adapter was cut from v1.0 pre-tag for the product reasons in the brief's status log.
- ✅ Epic 7 Stories 7.1-7.2 — README rewrite, OSS hygiene files (CONTRIBUTING, SECURITY, CODE_OF_CONDUCT, CHANGELOG, PR + issue templates)

**Remaining story (1 of 21):**
- ⏳ Epic 7 Story 7.3 — tag v1.0.0, verify GHCR publish. **Gated on operator validation:** Docker smoke test, end-to-end manual test ([docs/manual-test.md](./docs/manual-test.md)).

**Current task:** Operator validation pass scheduled 2026-05-16/17. See [docs/release-checklist.md](./docs/release-checklist.md) for the tag-day procedure.

**Budget:** ~14.5h burned of 25h v1.0 budget. Site work (~6h, off-budget) was launch infrastructure. Comfortable margin remaining for any validation rework.

After each story ships, update this "Current story" pointer.

**Architecture deviations from original spec:**
- `core/http/` → `core/gateway/` (package `gateway`) to avoid shadowing stdlib `net/http`. Architecture and PRD updated.
- Retry timing constants (`requestTimeout`, `transientRetryDelay`, `rateLimitedRetryDelay`) declared as package vars, not consts, so tests can override via the test-only helper `gateway.SetRetryDelaysForTest` (in `core/gateway/export_test.go`). Production never mutates them.
- [`site/`](./site/) Astro + Starlight directory added at repo root for the posthorn.dev marketing/docs site (2026-05-14). Not in original v1.0 spec scope — treated as launch infrastructure outside the 25h budget. Deploys to GitHub Pages via [`.github/workflows/site-deploy.yml`](./.github/workflows/site-deploy.yml). Custom domain in [`site/public/CNAME`](./site/public/CNAME). Build: `cd site && npm ci && npm run build`. Sidebar config and theming live in [`site/astro.config.mjs`](./site/astro.config.mjs).
- **2026-05-15: Caddy v2 adapter module cut.** Originally Epic 6 (Stories 6.1–6.3) shipped a `caddy/` sibling Go module providing `http.handlers.posthorn`. On tag eve, the adapter was cut after a product-level conversation about single-shape simplicity (see brief's status log). The `caddy/` directory, `go.work` file, parity test, and manual parity procedure were removed. FR27–FR30 and NFR10 were deleted from the PRD; ADR-6 and ADR-7 were retired in-place in the architecture doc.

**Deps added during implementation:** `github.com/BurntSushi/toml` (config), `github.com/hashicorp/golang-lru/v2` (rate limiter), `github.com/google/uuid` (submission IDs). All three were named in the architecture doc's allowed-deps list.

## Read the spec before touching code

The v1.0 specification is locked across three documents in [`spec/`](./spec/):

1. **[`spec/01-project-brief.md`](./spec/01-project-brief.md)** — problem, users, scope, success metrics, threat model, risks, constraints
2. **[`spec/02-prd.md`](./spec/02-prd.md)** — functional and non-functional requirements (FR27–FR30 / NFR10 are deleted slots from the 2026-05-15 Caddy adapter cut), 7-epic breakdown with stories and acceptance criteria
3. **[`spec/03-architecture.md`](./spec/03-architecture.md)** — file layout, lifecycle, request flow, component design, threat-to-defense mapping, dependencies, ADRs (ADR-6 and ADR-7 retired in-place from the adapter cut), forward-compatibility commitments for v1.x

The PRD has the canonical FR/NFR list with "must"-level requirements; the architecture doc has the implementation blueprint, including the target file layout under §"File layout".

## Hard guardrails

These derive from the locked spec. Do not violate without an explicit conversation that updates the spec first.

1. **Scope is v1.0 only.** Do not implement SMTP ingress, additional transports beyond Postmark, CSRF tokens, file attachments, webhook transport, SQLite storage, admin UI, or any feature listed in the brief's §"Out of scope". Even if implementation goes faster than the 25-hour budget, additional time goes to v1.0 polish (better errors, more validator coverage), never to v1.1+ features.

2. **Budget tripwires.**
   - 25-hour total implementation budget for v1.0
   - 90-day calendar tripwire from project rename (2026-04-27 → 2026-07-26) to v1.0.0 tag
   - If 25h hits with no end in sight: cut polish features first (better errors, deeper validator coverage). The standalone gateway core is non-cuttable — it's the whole product.

3. **Header injection prevention is mandatory (NFR1, NFR2).** Submitter-controlled fields **must never** be interpolated as raw strings into email headers. The Postmark transport must use Postmark's structured JSON API fields. The test suite must include explicit injection-payload coverage (CRLF in name/email/subject, `\r\nBcc:`, header smuggling). This is non-negotiable — see Risk R4.

4. **API keys must never be logged (NFR3).** Set them as HTTP headers during request construction, never log them in error or debug output. Tests must verify by triggering transport failures and asserting the key string does not appear in captured log output.

5. **Origin/Referer fail-closed (FR6, NFR4).** When `allowed_origins` is configured and both `Origin` and `Referer` headers are missing, reject the request. When `allowed_origins` is configured as an explicitly empty list, the parser must reject the configuration — no fail-open default for an explicitly empty allowlist.

6. **Every active FR/NFR traces back to the brief.** If you find yourself writing something not traceable to a spec requirement, stop and check the spec rather than improvising. (FR27–FR30, NFR10, NFR14 are intentionally deleted slots from the 2026-05-15 Caddy adapter cut; don't resurrect them.)

7. **Follow the architecture doc's file layout exactly.** Single Go module at `core/` with internal sub-packages (per the layout in [`spec/03-architecture.md`](./spec/03-architecture.md) §"File layout"). No second module; no Caddy adapter.

8. **Don't reintroduce the Caddy adapter.** The adapter was cut on 2026-05-15 after a deliberate product-level conversation about single-shape simplicity. If a future request asks to bring it back, treat it as scope discussion, not implementation — re-open the conversation in the brief's status log before any code change.

## Architectural decisions worth knowing

ADRs are recorded in [`spec/03-architecture.md`](./spec/03-architecture.md) §"Architectural decisions log". The most likely to come up during implementation:

- **ADR-1:** No third-party Postmark SDK. ~80 lines of bespoke HTTP client.
- **ADR-2 (revised twice):** Single Go module at `core/` with internal sub-packages. Started as a flat package (`caddy-formward`), briefly became a two-module workspace (when the Caddy adapter was in scope), collapsed back to single-module on 2026-05-15 when the adapter was cut.
- **ADR-3:** Hand-rolled token-bucket rate limiter, not `golang.org/x/time/rate`. We need LRU eviction at 10K IPs (NFR6) which `x/time/rate` doesn't provide.
- **ADR-6 (retired 2026-05-15):** Originally the zero-Caddy-dep invariant; trivially true after the adapter cut.
- **ADR-7 (retired 2026-05-15):** Originally framed standalone as primary, adapter as optional; no longer meaningful (standalone is the only shape).

If you find yourself wanting to deviate from an active ADR, update the architecture doc with the new decision and rationale before changing code.

## When in doubt

1. Re-read the relevant spec section
2. If the spec is silent or ambiguous, the architecture doc's open questions section ([`spec/03-architecture.md`](./spec/03-architecture.md) §"Open architectural questions") may already have a recommended answer
3. If neither helps, ask the author before improvising

The cost of asking is small. The cost of building the wrong thing is the entire 25-hour budget.
