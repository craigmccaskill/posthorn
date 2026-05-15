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

It accepts mail through pluggable ingress modes (HTTP form submissions in v1.0; JSON API in v1.1; SMTP listener in v1.3) and delivers it via pluggable HTTP API transports (Postmark in v1.0; Resend / Mailgun / AWS SES / outbound-SMTP relay in v1.2). It runs as a standalone Go binary or Docker container, with an optional Caddy module adapter for operators who already run Caddy.

The cloud-blocks-SMTP problem is the canonical entry point — DigitalOcean, AWS Lightsail, Linode, and most cloud hosts block ports 25/465/587 outbound, breaking both web-form-to-email patterns and SMTP-emitting apps simultaneously. Posthorn solves that. But the broader value is the unified outbound layer pattern, which applies even when SMTP isn't blocked: self-hosters running multiple apps benefit from centralizing the outbound mail concern regardless of underlying infrastructure.

## Status log

- **2026-04-27** — Project named *caddy-formward* and scoped as a Caddy v2 HTTP handler module replacing the dead `SchumacherFM/mailout` plugin. Spec locked.
- **2026-04-27** — Scope expanded to email gateway (two ingress modes: HTTP form, SMTP listener) after analyzing the broader audience experiencing the same DigitalOcean SMTP-block pain. Project renamed to *Posthorn*. Repo renamed in place from `caddy-formward` to `posthorn`. Caddy module status changed from primary deliverable to optional adapter. Spec rewritten from scratch (this document); previous brief, PRD, and architecture documents replaced.
- **2026-05-15** — Positioning sharpened from "gateway for cloud platforms that block outbound SMTP" to "unified outbound mail layer for self-hosted projects" after triaging 10 incoming GitHub issues (most from the Pensum integration POV). The cloud-SMTP-block wedge is preserved as the canonical discovery entry point; the broader unified-layer framing is the durable value proposition. v1.x roadmap restructured (v1.1 API mode, v1.2 multi-transport + ops polish, v1.3 SMTP ingress) to support this positioning.

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

The Caddy v1 `mailout` plugin filled half this gap (web-form-to-email) for Caddy users; it was never ported to v2. Even if it had been, it never addressed the SMTP-emitting-app side.

There is no actively maintained, self-hosted, HTTP-API-first email gateway in 2026.

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
5. **Offers an optional Caddy module adapter** as a sibling Go module (`github.com/craigmccaskill/posthorn/caddy`) for operators who already run Caddy and prefer Caddyfile syntax for the form-ingress mode.

The architecture is deliberately ingress-agnostic and transport-agnostic. The reusable middle layer — Message, Transport, retry policy, structured logging — does not care whether a Message arrived via HTTP form parser or SMTP MIME parser, nor whether it leaves via Postmark JSON API or Mailgun multipart API.

## Target Users

### Primary (v1.0)

**Indie developers and homelab operators self-hosting one or more web services on cloud infrastructure that blocks outbound SMTP.** They pay $5-20/month for a DigitalOcean or Vultr droplet, run two to ten services in Docker Compose, and want one tool to handle both their contact form and their apps' transactional email. They will not migrate from Formspree (or accept broken admin login) unless setup takes ten minutes or less.

This is the author. The Ghost dogfooding case covers both ingress modes once v1.x SMTP ingress ships.

### Secondary (v1.0)

**Caddy users running it as the front door for one or more sites.** v1.0 ships an optional Caddy adapter so they can configure form ingress in their existing Caddyfile rather than running a separate `posthorn` container. They get the same core gateway, with Caddyfile ergonomics. v1.3 SMTP ingress will not be exposed through the Caddy adapter (different deployment shape — see architecture).

### Secondary, emerging (v1.1+)

**Indie developers building multi-project stacks who want one centralized outbound mail gateway across all their apps.** Concrete example: a single operator running a blog (Ghost), a SaaS project on Cloudflare Workers (Pensum), and a couple of internal tools. Each has its own outbound mail need; without Posthorn, each needs its own Postmark integration. With Posthorn (v1.1 API mode + v1.0 form ingress), all of them point at one Posthorn instance — one set of credentials, one set of logs, one set of bounce-handling decisions.

This audience is shape-distinct from the original "homelab operator with a contact form" audience — they care more about the API-mode shape (Cloudflare Workers calling Posthorn server-to-server with `Authorization: Bearer`) than the form-ingress shape. They were not anticipated by the original v1.0 spec; their requirements drove the v1.1 reshuffle on 2026-05-15.

### Future audiences (post-v1, named to constrain architecture)

- **Self-hosted-app operators not running Caddy.** Bigger TAM than Caddy users. The standalone binary serves them without requiring a proxy stack. Architecture must keep the core Caddy-independent so this audience is reachable without forks.
- **Small agencies and freelancers deploying for clients.** Need template scalability, per-tenant config isolation, file attachments, observability that flows into their existing log pipelines. Out of scope for v1.0 — but the Transport interface and config loader must grow into per-tenant use without a rewrite.
- **Caddy v1 mailout migrants.** Want a config-compatible upgrade path. Out of scope for v1.0 (clean-slate config syntax). Caddy adapter directive name (`posthorn` in Caddyfile) does not contradict v1's `mailout` semantics.

## Goals and Success Metrics

### Done — when is v1 ready to ship?

All four must pass:

1. The author's blog runs Posthorn for the contact form for 30 days with zero dropped submissions, confirmed via Postmark logs.
2. README has copy-pasteable examples for both deployment shapes (standalone Docker; Caddy adapter), each verified end-to-end on a clean install.
3. Tagged v1.0.0 release on GitHub with a published Docker image at `ghcr.io/craigmccaskill/posthorn:v1.0.0` and `:latest`.
4. The Caddy adapter is published as a separate Go module and listed on caddyserver.com modules page within 7 days of v1.0.0.

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
- Caddy adapter as a separate Go module exposing form ingress as `caddyhttp.MiddlewareHandler` (`http.handlers.posthorn`)
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

## Post-MVP Vision

The v1.x roadmap was restructured 2026-05-15 in response to triage of 10 incoming GitHub issues (most driven by a concrete second-user integration — Pensum). Current structure ships value in coherent themed releases rather than one large v1.1 "lots of things" release. The canonical user-facing roadmap lives at [posthorn.dev/roadmap/](https://posthorn.dev/roadmap/); this section is the authoritative scope source it derives from.

**v1.1 — API mode.** Posthorn becomes usable as an internal mail API, not just a contact-form gateway. Server-to-server callers (workers, daemons, paid-event handlers) can hit Posthorn without needing browser-shaped defenses.
- API-key auth per endpoint (`auth = "api-key"` mode, mutex with form-mode defenses)
- JSON content type on API-mode endpoints
- Idempotency keys via standard `Idempotency-Key` header (24h TTL, in-memory; durable across restarts in v2)
- Batch send with per-recipient template substitution against Postmark `/email/batch`

**v1.2 — multi-transport + operational maturity.** Posthorn isn't Postmark-locked, and it's now production-ready.
- Resend, Mailgun, AWS SES, outbound-SMTP transports
- CSRF + time-based form tokens (form-mode spam protection beyond v1.0 honeypot + Origin/Referer)
- `/healthz` endpoint, `/metrics` (Prometheus exposition), dry run mode
- Named presets for `trusted_proxies` (`cloudflare`, etc.) on top of v1.0 CIDR-list syntax
- IP-stripping option for GDPR contexts

**v1.3 — SMTP ingress.** The strategic feature that completes the gateway thesis. ~10-14 hours of focused work.
- TCP listener accepting SMTP from clients on the local network, forwarding via the configured HTTP API transport
- New threat model: open-relay prevention, RCPT validation, sender allowlist, recipient/size caps, optional client-cert auth
- New `smtp_listener` config section, new `Ingress` interface
- Caddy adapter does NOT receive SMTP ingress — different deployment shape (standalone is the natural sidecar)
- This is what unblocks the Ghost admin login use case.

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

## Technical Constraints (locked)

| Constraint | Value |
|---|---|
| Language | Go 1.25+ |
| License | Apache-2.0 (matches Caddy itself, simplifies the adapter relationship) |
| Distribution | GitHub releases (binary), Docker image at GHCR, `go install` |
| Repo | github.com/craigmccaskill/posthorn |
| Core Go module | github.com/craigmccaskill/posthorn |
| Caddy adapter Go module | github.com/craigmccaskill/posthorn/caddy |
| Caddy module ID | http.handlers.posthorn |
| Caddyfile directive (adapter) | posthorn |
| Standalone config format | TOML |
| Caddy version (adapter) | 2.9+ |
| Build tooling | `go build` (standalone), `xcaddy` (adapter) |
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

- **SMTP-ingress threats** (open relay, MX spoofing, RCPT bombing, recipient enumeration) — defended in v1.2 when SMTP ingress ships. Architecture must not foreclose those defenses.
- Botnet spam from many low-rate IPs — v3 (captcha or proof-of-work)
- DDoS / Layer 7 attacks — CDN's responsibility, not the gateway's
- API key theft from misconfigured deployment — operator concern, addressed via documentation

## Constraints and Assumptions

**Constraints:**
- Single author, working part-time
- 25-hour total budget for v1.0 implementation (vs 15h in the prior `caddy-formward` brief; expanded to cover the standalone-binary plumbing and dual-deployment-shape testing)
- 90-day calendar tripwire from project rename (2026-04-27 → 2026-07-26) to v1.0.0 tag
- All testing must be possible with infrastructure the author already has (Postmark account; no SMTP server required for v1.0)

**Assumptions:**
- Postmark API and free-tier availability remain unchanged in pricing structure
- Caddy 2.9 module API remains stable through v1.0 development (affects adapter only)
- The `posthorn` repo name and Go module path remain unclaimed (verified 2026-04-27)
- Docker Hub `craigmccaskill/posthorn` namespace remains unclaimed by anyone other than author

## Risks

| ID | Risk | Likelihood | Impact | Mitigation |
|----|------|------------|--------|------------|
| R1 | Solo maintainer abandonment | Medium | High | Time-bound commitment statement in README; 90-day shipping tripwire — if v1.0 isn't shipped within 90 days of project rename, scope cuts further (cut Caddy adapter from v1.0 if necessary; keep core) |
| R2 | Effort blowup beyond 25h budget | High | High | Hard tripwire at 25h. Cut order: Caddy adapter first (v1.1 release item), then polish (better errors, validator depth). Core gateway is non-cuttable. |
| R3 | Discoverability failure | Medium | High | Multi-channel: caddyserver.com modules-page submission for adapter; Docker Hub README and topics for standalone; launch blog post documenting the Ghost-on-DO end-to-end story; submit to Hacker News once v1.2 SMTP ingress is dogfooded |
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
- Caddy v2 module docs: https://caddyserver.com/docs/extending-caddy (for the adapter only)
- xcaddy: https://github.com/caddyserver/xcaddy (for the adapter only)
- Original v1 Caddy mailout plugin (dead): https://github.com/SchumacherFM/mailout
- Posthorn historical context: https://en.wikipedia.org/wiki/Post_horn

### C. Related project documents

- Posthorn Dashboard (in author's Obsidian vault) — project roadmap, v2/v3 scope context, post-v1 audience research
- Ghost Migration project notes (in author's Obsidian vault) — immediate consumer of v1.x SMTP ingress
