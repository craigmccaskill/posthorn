# Posthorn

[![CI](https://github.com/craigmccaskill/posthorn/actions/workflows/ci.yml/badge.svg)](https://github.com/craigmccaskill/posthorn/actions/workflows/ci.yml)
[![Docs](https://img.shields.io/badge/docs-posthorn.dev-d4a300)](https://posthorn.dev)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](./LICENSE)
[![Go version](https://img.shields.io/badge/go-1.25%2B-00ADD8.svg)](https://go.dev)

**The unified outbound mail layer for self-hosted projects.** One gateway between your apps and your transactional mail provider — Postmark today; Resend, Mailgun, AWS SES, and outbound-SMTP relay coming in v1.2. Self-hosted, no mail server required.

> *Not related to [PostHog](https://posthog.com). PostHog is product analytics. Posthorn is an email gateway. Different categories, zero functional overlap.*

## Why

Nobody wants to run a mail server in 2026. Self-hosted operators use Postmark, Resend, Mailgun, or AWS SES because they're cheap, they handle deliverability properly, and someone else worries about SPF / DKIM / DMARC / bounces / sender reputation.

But every app you self-host has to integrate with that service **independently.** Your contact form. Your Ghost blog's admin emails. Your Gitea magic links. Your Mastodon notifications. Your worker that fires a license-delivery email when someone pays. Each one needs its own copy of the API key, its own integration code, its own quirks around retry and bounce handling. **The same outbound concern duplicated five times across your stack.**

And on cloud hosts that block outbound SMTP — DigitalOcean, AWS Lightsail, Linode, Vultr — the SMTP-only apps don't work at all without a workaround.

Posthorn is the bridge. One container, one config, one set of credentials. Your apps point at Posthorn. Posthorn talks to your transactional mail provider.

| Where you connect | Where Posthorn routes to | Today |
|---|---|---|
| HTTP form (contact forms, signups, webhooks) | Postmark | **v1.0** |
| JSON API (workers, cron, payment handlers, internal services) | Postmark | **v1.1** |
| SMTP (Ghost, Gitea, Mastodon, Matrix, NextCloud, Authentik) | Postmark + Resend / Mailgun / SES / SMTP relay | **v1.3** |

The full trajectory is on the [roadmap page](https://posthorn.dev/roadmap/).

## What Posthorn is not

To save you a wrong turn:

| | What it does | Look at instead |
|---|---|---|
| **Not a mail server** | No mailbox storage, no IMAP/JMAP, no DKIM key management. | [Stalwart](https://stalw.art), [Mailcow](https://mailcow.email), [iRedMail](https://www.iredmail.org) |
| **Not its own outbound infrastructure** | Posthorn relays through a provider you chose; it doesn't run its own SMTP fleet or manage IP reputation. | [Postal](https://postalserver.io), [Hyvor Relay](https://hyvor.com/relay) |
| **Not a marketing email platform** | No list management, no segmentation, no campaign dashboard. | [Listmonk](https://listmonk.app) |
| **Not webmail / a mailbox UI** | No interface for reading mail. | Roundcube, Snappymail (with a mail server) |

The wedge is **the integration layer** between your self-hosted apps and the transactional provider you've already picked.

## Documentation

**[posthorn.dev](https://posthorn.dev)** — full docs: getting started, configuration reference, deployment guides, feature deep-dives, security model, HTTP API reference, FAQ.

For project history and the locked v1.0 spec, see [`spec/`](./spec/).

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

## Caddy adapter (optional)

For operators already running Caddy as a front door:

```bash
xcaddy build --with github.com/craigmccaskill/posthorn/caddy
```

```caddyfile
example.com {
    posthorn /api/contact {
        to you@example.com
        from "Contact Form <noreply@example.com>"
        required name email message
        honeypot _gotcha
        allowed_origins https://example.com
        subject "Contact from {{.name}}"
        body "From {{.name}} <{{.email}}>: {{.message}}"
        rate_limit 5 1m

        transport postmark {
            api_key {env.POSTMARK_API_KEY}
        }
    }
}
```

Both deployment shapes run the same internal pipeline — they accept identical inputs and produce identical outbound mail. Parity is asserted in CI ([`caddy/caddyfile_test.go`](./caddy/caddyfile_test.go)) and validated end-to-end via the [manual test procedure](./docs/manual-test.md).

Full adapter docs: [posthorn.dev/deployment/caddy-adapter](https://posthorn.dev/deployment/caddy-adapter/).

## Production checklist

Before pointing real traffic at Posthorn:

1. **DNS** — SPF, DKIM, and DMARC records on your sending domain. Without these your mail goes to spam. See [posthorn.dev/security/dns](https://posthorn.dev/security/dns/).
2. **Reverse proxy** — Posthorn does not terminate TLS. Run it behind Caddy, nginx, or Traefik. See [posthorn.dev/deployment/reverse-proxy](https://posthorn.dev/deployment/reverse-proxy/).
3. **`allowed_origins`** — set this to lock submissions to your domain. Without it, anyone can POST to your endpoint.
4. **`rate_limit`** — set a tight bucket per endpoint (5/minute is a sensible default for a public contact form).
5. **`trusted_proxies`** — if behind a reverse proxy, list its CIDR so the rate limiter sees the real client IP.

The full operator checklist is on [posthorn.dev](https://posthorn.dev).

## Roadmap

| Version | Scope |
|---|---|
| **v1.0** | HTTP form ingress, Postmark transport, full spam-protection stack, rate limiting, templating, JSON+redirect responses, standalone Docker + Caddy adapter |
| **v1.1** | **API mode** — Posthorn becomes an internal mail API alongside its form-ingress role. Per-endpoint API-key auth, JSON content type, idempotency keys (in-memory), batch send |
| **v1.2** | **Multi-transport + ops polish** — Resend, Mailgun, AWS SES, outbound-SMTP transports; CSRF + time-based token spam protection; `/healthz`, `/metrics`, dry run mode |
| **v1.3** | **SMTP ingress** — TCP listener accepting SMTP from internal apps (Ghost, Gitea, Mastodon) and forwarding via the configured HTTP API transport |
| **v2** | **Platform maturity** — SQLite submission log, retry queue across restarts, suppression list, durable idempotency, lifecycle event callbacks, RFC 8058 unsubscribe, file attachments |
| **v3** | Admin UI, proof-of-work spam challenge, PGP encryption |

The full feature breakdown lives in [`spec/01-project-brief.md`](./spec/01-project-brief.md) §"Post-MVP Vision".

## Build from source

Requires Go 1.25+.

```bash
git clone https://github.com/craigmccaskill/posthorn
cd posthorn
go build -o posthorn ./core/cmd/posthorn
./posthorn version
```

Or install directly:

```bash
go install github.com/craigmccaskill/posthorn/cmd/posthorn@latest
```

## Contributing

The v1.0 specification is locked. Implementation follows the epic-and-story breakdown in [`spec/02-prd.md`](./spec/02-prd.md). Contributions outside the locked v1.0 scope should wait for v1.1 planning.

For implementation questions, the architecture document at [`spec/03-architecture.md`](./spec/03-architecture.md) is the source of truth.

Security issues: see [SECURITY.md](./SECURITY.md) — do **not** open public issues for security disclosures.

## License

Apache-2.0. See [LICENSE](./LICENSE).
