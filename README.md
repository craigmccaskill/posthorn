# Posthorn

[![CI](https://github.com/craigmccaskill/posthorn/actions/workflows/ci.yml/badge.svg)](https://github.com/craigmccaskill/posthorn/actions/workflows/ci.yml)
[![Docs](https://img.shields.io/badge/docs-posthorn.dev-d4a300)](https://posthorn.dev)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](./LICENSE)
[![Go version](https://img.shields.io/badge/go-1.25%2B-00ADD8.svg)](https://go.dev)

**The unified outbound mail layer for self-hosted projects.** One gateway between every app you self-host and the transactional mail provider you've already picked. Three ingress shapes (HTTP form, HTTP API, SMTP), five transports (Postmark, Resend, Mailgun, AWS SES, outbound-SMTP relay), single Go binary, single TOML config.

> *Not related to [PostHog](https://posthog.com). PostHog is product analytics. Posthorn is an email gateway. Different categories, zero functional overlap.*

## Why

Nobody wants to run a mail server in 2026. Self-hosted operators use Postmark, Resend, Mailgun, or AWS SES because they're cheap, they handle deliverability properly, and someone else worries about SPF / DKIM / DMARC / bounces / sender reputation.

But every app you self-host has to integrate with that service **independently.** Your contact form. Your Ghost blog's admin emails. Your Gitea magic links. Your Mastodon notifications. The Cloudflare Worker that fires a password-reset email when someone clicks the link. Each one needs its own copy of the API key, its own integration code, its own quirks around retry and bounce handling. **The same outbound concern duplicated across your stack.**

And on cloud hosts that block outbound SMTP — DigitalOcean, AWS Lightsail, Linode, Vultr — the SMTP-only apps don't work at all without a workaround.

Posthorn is the bridge. One container, one config, one set of credentials. Your apps point at Posthorn. Posthorn talks to your provider.

| Where your app connects | What Posthorn does |
|---|---|
| HTTP form (contact forms, signups, alert webhooks) | Honeypot + Origin/Referer + rate limit + optional CSRF; templates the email; sends |
| HTTP API mode (workers, cron, payment handlers, internal services) | `Authorization: Bearer` auth; JSON body; idempotent retries; per-request `to_override` for transactional sends |
| SMTP listener (Ghost, Gitea, Mastodon, Matrix, NextCloud, Authentik, anything that emits SMTP) | AUTH PLAIN or client-cert; STARTTLS-required; sender + recipient allowlists; parses MIME; forwards via HTTP API transport |

All three ingresses converge on one `transport.Message` and one outbound provider — pick from Postmark, Resend, Mailgun, AWS SES, or an outbound-SMTP relay.

## What Posthorn is not

To save you a wrong turn:

| | What it does | Look at instead |
|---|---|---|
| **Not a mail server** | No mailbox storage, no IMAP/JMAP, no DKIM key management, no MX target | [Stalwart](https://stalw.art), [Mailcow](https://mailcow.email), [iRedMail](https://www.iredmail.org) |
| **Not its own outbound infrastructure** | Posthorn relays through a provider you chose; it doesn't run its own SMTP fleet or manage IP reputation | [Postal](https://postalserver.io), [Hyvor Relay](https://hyvor.com/relay) |
| **Not a marketing email platform** | No list management, no segmentation, no campaign dashboard | [Listmonk](https://listmonk.app) |
| **Not webmail / a mailbox UI** | No interface for reading mail | Roundcube, Snappymail (with a mail server) |

The wedge is **the integration layer** between your self-hosted apps and the transactional provider you've already picked.

## Documentation

**[posthorn.dev](https://posthorn.dev)** — getting started, configuration reference, deployment guides, feature deep-dives, security model, HTTP API reference, FAQ. Five recipes covering contact forms, signup notifications, multi-form sites, monitoring alerts, and transactional email from a Cloudflare Worker.

For project history and the v1.0 spec, see [`spec/`](./spec/).

## Quick start (Docker)

```yaml
# docker-compose.yml
services:
  posthorn:
    image: ghcr.io/craigmccaskill/posthorn:latest
    restart: unless-stopped
    volumes:
      - ./posthorn.toml:/etc/posthorn/config.toml:ro
    environment:
      POSTMARK_API_KEY: ${POSTMARK_API_KEY}
    ports:
      - "127.0.0.1:8080:8080"   # bind to loopback; reverse-proxy from your front door
```

```toml
# posthorn.toml
[[endpoints]]
path = "/api/contact"
to = ["you@example.com"]
from = "Contact Form <noreply@example.com>"
honeypot = "_gotcha"
allowed_origins = ["https://example.com"]
required = ["name", "email", "message"]
subject = "Contact from {{.name}}"
body = """
From: {{.name}} <{{.email}}>

{{.message}}
"""
redirect_success = "/thank-you"

[endpoints.transport]
type = "postmark"

[endpoints.transport.settings]
api_key = "${env.POSTMARK_API_KEY}"

[endpoints.rate_limit]
count = 5
interval = "1m"
```

Reverse-proxy `/api/contact` from your front door (Caddy, nginx, Traefik) to `http://posthorn:8080`. Point your form's `action` at `/api/contact`. Done.

Full walkthrough: [posthorn.dev/getting-started/quick-start](https://posthorn.dev/getting-started/quick-start/).

## API mode (server-to-server)

For Workers, cron jobs, internal services — anything that speaks JSON instead of forms:

```toml
[[endpoints]]
path = "/api/transactional"
to = ["fallback@yourdomain.com"]
from = "YourApp <noreply@yourdomain.com>"
auth = "api-key"
api_keys = ["${env.WORKER_KEY_PRIMARY}", "${env.WORKER_KEY_BACKUP}"]
required = ["subject_line", "message"]
subject = "{{.subject_line}}"
body = "{{.message}}"

[endpoints.transport]
type = "postmark"

[endpoints.transport.settings]
api_key = "${env.POSTMARK_API_KEY}"
```

```bash
curl -X POST https://posthorn.yourdomain.com/api/transactional \
  -H "Authorization: Bearer $WORKER_KEY_PRIMARY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: reset:user-123:$(date -u +%FT%H)" \
  --data '{
    "to_override": "alice@example.com",
    "subject_line": "Reset your password",
    "message": "Click here: https://app.example.com/reset/abc"
  }'
```

Full walkthrough: [posthorn.dev/recipes/cloudflare-worker](https://posthorn.dev/recipes/cloudflare-worker/).

## SMTP listener (Ghost / Gitea / Mastodon / Authentik)

For apps that speak SMTP natively and can't be reconfigured to call an HTTP API:

```toml
[smtp_listener]
listen          = ":2525"
require_tls     = true
tls_cert        = "/etc/posthorn/cert.pem"
tls_key         = "/etc/posthorn/key.pem"
auth_required   = "smtp-auth"
allowed_senders = ["*@yourdomain.com"]
max_recipients_per_session = 10
max_message_size = "1MB"

[[smtp_listener.smtp_users]]
username = "ghost"
password = "${env.GHOST_SMTP_PASSWORD}"

[smtp_listener.transport]
type = "postmark"

[smtp_listener.transport.settings]
api_key = "${env.POSTMARK_API_KEY}"
```

Point Ghost (or any app's SMTP config) at `posthorn.yourdomain.com:2525` with the username/password above. Posthorn parses the MIME, builds a `transport.Message`, forwards via Postmark.

Full doc: [posthorn.dev/features/smtp-ingress](https://posthorn.dev/features/smtp-ingress/).

## Picking a transport

| Transport | Best for | Auth | Body |
|---|---|---|---|
| **Postmark** | Transactional email, strong deliverability defaults | `X-Postmark-Server-Token` | JSON |
| **Resend** | Modern HTTP API, developer-friendly dashboard | `Authorization: Bearer` | JSON |
| **Mailgun** | Higher-volume transactional, US + EU regions | HTTP Basic | multipart/form-data |
| **AWS SES** | AWS-native deployments, cheapest at volume | AWS SigV4 (bespoke) | JSON |
| **Outbound SMTP** | Any STARTTLS-capable relay (Mailtrap, your Postfix smarthost, etc.) | AUTH PLAIN | SMTP DATA |

Switching providers is a TOML edit — every transport implements the same `Transport` interface. See [posthorn.dev/configuration/transports](https://posthorn.dev/configuration/transports/) for per-provider config.

## Production checklist

Before pointing real traffic at Posthorn:

1. **DNS** — SPF, DKIM, and DMARC records on your sending domain. Without these your mail goes to spam. See [posthorn.dev/security/dns](https://posthorn.dev/security/dns/).
2. **Reverse proxy** — Posthorn does not terminate TLS. Run it behind Caddy, nginx, or Traefik. See [posthorn.dev/deployment/reverse-proxy](https://posthorn.dev/deployment/reverse-proxy/).
3. **`allowed_origins`** (form-mode endpoints) — set this to lock submissions to your domain. Without it, anyone can POST to your endpoint.
4. **`rate_limit`** — set a tight bucket per endpoint (5/minute is a sensible default for a public contact form; API mode rate-limits per matched key).
5. **`trusted_proxies`** — if behind a reverse proxy, list its CIDR (or use the `cloudflare` named preset) so the rate limiter sees the real client IP.
6. **`/healthz` and `/metrics`** — auto-registered on the same listener. Wire your Docker healthcheck or Prometheus scrape to these.

The full operator checklist is on [posthorn.dev](https://posthorn.dev).

## What's in v1.0

| Block | Detail |
|---|---|
| **Form ingress** | Form-encoded + multipart bodies; honeypot, Origin/Referer fail-closed, rate limit, optional CSRF tokens |
| **API mode** | `auth = "api-key"` with Bearer tokens (constant-time compare); JSON content type; idempotency keys (24h, in-memory LRU); per-request `to_override` |
| **Transports** | Postmark, Resend, Mailgun, AWS SES (bespoke SigV4), outbound-SMTP relay |
| **SMTP listener** | TCP listener with AUTH PLAIN / client-cert, STARTTLS-required, sender + recipient allowlists, size cap, MIME → `transport.Message` |
| **Operations** | `/healthz`, `/metrics` (Prometheus exposition), dry-run mode, IP-stripping, named `trusted_proxies` presets (Cloudflare) |
| **Failure handling** | 1 retry on transient/5xx (1s), 1 retry on 429 (5s), 10s hard timeout |
| **Logging** | Structured JSON; UUIDv4 submission IDs and SMTP session IDs; `transport_message_id` in `submission_sent` |
| **Deployment** | Single Go binary, multi-arch distroless Docker image at `ghcr.io/craigmccaskill/posthorn` |

Three external Go dependencies in the whole module: TOML parser, UUID library, LRU cache. Every transport is bespoke — no vendor SDK in transport code.

## Roadmap

**v2 — platform maturity.** SQLite submission log, retry queue across restarts, suppression list (auto on hard bounces), durable idempotency, lifecycle event callbacks via HMAC-signed webhook, RFC 8058 one-click unsubscribe, file attachments, HTML body, multiple outputs per endpoint (email + webhook + log fan-out), multi-tenant SMTP routing.

**v3 — speculative.** Admin UI, proof-of-work spam challenge, PGP encryption. Depends on community traction.

Full trajectory: [posthorn.dev/roadmap](https://posthorn.dev/roadmap/).

## Build from source

Requires Go 1.25+.

```bash
git clone https://github.com/craigmccaskill/posthorn
cd posthorn/core
go build -o /tmp/posthorn ./cmd/posthorn
/tmp/posthorn version
```

Or install directly:

```bash
go install github.com/craigmccaskill/posthorn/cmd/posthorn@latest
```

## Contributing

The v1.0 specification is in [`spec/`](./spec/) (brief, PRD, architecture). The architecture doc at [`spec/03-architecture.md`](./spec/03-architecture.md) is the source of truth for design questions.

Security issues: see [SECURITY.md](./SECURITY.md) — do **not** open public issues for security disclosures.

## License

Apache-2.0. See [LICENSE](./LICENSE).
