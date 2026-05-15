# Security policy

## Supported versions

| Version | Supported |
|---|---|
| 1.0.x | ✅ Active development |
| < 1.0  | ❌ Pre-release; no security support |

When v1.1 ships, the 1.0.x line moves to passive security-fix-only support for 6 months. v0.x pre-release builds are not supported.

## Reporting a vulnerability

**Please do not open public GitHub issues for security vulnerabilities.**

Use GitHub's [private vulnerability reporting](https://github.com/craigmccaskill/posthorn/security/advisories/new) — the "Report a vulnerability" button under the Security tab. That creates a private channel between you and the maintainer.

If for some reason that isn't available to you, email **craig.mccaskill+posthorn-security@gmail.com** instead.

### What to include

- A clear description of the issue and the impact you observed (or believe is achievable)
- Posthorn version (output of `posthorn version` or the Docker image tag)
- Deployment shape (Docker container / standalone binary)
- A minimal reproducer if you have one (sanitized config, sample request)
- Any logs or stack traces (with API keys redacted)

### Response timeline

- **48 hours** — acknowledgement of your report
- **7 days** — assessment of severity and whether the issue qualifies as a vulnerability in scope (see below)
- **30 days** — a fix and disclosure plan for confirmed vulnerabilities, in coordination with you

These are upper bounds; most reports get a same-day response.

### Coordinated disclosure

We follow a 90-day coordinated disclosure window from confirmation. If the issue is publicly disclosed before a fix is available — by anyone — we publish the advisory immediately and ship the fix as soon as it's ready.

If you'd like credit in the advisory, say so in the report. Anonymous reports are honored.

## What's in scope

These are real vulnerabilities and we want to hear about them:

- **Header injection** in outbound mail (NFR1, NFR2) — a way to make submitter-controlled input become an SMTP header (especially `Bcc:`, `From:`, header smuggling via CRLF)
- **API key leakage** (NFR3) — a way to make the configured Postmark token appear in log output, error responses, or other observable channels
- **Origin/Referer fail-open bypass** (FR6, NFR4) — submissions accepted that should have been rejected by the allowlist
- **Rate limit bypass** — a way to exceed `rate_limit` for a single client beyond the configured bucket capacity (note: per-IP keying, behind a misconfigured `trusted_proxies` is operator-misconfiguration territory, see below)
- **Honeypot signal leakage** — a way to distinguish an honeypot-triggered 200 from a genuine success without sending mail
- **DoS via the request pipeline** — a way to make a single request consume more than the documented bounds (10s, `max_body_size`)
- **Path traversal** in body-template file paths (templates load via relative paths from the config dir)
- **TLS termination assumption** failures — anything that makes plaintext-only deployments observably secure when they aren't

## What's not in scope

These are real concerns but they aren't Posthorn vulnerabilities:

- **Operator misconfiguration**: loose `trusted_proxies` (e.g., `0.0.0.0/0`), committed API keys to public repos, missing TLS at the reverse proxy, missing SPF/DKIM/DMARC on the sending domain, public-internet-bound `:8080` without auth in front
- **Upstream provider compromises**: if Postmark is breached, that's an upstream incident
- **Layer 4/7 DDoS**: that's the CDN/reverse-proxy layer's responsibility, not Posthorn's
- **Spam from many low-rate distributed IPs**: per-IP rate limiting doesn't catch this; coordinated mitigation lands in v3 (proof-of-work / captcha)
- **Issues in deprecated pre-v1.0 builds**: those aren't supported

## Security guarantees Posthorn enforces in code

These are not policies; they're **invariants the test suite asserts on every build**.

1. **Header injection prevention** (NFR1/NFR2): every submitter-controlled value reaches the outbound transport exclusively through JSON-encoded struct fields. There is no path in [`core/transport/postmark.go`](./core/transport/postmark.go) or in any other transport that constructs an SMTP header via string concatenation of submitter input.
2. **API keys never appear in log output** (NFR3): the test suite triggers transport failures with a known API-key string and asserts the string does not appear in captured log output. See [`core/transport/postmark_test.go`](./core/transport/postmark_test.go) and [`core/log/log_test.go`](./core/log/log_test.go).
3. **Origin/Referer fail-closed** (FR6, NFR4): when `allowed_origins` is configured and both `Origin` and `Referer` headers are absent, the request is rejected with 403. The config loader rejects explicitly-empty `allowed_origins = []` at parse time.
4. **Bounded request latency** (FR22, NFR5): every request runs under a hard 10-second `context.WithTimeout`. Any retry in flight is cancelled when the deadline fires.

Operator-facing summary of all four: [posthorn.dev/security](https://posthorn.dev/security/threat-model/).

## Public disclosure track record

Once we ship the first advisory, it'll be listed here with the affected versions and resolved-in versions. Empty for now.

| ID | Date | Summary | Affected | Fixed in |
|---|---|---|---|---|
| _(none yet)_ | | | | |
