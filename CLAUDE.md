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

## Status (as of 2026-09-29)

**Phase: v2.0.0 released 2026-09-29.** Epics 13–17 merged 2026-08-03 (PRs #96–#99); rc.1/rc.2 tagged the same day. Story 18.3 landed 2026-09-29 as a scripted live validation (PR #112): a real Postmark send, Delivery and hard-bounce webhooks back through a cloudflared quick tunnel, HMAC verified independently, suppression enforced, CLI row listed. `make validate-lifecycle` locally; `lifecycle-live.yml` on manual dispatch and every `v*-rc*` tag behind the `live-providers` Environment (missing secret = hard fail on rc tags). First live validation of any v2 surface; green on the first run (29s). rc.3 carried #107 `hello_hostname` (bautisflow), #113 SMTP transfer-encoding decode (derekantrican, listmonk on Railway → Resend), the #119 metrics-before-reply fix (root cause of the `TestSMTP_RecorderWired` flake: the DATA reply was written before the recorder call, on all three outcome paths), CI on Go 1.26 with setup-go v7, and the Dependabot batch (sqlite 1.59.0, distroless digest, action bumps). rc.3's lifecycle gate passed but its Release run failed the Trivy step: eight HIGH Go stdlib CVEs in the builder pin (Go 1.26.5, from the August CVE fix), all fixed in 1.26.6+. rc.4 = rc.3 + builder bump to 1.26.8-alpine (digest-pinned); GA is tagged from rc.4. Lesson recorded: the builder pin and CI's `go-version` must move together. v2.0 scope is the 2026-08-02 recut: #5 HTML body (ADR-19), #21/#22 storage spine + durable idempotency (ADR-20/21), #8 Postmark lifecycle (ADR-22), #4 minimal suppression (ADR-23), #23 webhook transport (ADR-24), #24 attachments (ADR-25); FR71–FR92, NFR25–NFR31, Epics 13–18. Doctrine unchanged: security features opt-in never default-on (NFR25); no `[storage]` = v1.x byte-identical.

**Earlier: v1.2.0 released 2026-07-04** (spam-protection minor; validated via `v1.2.0-rc.1` deployed on craigmccaskill.com against live spam before GA). `:latest`/`:1`/`:1.2`/`:1.2.0` → the release; `:1.1.0`/`:1.0.0` preserved. **v1.2.0 = the spam checks** the whole audit was in service of: after real captured spam showed the pattern is content-shapeless (distributed reply-bait, burner Gmails, four languages, 0–1 links), the milestone was re-cut to content-agnostic checks — #44 reputation (StopForumSpam email/IP, fail-open, cached), #45 proof-of-browser (ADR-18 challenge endpoint + `min_age` time-trap), #33 Turnstile (fail-closed backstop), all on the #60 two-phase pipeline. Plus #57/#58 SMTP metrics/validate fixes and Authentik/Mastodon recipes. Content-shape #34 and domain-blocklist #35 deprioritized against the data.

**Earlier: v1.1.0 (2026-07-04) = the full-audit hardening release** (PRs #49/#64/#65/#66/#67/#77/#78/#79 + 6 Dependabot): #37 listener-only-config bug, #41 auth-none public-bind refusal (BREAKING — needs `trusted_network = true`), #42 server timeouts, #50 SMTP brute-force/conn caps, #59 shared retry, #38/#39/#56 CI/supply-chain, #76 provider-testing harness. v1.0.0 was 2026-05-26. Two external humans engaged (amit-tewari #30 multi-tenant SMTP; monperrus python-posthorn).

**Remaining / next:** the first minor after 2.0 is listener-focused: #120 multiple `[[smtp_listeners]]` (ADR-13 amendment; the decision given to amit-tewari on #30) and #115 allowlisted `List-Unsubscribe` passthrough (touches ADR-12; from Derek's listmonk workload, ~100–200 msgs/month, wants it for reputation not compliance). Both need a spec line before code. Operator QoL: #118 config from an env var (counter-proposal to Derek's wait-for-config mode; awaiting his answer), #27, #28, #95. Docs: #116 listmonk recipe (Railway IPv6 bind vs #41, listmonk concurrency vs `max_connections_per_ip`). Held on purpose: #110 builder → Go 1.27 (merge post-GA and bump CI in the same PR). Spam tail (#61/#62/#63/#32) and audit tail (#51–55, #74, #75) unchanged: build on live signal. **Bookkeeping resolved 2026-09-29:** `live-providers` had no secrets and the repo no variables until today, so every green weekly `Integration (live)` run only ever exercised Postmark's public test token; Resend/Mailgun/SES skip. Now set: `POSTMARK_SERVER_TOKEN` (environment secret), `POSTHORN_TEST_FROM`/`POSTHORN_TEST_TO` (repo variables). The brief's "operator validation truth" and docs/manual-test.md still need that sentence. The site's [roadmap page](./site/src/content/docs/roadmap.mdx) is canonical.

**Pre-tag hardening (2026-05-17 → 2026-05-26), after the v1.0 scope freeze:**
- `redirect_success` / `redirect_error` were configured-but-never-wired in the handler; fixed in `a470328` during the full site audit pass.
- Pre-launch security pass (`edb6692`): body-size default, brute-force defense on api-mode auth failures, CODEOWNERS.
- Strict TOML field validation — unknown config keys are now parse errors (`2f75b2e`; the planned v1.0.1 was folded into v1.0.0 pre-tag).
- SMTP listener: `auth_required = "none"` for internal-network deployments (`4d85643`); STARTTLS default aligned with the marketing claim (`8bfe5bf`).
- 413 response body now includes the configured limit (`708521b`).

**Operator validation truth (was Story 12.3):** Postmark (2026-05-16) and Resend (2026-05-24, recorded in [docs/manual-test.md](docs/manual-test.md) with DKIM/SPF pass + NFR3 sentinel check) are validated against live accounts. **Mailgun, AWS SES, outbound-SMTP relay, and the SMTP listener shipped without recorded live validation.** Manual-test procedures exist for all of them; this is open operational debt, not closed.

**Issue tracker (groomed):**
- v1.1 spam minor (milestone 1): #31 honeypots, #32 CSRF min-age, #33 Turnstile, #34 content-shape, #44 StopForumSpam, #45 proof-of-browser (design settled: ADR-18 challenge endpoint), #60 pipeline pre-work, #61 on_spam/tag, #62 single-use tokens, #63 strict origin. Demoted: #35 domain blocklist, #36 per-day cap.
- 2026-07-03 audit queue: #50 SMTP brute-force/conn limits (highest severity), #51 internal ops bind, #52 SMTP CRLF defense-in-depth, #53 skip-verify WARN, #54 allowlist case bug, #55 inflight bound, #56 release supply chain, #57 SMTP metrics dead code, #58 validate skips SMTP checks, #59 SMTP retry-policy divergence (spec decision)
- QoL queue (v1.0.x or v1.1): #27 ephemeral self-signed TLS cert, #28 friendlier validation errors, #29 flatten Go module to repo root for `go install` (conflicts with ADR-2 — decide deliberately)
- v2 platform: #4, #5, #8, #9, #21–25
- **#30 (external, amit-tewari, 2026-05-28): sender-driven multi-tenant SMTP routing.** Challenges the roadmap's RCPT-driven framing and touches ADR-13. Treat as spec/roadmap conversation, not code.

**Repo state:** Single Go module at `core/` with 19 packages plus `cmd/posthorn/`:
- v1.0 originals: `config`, `gateway`, `transport`, `spam`, `ratelimit`, `validate`, `template`, `response`, `log`
- Added 2026-05-16 (block B+C+D): `idempotency`, `csrf`, `metrics`, `ingress`, `smtp`
- Added 2026-07 (v1.2): `reputation`, `captcha`
- Added 2026-08 (v2.0): `storage`, `lifecycle`, `providertest` (live tiers: `-tags integration` provider battery, `-tags lifecyclelive` Story 18.3)

Public docs site at [posthorn.dev](https://posthorn.dev) (Astro + Starlight, deployed via GH Pages from `site/`); grew well past the launch 41 pages with post-launch recipes (Ghost, Gitea, Hugo+Comentario, Umami) and D2 diagrams.

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
- ✅ Epic 12 (12.1-12.4) — doc rescope sweep, README rewrite, operator validation (partial — see "Operator validation truth" above), v1.0.0 tagged 2026-05-26. **All epics closed.**

**Architecture deviations from original spec (cumulative):**
- `core/http/` → `core/gateway/` to avoid shadowing stdlib `net/http`.
- Retry timing constants declared as package vars (not consts) so tests can override via `gateway.SetRetryDelaysForTest`. **2026-07-03 (#59):** the FR19-22 policy itself moved to `transport.SendWithRetry`, shared by both ingresses — previously the SMTP session called `Send` directly and skipped retries. The gateway test hook keeps its API and now sets `transport.TransientRetryDelay`/`RateLimitedRetryDelay`.
- [`site/`](./site/) Astro + Starlight directory; deploys via [`.github/workflows/site-deploy.yml`](./.github/workflows/site-deploy.yml). Build: `cd site && npm ci && npm run build`.
- **2026-05-15:** Caddy v2 adapter module cut. FR27–FR30, NFR10 deleted; ADR-6, ADR-7 retired in-place.
- **2026-05-15:** Honeypot 200 response shape parity (commit `0f27f4c`).
- **2026-05-15:** v1.0 recipes shipped (commit `250f460`).
- **2026-05-16 (morning):** `transport.Transport.Send` signature changed from `error` to `(SendResult, error)`. `SendResult.MessageID` captures the upstream's message ID; logged as `transport_message_id` in `submission_sent`. Forward-compatible for all transports added later that day.
- **2026-05-16 (during day):** Batch send dropped from v1.1 scope entirely. Documented in brief's "Deliberately not on the roadmap" section. The user pushed the "ships when an operator surfaces with concrete batch volume" framing.
- **2026-05-16 (later day):** Per-request `to_override` added to api-mode (FR46, ADR-11). Originally deferred (D5 in design discussion) but the Cloudflare Worker recipe surfaced that the named v1.1 audience can't pre-declare recipients. `from` stays endpoint-configured to prevent spoofing.
- **2026-05-16 (end of day):** v1.x scope collapsed into a single v1.0 release (see "Major scope expansion" above). PRD amendment headings reorganized as block B/C/D. CHANGELOG consolidated.
- **2026-05-16:** ADR-14 tripwire fired during SES SigV4 implementation. Total LOC (including tests) reached 1,158; implementation-only 493. Deviation accepted (option a in the ADR) — tests are 400-500 LOC per transport uniformly across the codebase, so the "include tests in tripwire" wording was overly conservative.

**Deps in module:** `github.com/BurntSushi/toml`, `github.com/google/uuid`, `github.com/hashicorp/golang-lru/v2`, `modernc.org/sqlite` (ADR-20; quarantined behind `core/storage`). Four direct external deps for the whole codebase. Every transport bespoke.

**Launch artifacts in the vault (not in this repo):** launch happened 2026-05-26 around the v1.0.0 release. The blog post ([`~/vaults/cmcc/Areas/Blog/Posthorn Launch.md`](~/vaults/cmcc/Areas/Blog/Posthorn Launch.md)) and distribution playbook ([`~/vaults/cmcc/Projects/Posthorn/Distribution playbook.md`](~/vaults/cmcc/Projects/Posthorn/Distribution playbook.md)) are historical at this point; check the vault for which channels actually fired before reusing any pre-staged text.

**Test health (verified 2026-09-29):** `go vet ./... && go test -race -count=1 ./...` from `core/` passes on Go 1.26.5 locally and 1.26.8 in CI (main @ the rc.4 commit). The `ingress` package has no test files (interface-only, accepted). Go lives at `~/.local/go/bin` on the dev box and is not on the non-interactive PATH.

## Read the spec before touching code

The v1.0 specification is across three documents in [`spec/`](./spec/):

1. **[`spec/01-project-brief.md`](./spec/01-project-brief.md)** — problem, users, scope, success metrics, threat model, risks, constraints. Status log captures the major scope decisions (2026-04-27 initial, 2026-04-27 SMTP-broader scope, 2026-05-15 Caddy cut, 2026-05-16 v1.1 amendment + batch drop + `to_override` add + v1.x-fold-into-v1.0).
2. **[`spec/02-prd.md`](./spec/02-prd.md)** — FR1–FR68 and NFR1–NFR24 organized as four blocks (A: form ingress, B: API mode, C: multi-transport + ops, D: SMTP ingress). FR27–FR30 / NFR10 / NFR14 are deleted slots from the 2026-05-15 Caddy adapter cut. Epics 1–12. Traceability table maps every FR to a brief section.
3. **[`spec/03-architecture.md`](./spec/03-architecture.md)** — file layout, lifecycle, request flow, component design, threat→defense mapping (now covers HTTP form + API mode + SMTP threats), dependencies, ADRs 1–18, forward-compatibility commitments for v2.

The PRD has the canonical FR/NFR list with "must"-level requirements; the architecture doc has the implementation blueprint, including the target file layout under §"File layout".

## Hard guardrails

These derive from the spec. Do not violate without an explicit conversation that updates the spec first.

1. **Sanctioned scope is shipped v1.0 plus the v1.1 spam-protection milestone ([milestone 1](https://github.com/craigmccaskill/posthorn/milestone/1): #31–34, #44, #45, pipeline pre-work #60, and the 2026-07-03 scope additions #61 on_spam/tag, #62 single-use tokens, #63 strict origin) and the QoL queue (#27, #28).** #35/#36 are demoted out of the milestone (0/5 against the observed spam corpus; re-enter on a matching real-world pattern). on_spam digest mode is explicitly deferred to v1.1.x. **As of 2026-08-02, v2.0 block E is also sanctioned** ([milestone 3](https://github.com/craigmccaskill/posthorn/milestone/3), recut same day: #4-minimal, #5, #8, #21, #22, #23, #24 — FR71–FR92, Epics 13–18). Do NOT implement the demand-gated items without a spec conversation: #9 unsubscribe injection (and no campaign features ever — no scheduling, segments, campaign state, or open/click analytics; that's Listmonk's lane), #25 fan-out, SMTP-listener attachment passthrough, batch send, multi-tenant SMTP routing, HTTP suppression admin API. v3 features (admin UI, proof-of-work, PGP) are even further out. Suppression stores the minimum state that protects the operator's provider account, nothing more.

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
- **ADR-18 (2026-07-03):** Proof-of-browser (#45) tokens come from a Posthorn-served challenge GET endpoint — a scoped exception to ADR-16's no-round-trip stance (which is unchanged for CSRF). Composes with #62 single-use cache + min-age; CORS via `allowed_origins`; rate-limited. Deliberately recorded as defeating no-JS bots, not adapted bots — Turnstile (#33) stays the escalation tier.
- **ADR-19 (2026-08-02):** HTML bodies render through `html/template`, never `text/template` — contextual auto-escaping makes submitter input inert by construction (NFR1 in HTML context). Raw passthrough deliberately impossible; a future `trusted_fields` opt-in would need its own ADR. Text part auto-derived by bespoke converter; `text_body` overrides.
- **ADR-20 (2026-08-02):** SQLite via `modernc.org/sqlite` — pure Go preserves the static multi-arch build; the largest dep ever taken, named honestly (transpiled C, big module tree), quarantined behind `core/storage`. `max_size` → `max_page_count` so Posthorn can't fill its own disk; ballast pattern recorded-not-shipped.
- **ADR-21 (2026-08-02, revises ADR-5):** Sync-first retry queue — request path unchanged, 200 still means sent; 202 "queued" only where v1.x dropped mail after inline retries. 2xx = terminally handled (`sent`/`queued`/`suppressed`). At-least-once with documented crash-window duplicates (NFR28). `log_failed_submissions = false` disables queueing. Storage failure degrades to v1.x sync; mail never blocks on the disk.
- **ADR-22 (2026-08-02):** Lifecycle ingestion Postmark-only behind a provider-agnostic normalized event shape; basic-auth fail-closed. Costs named: per-provider capability matrix; requires Posthorn reachable by provider webhooks.
- **ADR-23 (2026-08-02):** Suppression reason-scoped (bounce/complaint = global; unsubscribe-class = per-endpoint, unshipped) and CLI-managed (`posthorn suppressions`); rows exempt from retention; no HTTP admin API.
- **ADR-24 (2026-08-02, amends ADR-12):** `Message.Fields` (raw submitted values) crosses the boundary for structured sinks; HTTP ingresses populate, SMTP leaves nil, mail transports must ignore, never in headers.
- **ADR-25 (2026-08-02):** Attachments opt-in + fail-closed: explicit `allowed_types` required, enforcement on sniffed bytes not declared type; form + api-mode only.

If you find yourself wanting to deviate from an active ADR, update the architecture doc with the new decision and rationale before changing code.

## When in doubt

1. Re-read the relevant spec section
2. If the spec is silent or ambiguous, the architecture doc's "Open architectural questions" section may already have a recommended answer
3. If neither helps, ask the author before improvising

The cost of asking is small. The cost of building the wrong thing is real LOC + test surface to walk back.
