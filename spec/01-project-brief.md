---
title: "Project Brief: Posthorn v1"
status: locked
created: 2026-04-27
synced_from_obsidian: 2026-04-27
---

# Project Brief: Posthorn v1

This document captures the locked decisions for Posthorn v1.0. It is the upstream artifact for [the PRD](./02-prd.md) and [the architecture document](./03-architecture.md). Changes here require revisiting both downstream documents.

The project's Obsidian dashboard retains the broader project roadmap and v2/v3 scope context (not included in this repository). This brief is scoped strictly to v1.

## Executive Summary

Posthorn is the **unified outbound mail layer for self-hosted projects**. Nobody wants to run a mail server in 2026 — self-hosted operators use Postmark, Resend, Mailgun, or AWS SES for the deliverability + bounce-handling reasons that have always favored managed providers. But every app a self-hoster runs has to integrate with that provider independently, duplicating credentials, integration code, and operational concerns across the stack. Posthorn is the single gateway that all those apps point at.

It accepts mail through pluggable ingress modes (HTTP form submissions in v1.0; JSON API in v1.1; SMTP listener in v1.3) and delivers it via pluggable HTTP API transports (Postmark in v1.0; Resend / Mailgun / AWS SES / outbound-SMTP relay in v1.2). It runs as a standalone Go binary or Docker container behind whatever reverse proxy the operator already runs.

The cloud-blocks-SMTP problem is the canonical entry point — DigitalOcean, AWS Lightsail, Linode, and most cloud hosts block ports 25/465/587 outbound, breaking both web-form-to-email patterns and SMTP-emitting apps simultaneously. Posthorn solves that. But the broader value is the unified outbound layer pattern, which applies even when SMTP isn't blocked: self-hosters running multiple apps benefit from centralizing the outbound mail concern regardless of underlying infrastructure.

## Status log

- **2026-04-27** — Project named *caddy-formward* and scoped as a Caddy v2 HTTP handler module replacing the dead `SchumacherFM/mailout` plugin. Spec locked.
- **2026-04-27** — Scope expanded to email gateway (two ingress modes: HTTP form, SMTP listener) after analyzing the broader audience experiencing the same DigitalOcean SMTP-block pain. Project renamed to *Posthorn*. Repo renamed in place from `caddy-formward` to `posthorn`. Caddy module status changed from primary deliverable to optional adapter. Spec rewritten from scratch (this document); previous brief, PRD, and architecture documents replaced.
- **2026-05-15** — Positioning sharpened from "gateway for cloud platforms that block outbound SMTP" to "unified outbound mail layer for self-hosted projects" after triaging 10 incoming GitHub issues (most from the Pensum integration POV). The cloud-SMTP-block wedge is preserved as the canonical discovery entry point; the broader unified-layer framing is the durable value proposition. v1.x roadmap restructured (v1.1 API mode, v1.2 multi-transport + ops polish, v1.3 SMTP ingress) to support this positioning.
- **2026-05-15** — Caddy v2 adapter module cut from v1.0 pre-tag. Originally kept as a secondary deployment shape (carryover from the project's `caddy-formward` origin), the adapter increasingly diverged from the standalone (v1.3 SMTP ingress and v2 SQLite storage were both already standalone-only) and the two-shapes-per-feature carve-outs muddled the single-shape thesis. Caddy users keep first-class support via the reverse-proxy path. FR27–FR30 / NFR10 deleted from the PRD, ADR-6 and ADR-7 retired from the architecture doc, R3 modules-page submission dropped. The `core/gateway.Handler` interface is preserved so a community-maintained module against it remains possible without the project carrying the maintenance.
- **2026-05-16** — Batch send dropped from the v1.1 scope while drafting the v1.1 FR amendment. The 2026-05-15 roadmap restructure named batch as a v1.1 feature, but the named v1.1 audience (one operator, many projects — Pensum, Ghost, internal tools) has no concrete workload that needs N-message-per-call efficiency. The other three v1.1 features (API-key auth, JSON content type, idempotency keys) each map to a specific failure mode that today blocks the audience from using Posthorn at all; batch only solves a performance problem none of them has yet. Locking batch's design (rate-limit-per-message vs per-batch, partial-success semantics, idempotency scope, per-message logging shape) absent operator-driven evidence is exactly the kind of feature-count-first thinking the gateway-not-infrastructure principle exists to prevent. Reconsidered if a concrete operator surfaces with the need.
- **2026-05-16 (later same day)** — Per-request recipient override (`to_override` JSON field) added to v1.1 scope as FR46. Drafting the Cloudflare Worker recipe surfaced that the named v1.1 audience (Workers sending password resets, receipts, notifications) cannot pre-declare every possible recipient in config — each transactional event goes to a different end user. The original D5 design decision deferred all per-request overrides on blast-radius grounds; that was correct for `from` (per-request sender enables spoofing via leaked keys, irreversible reputation damage) but wrong for `to` (the operator's domain reputation isn't at stake when a leaked key sends *to* arbitrary addresses, and "one endpoint per recipient" doesn't scale to user-shaped recipients). `from` stays endpoint-configured; `to_override` accepts a JSON string or array of strings, each validated as a syntactic email (FR11 reuse). Rate limits, idempotency, and Postmark's own abuse detection remain the defenses against a leaked-key spam attack. New ADR-11 captures the safety reasoning.
- **2026-05-16 (end of day)** — v1.x scope collapsed into a single v1.0 release. After completing the v1.1 API mode amendment and shipping the Cloudflare Worker recipe, an evaluation of what each future milestone (v1.2 multi-transport + ops, v1.3 SMTP ingress, v2 platform) imports concluded that v1.2 and v1.3 belong in the initial public release while v2 stays deferred as the stateful-platform boundary. The v1.0/v1.1/v1.2/v1.3 split was a planning artifact — a sequencing line drawn during the 2026-04-27 brief and refined during the 2026-05-15 roadmap restructure. With v1.1's work complete, the brief's design principles (gateway-not-infrastructure, no-feature-count-competition, config-over-UI, bespoke-before-SDK) hold equally for everything through v1.3; v2's SQLite + async-queue architecture is the category change that justifies the deferral. The combined v1.0 scope is now a single XL effort (originally locked v1.0 plus blocks B/C/D); audience reception will inform whether v2 follows or whether the v1.x line is sufficient. The new FRs span FR47-FR68, new NFRs span NFR22-NFR24, new Epics 9-11. Audience reasoning: at first public release, "Posthorn 1.0 = unified outbound mail gateway with form ingress + API mode + idempotency + four transports + Prometheus + SMTP-listener" is a more credible and self-consistent positioning than "Posthorn 1.0 = contact-form gateway; API mode and multi-transport coming soon." Internal version sequencing was not load-bearing on the audience side.

## Design principles

Five principles that pin the shape of Posthorn across versions. These sit above the ADRs (in [the architecture doc](./03-architecture.md)) — the ADRs answer "why this technical choice"; these answer "what shape is Posthorn?" When a new feature, ADR, or roadmap item conflicts with one of these, the principle wins and the proposal needs revisiting.

### 1. Gateway, not infrastructure

Posthorn sits between an operator's apps and a transactional mail provider the operator already chose. It does not replace the provider. It does not run its own outbound SMTP fleet, manage its own IP reputation, or host mailboxes. Operators bring an API key from a provider; Posthorn handles the gateway logic.

This is why feature requests like "manage DKIM rotation," "publish DMARC reports," or "run an outbound MTA" are out of scope — those are mail-server / mail-platform features, and we don't run mail servers or mail platforms.

### 2. Integration layer, not mail-receiving layer

Posthorn unifies the **outbound** integration concern across an operator's stack. Many ingress shapes (HTTP forms today, JSON APIs in v1.1, SMTP from internal apps in v1.3) converge on a single transport surface that talks to the operator's provider. The integration layer is one-way: many inputs → one output.

Posthorn does not unify the **inbound** mail-receiving concern. Acting as the MX target for a domain, performing receive-side SPF/DKIM verification, doing inbound spam filtering, managing mailbox storage — these are mail-server responsibilities. When operators need them, the right answer is a mail server (Stalwart) or a hosted inbound provider (Postmark Inbound, Mailgun Routes, Cloudflare Email Workers) — not Posthorn.

### 3. No feature-count competition against category leaders

Three established categories of self-hosted email infrastructure exist (see Problem Statement §"Adjacent projects we deliberately don't compete with"). Posthorn occupies exactly one of them — the gateway slot. Operators who need a mail server should use Stalwart; operators who want to be their own transactional provider should use Postal; operators who need marketing email should use Listmonk. We do not match those projects on feature count in their lanes; that's a known-losing fight.

The corollary: if a feature request would push Posthorn deeper into one of those adjacent categories, the answer is "no, use the better tool for that," not "let's add it." This applies to webmail UIs, mailbox storage, IP reputation management, campaign dashboards, segmentation engines, and similar features that look reasonable in isolation but commit us to category fights we'd lose.

### 4. Config files over admin UIs

Posthorn is configured via a single TOML file. There is no runtime mutation surface that could drift from the config file. Reviewing a config diff is reviewing the system's behavior — there's no `posthorn admin` CLI, no settings page, no live-reload-this-endpoint operation that bypasses the file.

This is a deliberate trade. Admin UIs add ops complexity (auth, audit logging, state reconciliation) and create surface for the configured behavior to diverge from the documented behavior. The cost is real — operators editing TOML by hand instead of clicking a UI — and we pay it intentionally. v3+ may introduce a read-only UI for browsing submissions / logs; it will not be a configuration surface.

### 5. Bespoke before SDK, when the surface is small

For integrations with small, stable surfaces (Postmark's ~2 endpoints; SES's SigV4 + send call; Resend / Mailgun similarly), Posthorn writes the integration directly using stdlib + minimal dependencies. We don't pull in `aws-sdk-go-v2` for SES, `postmark-go` for Postmark, etc. The bias is toward bespoke for two reasons: a smaller dep tree (security audit surface, build complexity, version-pin maintenance) and a more auditable integration (every byte touching the upstream API is in our repo).

The rule of thumb: bespoke when the integration is ~200 lines or less; SDK when bespoke would be ~1000+. Posthorn's v1.0 dependency surface — TOML parser, LRU cache, UUID library, and the stdlib — is the floor this principle drives toward.

This is [ADR-1](./03-architecture.md#architectural-decisions-log) elevated to a project-wide principle, not just a Postmark decision.

## Problem Statement

### The gap

Cloud platforms — DigitalOcean, AWS Lightsail, Linode, and most others — block outbound SMTP on ports 25, 465, and 587 by default to fight spam. The block is not negotiable: a 2026-04 support exchange with DigitalOcean confirmed they will not unblock the port range, and explicitly recommended using an HTTP API service like Postmark, Resend, Mailgun, or AWS SES.

This breaks two common deployment patterns simultaneously:

1. **Web forms that send email.** Contact forms, signup confirmations, alert webhooks. Every static-site host or Caddy/nginx-fronted blog hits this on day one.
2. **Self-hosted apps that emit SMTP for transactional mail.** Ghost, Gitea, Mastodon, Discourse, Matrix, NextCloud, Authentik, et al. all generate authentication codes, magic links, and notifications via SMTP. Their config files assume an SMTP server they can reach.

The current options for either pattern are bad:

- **Move to a host that doesn't block SMTP** — Hetzner, OVH, some Vultr plans. Disruptive; sometimes not viable.
- **Pay for SaaS form services or transactional providers with HTTP-only APIs** — Formspree, Netlify Forms, etc. for forms; rewrite app config to use API SDKs (rarely supported).
- **Run Postfix as a relay configured to use HTTP API** — Postfix doesn't natively speak HTTP. Workable with custom milters or `smtp-transport` glue, but heavy and fragile.
- **Find a maintained SMTP-to-HTTP bridge** — there isn't one. The landscape is `bytemark/smtp` and similar postfix-based images that still need outbound SMTP somewhere; or dead/abandoned `smtp2http` projects.

The Caddy v1 `mailout` plugin filled half this gap (web-form-to-email) for Caddy users; it was never ported to v2 and never addressed the SMTP-emitting-app side. (Posthorn started life as a v2 successor to `mailout`; that genealogy is preserved in the status log above.)

There is no actively maintained, self-hosted, HTTP-API-first email gateway in 2026.

### Adjacent projects we deliberately don't compete with

Three categories of self-hosted email infrastructure exist in 2026. Posthorn occupies exactly one of them.

| Category | What it does | Leaders | Posthorn |
|---|---|---|---|
| Modern mail server | SMTP + IMAP/JMAP + storage + webmail. Replaces Gmail/Workspace as a destination mailbox. | Stalwart, Mailcow, iRedMail | Out of scope — would never match feature count |
| Self-hosted outbound delivery platform | Mailgun/SendGrid clone. Runs its own outbound SMTP fleet, manages IP reputation, ships a campaign dashboard. | Postal, Hyvor Relay | Out of scope — different audience, different shape |
| **Email gateway** | Sits between an operator's apps and an external transactional provider the operator already chose. Owns the integration layer, not the mailbox or the delivery infrastructure. | No clear leader | **Target slot** |

The first two categories have established projects with multi-year head starts. Posthorn does not try to match them on feature count in their lanes — that's a known-losing fight. Operators who need a real mail server should use Stalwart; operators who want to be their own transactional mail provider should use Postal. Posthorn is for the operator who has already picked a transactional provider (Postmark, Resend, Mailgun, SES) and wants one gateway in front of it that all their apps point at.

This positioning explicitly avoids the "Mailgun killer" framing — Postal owns that lane and would win head-to-head on feature count and reputation. Posthorn is a different shape: it doesn't replace the provider, it unifies the operator's integration with the provider.

### The deployment reality

Indie/homelab self-hosters running on cloud platforms experience this as two separate problems that share a root cause. They typically solve each independently:

- The blog contact form goes to Formspree (paid SaaS).
- The Ghost admin emails get debugged for hours, then routed through a friend's mail server, or the operator gives up and accepts that admin login is broken.

Posthorn lets them solve both with one tool, one Postmark account, one DNS configuration, and one set of logs.

### The personal use case

The author's blog (craigmccaskill.com) currently uses Formspree for its contact form and is partially broken on Ghost admin login because outbound SMTP is blocked on its DigitalOcean droplet. Both problems exist on the same host, with the same Postmark account standing ready as the upstream API. Posthorn replaces Formspree (v1.0 ship) and unblocks Ghost (v1.x SMTP ingress ship).

## Proposed Solution

A standalone Go binary (`posthorn`), distributed primarily as a Docker image, that:

1. **Accepts mail through pluggable ingress modes.** v1.0 ships with HTTP form ingress (form-encoded POST submissions on configured paths). v1.x adds SMTP ingress (TCP listener accepting SMTP from clients on the local docker network).
2. **Delivers mail through pluggable HTTP API transports.** v1.0 ships with Postmark; v1.2 adds Resend, Mailgun, AWS SES, and outbound-SMTP-relay (for operators on hosts that don't block it). Same `Transport` interface across ingress modes.
3. **Provides shared operational features** across all ingress/transport pairs: structured logging, rate limiting, retry policy, observability, secrets via env-var resolution.
4. **Is configured via a single TOML file**, with `${env.VAR}` placeholders for secrets.
5. **Runs behind whatever reverse proxy the operator already uses** — Caddy, nginx, Traefik, Cloudflare. Posthorn does not terminate TLS; it expects to sit behind a proxy that does.

The architecture is deliberately ingress-agnostic and transport-agnostic. The reusable middle layer — Message, Transport, retry policy, structured logging — does not care whether a Message arrived via HTTP form parser or SMTP MIME parser, nor whether it leaves via Postmark JSON API or Mailgun multipart API.

## Target Users

### Primary (v1.0)

**Indie developers and homelab operators self-hosting one or more web services on cloud infrastructure that blocks outbound SMTP.** They pay $5-20/month for a DigitalOcean or Vultr droplet, run two to ten services in Docker Compose, and want one tool to handle both their contact form and their apps' transactional email. They will not migrate from Formspree (or accept broken admin login) unless setup takes ten minutes or less.

This is the author. The Ghost dogfooding case covers both ingress modes once v1.x SMTP ingress ships.

### Secondary, emerging (v1.1+)

**Indie developers building multi-project stacks who want one centralized outbound mail gateway across all their apps.** Concrete example: a single operator running a blog (Ghost), a SaaS project on Cloudflare Workers (Pensum), and a couple of internal tools. Each has its own outbound mail need; without Posthorn, each needs its own Postmark integration. With Posthorn (v1.1 API mode + v1.0 form ingress), all of them point at one Posthorn instance — one set of credentials, one set of logs, one set of bounce-handling decisions.

This audience is shape-distinct from the original "homelab operator with a contact form" audience — they care more about the API-mode shape (Cloudflare Workers calling Posthorn server-to-server with `Authorization: Bearer`) than the form-ingress shape. They were not anticipated by the original v1.0 spec; their requirements drove the v1.1 reshuffle on 2026-05-15.

### Future audiences (post-v1, named to constrain architecture)

- **Self-hosted-app operators with stacks behind any reverse proxy** — nginx, Traefik, Cloudflare, HAProxy. The reverse-proxy-agnostic standalone serves them without forks.
- **Small agencies and freelancers deploying for clients.** Need template scalability, per-tenant config isolation, file attachments, observability that flows into their existing log pipelines. Out of scope for v1.0 — but the Transport interface and config loader must grow into per-tenant use without a rewrite.

## Goals and Success Metrics

### Done — when is v1 ready to ship?

All four must pass:

1. The author's blog runs Posthorn for the contact form for 30 days with zero dropped submissions, confirmed via Postmark logs.
2. README has a copy-pasteable Docker Compose example verified end-to-end on a clean install.
3. Tagged v1.0.0 release on GitHub with a published Docker image at `ghcr.io/craigmccaskill/posthorn:v1.0.0` and `:latest`.

### Worked — did v1 actually achieve anything?

Two binary signals:

- **At least one external user runs Posthorn in production within 90 days of release** (signal: an issue or discussion thread that's not the author).
- **The Ghost admin login problem is solved by the v1.x SMTP ingress within 6 months of v1.0 release.** This is the canonical end-to-end validation that the architecture is right.

GitHub stars, blog traffic, HN front page are noise relative to these. A real second user and a fully-dogfooded round trip are the binary tests of "this product solves a real problem."

## MVP Scope (v1.0)

### In scope

**Ingress**
- HTTP form ingress: form-encoded POST submissions on configured paths
- Multi-endpoint support: multiple form configurations, each independent

**Egress (transports)**
- Postmark HTTP API transport (only transport in v1.0)
- Pluggable Transport interface ready for v1.2 additions

**Spam protection**
- Honeypot field (configurable name)
- Origin/Referer check, fails closed when both headers missing if `allowed_origins` is configured
- Token bucket rate limiter per endpoint, with `trusted_proxies` config for X-Forwarded-For handling
- Maximum request body size limit

**Validation**
- Required fields list with per-field error responses (HTTP 422)
- Email format validation on the submitter's email field

**Email features**
- Go template rendering for subject and body, with form fields as template data
- Custom fields passthrough — fields not named in config appear in a structured block at the bottom of the email

**Response handling**
- JSON API responses with appropriate HTTP status codes (200, 422, 429, 400, 502)
- Content negotiation via `Accept` header (JSON for fetch, redirect for plain forms)
- `redirect_success` and `redirect_error` URLs

**Deployment shape**
- Standalone Go binary (`posthorn serve --config config.toml`)
- Docker image at `ghcr.io/craigmccaskill/posthorn` with multi-arch support (amd64, arm64)
- Runs behind any reverse proxy that handles TLS (Caddy, nginx, Traefik, Cloudflare, etc.)
- TOML config file with `${env.VAR}` placeholders for secrets

**Operational**
- Structured (JSON) logging for all events: submissions, sends, failures, spam blocks, rate limits
- Configurable `log_failed_submissions` flag (default `true`) for terminal-failure recovery
- Graceful shutdown on SIGTERM/SIGINT with in-flight request drain

**Failure handling**
- Synchronous send with one retry on transient errors (network/5xx)
- 429 handling with longer backoff (5s)
- Fail fast on 4xx config errors
- Hard 10s request timeout including retries
- On terminal failure, log full submission payload at ERROR level (configurable) and return HTTP 502

**Security NFR**
- Submitter-controlled fields must never be interpolated into email headers as raw strings; must pass through transport library APIs as structured data
- Test coverage required against header-injection payloads (CRLF in email, name, subject; BCC injection attempts)
- API keys never appear in log output

### Out of scope (v1.0)

- **SMTP ingress** — v1.3 (the strategic post-v1.0 feature; see Post-MVP Vision)
- API-key auth per endpoint, JSON content type, batch send, idempotency keys — v1.1
- SMTP outbound transport, Resend, Mailgun, SES HTTP API transports — v1.2
- CSRF / time-based token spam protection — v1.2
- Prometheus metrics, health check endpoint, dry run mode, IP stripping, trusted_proxies presets — v1.2
- Webhook transport, lifecycle event callbacks — v2
- Suppression list, durable idempotency, automatic unsubscribe injection — v2
- SQLite submission log, retry queue across restarts — v2
- File attachments, multi-output fan-out — v2
- HTML body, markdown body, confirmation auto-replies — v2 / post-v1
- Per-tenant config isolation, multi-config deployments — post-v1
- Captcha, proof-of-work, admin UI, PGP encryption — v3
- **Inbound mail parsing (MX-target reception + IMAP polling + MIME → JSON webhook delivery)** — **deliberately deferred to v3+**. Would complete the "bidirectional gateway for apps and agents" framing some reviewers suggest, but materially expands scope: different threat model (anyone can send mail to MX; requires spam handling, abuse policy, possibly Rspamd integration), no concrete user surfacing it (Pensum is send-only), and no agent-shaped consumer that would need it as a precondition. Reconsidered if a second concrete user surfaces with the need.

## Post-MVP Vision

The v1.x roadmap was restructured 2026-05-15, then collapsed into a single v1.0 release 2026-05-16 (see status log). The combined v1.0 scope is recorded below as three feature blocks — they're sequenced as Epics within the single v1.0 release rather than as separate version milestones. The canonical user-facing roadmap lives at [posthorn.dev/roadmap/](https://posthorn.dev/roadmap/); this section is the authoritative scope source it derives from.

**v1.0 feature block A — Form ingress** (originally locked v1.0 scope). HTTP form-encoded ingress with honeypot, Origin/Referer fail-closed, rate limiting, retry policy, structured logging. Postmark transport. Standalone binary + Docker image. Epics 1-7, FR1-FR26, NFR1-NFR18.

**v1.0 feature block B — API mode** (originally v1.1; folded into v1.0 on 2026-05-16). Posthorn becomes usable as an internal mail API. Server-to-server callers (workers, daemons, paid-event handlers) hit Posthorn without browser-shaped defenses.
- API-key auth per endpoint (`auth = "api-key"` mode, mutex with form-mode defenses)
- JSON content type on API-mode endpoints
- Idempotency keys via standard `Idempotency-Key` header (24h TTL, in-memory; durable across restarts in v2)
- Per-request `to_override` (string or array) — see ADR-11 for the safety asymmetry vs `from`

Epic 8, FR31-FR46, NFR19-NFR21.

**v1.0 feature block C — Multi-transport + operational maturity** (originally v1.2; folded into v1.0 on 2026-05-16). Posthorn isn't Postmark-locked, and it's production-ready.
- Resend, Mailgun, AWS SES, outbound-SMTP transports
- CSRF tokens for form-mode (HMAC-issued by operator at form-render time; see ADR-16)
- `/healthz` endpoint, `/metrics` (hand-rolled Prometheus exposition — see ADR-15)
- Dry-run mode (full pipeline minus `transport.Send`)
- Named presets for `trusted_proxies` (`cloudflare`, etc.) on top of CIDR-list syntax
- IP-stripping option for GDPR contexts

Epics 9-10, FR47-FR59.

**v1.0 feature block D — SMTP ingress** (originally v1.3; folded into v1.0 on 2026-05-16). Completes the gateway thesis. TCP listener accepts SMTP from internal clients, forwards via the configured HTTP API transport. Unblocks the Ghost admin login use case.
- TCP listener with SMTP AUTH (PLAIN/LOGIN) and optional client-cert auth (see ADR-12 for `Ingress` interface design)
- Open-relay prevention via sender allowlist + recipient allowlist or count cap
- Size and TLS enforcement
- New `Ingress` interface; existing HTTP handlers retrofitted onto it
- New `[smtp_listener]` top-level config section
- One outbound transport per SMTP listener; multi-tenant routing is v2 territory (see ADR-13)

Epic 11, FR60-FR68, NFR22-NFR24.

**v2 — platform maturity.** The architectural shift that unlocks operating Posthorn as a real mail platform.
- SQLite submission log + persistent retry queue across restarts
- Suppression list (auto on hard bounces and spam complaints), durable idempotency (replaces v1.1 in-memory), Postmark lifecycle event callbacks forwarded to caller via HMAC-signed webhook, automatic unsubscribe link injection (RFC 8058 one-click)
- HTML body support (alongside the planned markdown body)
- File attachments (multipart uploads forwarded to transport)
- Multiple outputs per endpoint (email + webhook + log fan-out)

**v3** is speculative and depends on community traction:
- Admin UI (embedded web app, requires SQLite storage)
- Proof-of-work spam challenge (defeats botnet spam that per-IP rate limiting can't catch)
- PGP encryption

### Deliberately not on the roadmap (revisit on concrete operator pain)

Features that have been considered and explicitly removed pending a real workload that demands them. Distinct from the version-tagged scope above — these have *no* target version, and won't get one until an operator surfaces with the use case.

- **Batch send API** (`{"messages": [...]}` body with per-message status response, mapping to provider batch endpoints like Postmark's `/email/batch`). Drafted as v1.1 scope during the 2026-05-15 roadmap restructure; removed 2026-05-16 while drafting the v1.1 FRs. Reasoning: the named v1.1 audience sends 1 message per task (password resets, welcome flows, notifications). Adding a batch shape requires locking design decisions — rate-limit charging (per-message vs per-batch), partial-success semantics, idempotency scope (whole batch vs each message), per-message log fan-out — without operator-driven evidence to ground them. The gateway-not-infrastructure principle (a Posthorn batch endpoint with its own scheduling/queuing/replay logic pulls toward infrastructure shape) and the no-feature-count-competition principle (Postmark has `/email/batch`, so we should too is exactly the framing the principle exists to reject) both argue for waiting. Trigger to reconsider: a real operator workload sending >50 messages per task where round-trip overhead is the binding constraint, with enough usage detail to drive the design.

## Technical Constraints (locked)

| Constraint | Value |
|---|---|
| Language | Go 1.25+ |
| License | Apache-2.0 |
| Distribution | GitHub releases (binary), Docker image at GHCR, `go install` |
| Repo | github.com/craigmccaskill/posthorn |
| Go module | github.com/craigmccaskill/posthorn |
| Config format | TOML |
| Build tooling | `go build` |
| Config syntax stability | Stable within a major version after v1.0.0 |
| Go API stability | Not guaranteed; subject to refactor |

## Threat Model

### In scope for v1.0 (HTTP form ingress)

| # | Threat | v1.0 defense |
|---|---|---|
| 1 | Drive-by scraper bots | Honeypot field |
| 2 | Direct-POST bots that skip the form page | Origin/Referer check, fails closed |
| 3 | Basic targeted abuse | Token bucket rate limit (proxy-aware via `trusted_proxies`) |
| 4 | Postmark quota burn | Rate limit + max request body size |
| 5 | Email header injection | Structured-data transport API + explicit injection-payload test coverage |
| 6 | API key theft from logs/error output | Keys never logged; explicit test coverage |

### Out of scope for v1.0

- **SMTP-ingress threats** (open relay, MX spoofing, RCPT bombing, recipient enumeration) — defended in v1.3 when SMTP ingress ships. Architecture must not foreclose those defenses.
- Botnet spam from many low-rate IPs — v3 (captcha or proof-of-work)
- DDoS / Layer 7 attacks — CDN's responsibility, not the gateway's
- API key theft from misconfigured deployment — operator concern, addressed via documentation

### Outbound abuse posture

Because Posthorn relays through an external transactional provider (Postmark, Resend, Mailgun, SES), **outbound IP reputation management is the provider's concern, not Posthorn's**. The provider controls the sending IPs, enforces sender quotas, monitors complaint rates, and suspends abusive accounts. Posthorn never operates as the outbound MTA itself in v1.x.

Posthorn's role in the outbound abuse chain is narrower:

| Mechanism | What it does | Why Posthorn handles it |
|---|---|---|
| Token-bucket rate limit (FR8) | Bounds per-endpoint, per-IP submission volume | Prevents one compromised caller from draining a Postmark quota before the provider's own throttle kicks in |
| Max body size cap (FR7) | Bounds individual message size | Protects Posthorn process memory; secondary defense against payload-volume attacks |
| Suppression list (v2, #4) | Refuses to send to known-bouncing addresses | Prevents repeated sends that harm sender reputation at the provider level |
| Structured logs (FR17, NFR7) | Every outbound decision logged with submission_id, endpoint, transport, payload | Operator-side forensics for identifying abuse patterns |

Posthorn does **not**:

- Manage its own sender reputation (no IPs it controls)
- Throttle based on global reputation signals (the provider's job)
- Implement content-based abuse detection or spam classification (the provider's spam systems handle this; Posthorn is a thin relay, not a filter)
- Coordinate quota across multiple providers (one provider per endpoint; per-endpoint quotas independent)

This is a deliberate posture. Operators who need stronger guarantees — running their own outbound infrastructure with reputation management, or operating at scale where provider quotas matter — should use Postal, not Posthorn. Posthorn's value is in the gateway abstraction, not in being the outbound MTA.

## Constraints and Assumptions

**Constraints:**
- Single author, working part-time
- 25-hour total budget for v1.0 implementation (vs 15h in the prior `caddy-formward` brief; expanded to cover the standalone-binary plumbing)
- 90-day calendar tripwire from project rename (2026-04-27 → 2026-07-26) to v1.0.0 tag
- All testing must be possible with infrastructure the author already has (Postmark account; no SMTP server required for v1.0)

**Assumptions:**
- Postmark API and free-tier availability remain unchanged in pricing structure
- The `posthorn` repo name and Go module path remain unclaimed (verified 2026-04-27)
- Docker Hub `craigmccaskill/posthorn` namespace remains unclaimed by anyone other than author

## Risks

| ID | Risk | Likelihood | Impact | Mitigation |
|----|------|------------|--------|------------|
| R1 | Solo maintainer abandonment | Medium | High | Time-bound commitment statement in README; 90-day shipping tripwire — if v1.0 isn't shipped within 90 days of project rename, scope cuts further (start with polish, then optional features; the core gateway is non-cuttable). |
| R2 | Effort blowup beyond 25h budget | High | High | Hard tripwire at 25h. Cut order: polish (better errors, validator depth) first, then optional features. Core gateway is non-cuttable. |
| R3 | Discoverability failure | Medium | High | Multi-channel: Docker Hub README and topics for the container; launch blog post documenting the Ghost-on-DO end-to-end story; submit to Hacker News once v1.2 SMTP ingress is dogfooded. |
| R4 | Header injection vulnerability ships in v1.0.0 | Medium without explicit testing; low with | Very high | Explicit injection-payload test cases as a PRD requirement; use Postmark JSON API exclusively |
| R5 | Email deliverability rabbit hole on launch day | Medium | Medium | Pre-launch DNS verification checklist (SPF, DKIM, DMARC); document DNS requirements in README |
| R6 | Postmark API or pricing change mid-development | Low | High | Acknowledged. Resend/Mailgun transports in v1.1 are the natural backstops. |
| R7 | Identity confusion with PostHog (analytics) | Low | Low | One-line README disambiguation. Different category, no functional overlap. |
| R8 | Scope creep into "real MTA" territory | Medium | High | Out-of-scope list is explicit. v1.x SMTP ingress is *not* an MTA — it's an authenticated relay accepting mail from known internal clients only. No MX records, no IMAP, no mailbox storage. Architecture must not foreclose this discipline. |

## Open Questions

None remaining at the brief level. Implementation-level questions belong in [the PRD](./02-prd.md). Architecture-level open questions belong in [the architecture document](./03-architecture.md).

## Appendices

### A. Why "Posthorn"

A posthorn is the small brass horn that post riders carried from the 16th century onward to announce the arrival or departure of mail. The symbol persists today on the logos of Deutsche Post, Czech Post, Austrian Post, and several other European postal services. The European General Court has ruled that the symbol is not exclusive to any single postal operator, so the name carries postal heritage without trademark entanglement.

The name was selected over "caddy-formward" (the project's prior identity, which framed it as a Caddy-bound form handler) when scope expanded to a general email gateway. Other candidates considered and rejected: Postern (collision with Android proxy app), Wicket (Apache Wicket framework), Hatch (PyPA Hatch), Frank (Frank!Framework — uncomfortably close in messaging-integration space), Pony (PonyORM + Pony language), Postino (multiple email-related GitHub projects), Herald (multiple email-related projects), Wren (Wren programming language). Posthorn was the only candidate with a clean software namespace and direct postal heritage.

The closest collision is PostHog (open-source product analytics), which is similar visually and phonetically. Different category (analytics vs email gateway); no functional overlap. README opens with a one-line disambiguation. Acknowledged as a known yellow flag, not a red one.

### B. References

- DigitalOcean SMTP block policy (2026-04 support exchange): outbound 25/465/587 blocked, no exceptions, recommends HTTP API
- Postmark API: https://postmarkapp.com/developer
- Original v1 Caddy mailout plugin (dead): https://github.com/SchumacherFM/mailout (genealogical only; Posthorn started as a v2 successor before scope expanded)
- Posthorn historical context: https://en.wikipedia.org/wiki/Post_horn

### C. Related project documents

- Posthorn Dashboard (in author's Obsidian vault) — project roadmap, v2/v3 scope context, post-v1 audience research
- Ghost Migration project notes (in author's Obsidian vault) — immediate consumer of v1.x SMTP ingress
