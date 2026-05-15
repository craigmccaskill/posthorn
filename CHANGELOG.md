# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- HTTP form ingress with multiple independent endpoints per config (FR1, FR2)
- Postmark HTTP API transport with bespoke ~80-line client (FR3, FR4, ADR-1)
- Honeypot field, Origin/Referer fail-closed check, max body size, token-bucket rate limit with LRU eviction at 10K IPs (FR5-FR9, NFR4, NFR6)
- Required-field and email-format validation returning structured 422 (FR10, FR11)
- Go `text/template` rendering for subject and body with custom-fields passthrough block (FR12, FR13)
- JSON responses, content negotiation, `redirect_success` / `redirect_error` (FR14-FR16)
- One retry on transient/5xx after 1s, one retry on 429 after 5s, no retry on 4xx config errors, 10s hard request timeout (FR19-FR22)
- Structured JSON logging with UUIDv4 submission IDs propagated through every log line (FR17, FR18, NFR7, NFR8)
- Standalone binary `cmd/posthorn` with `serve` and `validate` subcommands, SIGTERM/SIGINT graceful shutdown (FR24-FR26)
- Multi-stage Dockerfile producing a distroless static image for `linux/amd64` and `linux/arm64`, published to `ghcr.io/craigmccaskill/posthorn` on tag push (NFR12, NFR13)
- GitHub Actions CI running `go vet` and `go test -race` across both workspace modules
- Caddy v2 adapter module at `github.com/craigmccaskill/posthorn/caddy` registering `http.handlers.posthorn`, with Caddyfile directive support and a TOML-parity unit test (FR27-FR30, NFR10)
- Public documentation site at [posthorn.dev](https://posthorn.dev) (Astro + Starlight)

## [1.0.0] — TBD

_Pending Story 7.3 tag. Contents will move from [Unreleased] when v1.0.0 is tagged._
