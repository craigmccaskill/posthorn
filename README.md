# Posthorn

[![CI](https://github.com/craigmccaskill/posthorn/actions/workflows/ci.yml/badge.svg)](https://github.com/craigmccaskill/posthorn/actions/workflows/ci.yml)
[![Docs](https://img.shields.io/badge/docs-posthorn.dev-d4a300)](https://posthorn.dev)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](./LICENSE)
[![Go version](https://img.shields.io/badge/go-1.25%2B-00ADD8.svg)](https://go.dev)

A self-hosted email gateway for cloud platforms that block outbound SMTP. Accepts mail through HTTP form ingress and delivers it via Postmark's HTTP API.

> *Not related to [PostHog](https://posthog.com). PostHog is product analytics. Posthorn is an email gateway. Different categories, zero functional overlap.*

## Why

DigitalOcean, AWS Lightsail, Linode, Vultr, and most cloud hosts block outbound SMTP on ports 25, 465, and 587 by default. The block is policy, not configurable — providers explicitly recommend an HTTP API service like Postmark, Resend, or Mailgun instead.

That breaks two patterns at once: web forms that send email (contact forms, signups) and self-hosted apps that emit SMTP for transactional mail (Ghost, Gitea, Mastodon, et al. for admin emails and magic links).

The current options are bad: pay for SaaS form services, rewrite app configs to use API SDKs (rarely supported), run Postfix as a relay with custom HTTP glue, or move to a host that doesn't block SMTP. There is no actively maintained, self-hosted, HTTP-API-first email gateway in 2026.

Posthorn is the bridge.

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
| **v1.1** | Resend, Mailgun, AWS SES, outbound-SMTP transports; CSRF + time-based token spam protection; dry run; health check; Prometheus metrics |
| **v1.2** | **SMTP ingress** — TCP listener accepting SMTP from internal apps (Ghost, Gitea, Mastodon) and forwarding via the configured HTTP API transport |
| **v2** | SQLite submission log, retry queue across restarts, file attachments, webhook transport |
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
