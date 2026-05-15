# Contributing to Posthorn

Thanks for your interest. This guide covers what you need to build, test, and contribute.

## Scope

The v1.0 specification is **locked**. The full requirements live in [`spec/`](./spec/) across three documents:

1. [`spec/01-project-brief.md`](./spec/01-project-brief.md) — problem, users, scope, threat model, risks
2. [`spec/02-prd.md`](./spec/02-prd.md) — functional and non-functional requirements, epic and story breakdown
3. [`spec/03-architecture.md`](./spec/03-architecture.md) — file layout, lifecycle, request flow, component design, ADRs

Contributions outside the locked v1.0 scope (SMTP ingress, additional transports beyond Postmark, CSRF tokens, file attachments, webhook transport, SQLite, admin UI) should wait for v1.1+ planning. The canonical "out of scope" list is in [`spec/01-project-brief.md`](./spec/01-project-brief.md) §"MVP Scope > Out of scope". If you're unsure, open an issue before writing code.

The architecture doc's [Architectural decisions log](./spec/03-architecture.md#architectural-decisions-log) records the ADRs that pin the structure. To deviate from any of them, update the architecture doc with the new decision and rationale before changing code.

## Prerequisites

- Go 1.25+
- A Postmark account for end-to-end testing (a sandbox token is sufficient)
- Docker (optional, for testing the container deployment)

## Repository layout

Posthorn is a single Go module:

- [`core/`](./core/) — the gateway, the `cmd/posthorn` binary, all the business logic.
- [`spec/`](./spec/) — the locked v1.0 specification.
- [`docs/`](./docs/) — operator-facing documentation that lives in-repo. The public site source is in [`site/`](./site/) and ships to [posthorn.dev](https://posthorn.dev).
- [`site/`](./site/) — Astro + Starlight source for the docs site.

## Build and test

```bash
# Run the test suite
cd core && go test -race ./...

# Build the binary
go build -o posthorn ./cmd/posthorn
./posthorn version

# Build the docs site
cd site && npm ci && npm run build
```

CI runs `go vet ./...` and `go test -race -count=1 -timeout=2m ./...` on every push and pull request. See [`.github/workflows/ci.yml`](./.github/workflows/ci.yml).

## End-to-end smoke test

Whenever you touch `core/gateway/`, `core/transport/`, `core/template/`, or `core/config/`, run the [manual end-to-end test](./docs/manual-test.md) against a real Postmark account before opening a PR. The unit tests cover config and pipeline behavior; the manual procedure exercises the full request pipeline through the transport and confirms mail actually delivers.

## Commit conventions

- Tag each commit with the story ID it implements, e.g. `feat(gateway): retry policy on transient transport errors (Story 4.1)`
- Prefixes: `feat:` new functionality, `fix:` bug fixes, `test:` test-only changes, `docs:` documentation, `chore:` build/config/CI
- Reference the relevant FR or NFR in the commit body when it adds clarity (e.g., "Implements NFR1 — header injection prevention via structured JSON API")
- Don't squash stories into a single commit — each story should be at least one commit so the git history maps to the PRD

## Updating the spec

If implementation reveals something the spec missed, update the relevant doc in `spec/` and reference the change in the commit that exposes it. The spec is the source of truth for v1.0 work; pull requests that change behavior without a corresponding spec update will be sent back.

## Security

This codebase handles untrusted input from public form submissions and credentials for an outbound email API. Security-relevant changes — header construction, API key handling, rate limiting, input validation, fail-closed origin checks — require explicit test coverage per the security NFRs in [`spec/02-prd.md`](./spec/02-prd.md) (NFR1 through NFR4).

For vulnerability reporting, see [SECURITY.md](./SECURITY.md). **Do not open public GitHub issues for security vulnerabilities.**

## Questions

Open a GitHub issue or start a discussion. For implementation questions, [`spec/03-architecture.md`](./spec/03-architecture.md) is the source of truth; for scoping questions, [`spec/01-project-brief.md`](./spec/01-project-brief.md).
