# Security policy

## Supported versions

| Version | Supported |
|---|---|
| 1.0.x | ✅ Current stable |
| < 1.0  | ❌ Pre-release; no security support |

When the next minor or major release ships, the previous stable line moves to passive security-fix-only support for 6 months. v0.x pre-release builds are not supported.

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

These are real vulnerabilities and we want to hear about them.

**Form mode**

- **Header injection** in outbound mail (NFR1, NFR2) — a way to make submitter-controlled input become an SMTP header (especially `Bcc:`, `From:`, header smuggling via CRLF)
- **Origin/Referer fail-open bypass** (FR6, NFR4) — submissions accepted that should have been rejected by the allowlist
- **Rate limit bypass** — a way to exceed `rate_limit` for a single client beyond the configured bucket capacity (note: per-IP keying behind a misconfigured `trusted_proxies` is operator-misconfiguration territory, see below)
- **Honeypot signal leakage** — a way to distinguish an honeypot-triggered 200 from a genuine success without sending mail
- **CSRF token forgery** (FR57, ADR-16) — a way to produce a valid HMAC token without knowledge of `csrf_secret`, or to extend a token's validity past `csrf_token_ttl`

**API mode**

- **Bearer auth bypass** (FR33, NFR19) — a way to authenticate without a matching `api_keys` entry, including timing oracles against the constant-time compare
- **Per-IP brute-force lockout bypass** — a way to make repeated auth failures from one IP not consume the lockout budget
- **Idempotency-key tampering** (FR40–FR44, NFR20) — a way to retrieve a cached response without presenting the original key, or to poison the cache so a future caller's request returns someone else's response
- **`to_override` abuse** (FR46, ADR-11) — a way to make `to_override` accept malformed addresses, or any path that allows per-request override of `from` (intentionally forbidden)

**SMTP listener**

- **MIME-header recipient smuggling** (NFR22) — a way to make a recipient address in inbound MIME `To:`/`Cc:`/`Bcc:` headers end up as an outbound `RCPT TO`. Recipients must come from the SMTP envelope only.
- **Sender allowlist bypass** (FR64) — a way to authenticate (or in `auth = "none"` mode, connect from outside the trust zone) and emit mail with a `MAIL FROM` not on the `allowed_senders` list
- **AUTH PLAIN credential leakage** (NFR23) — a way to make a `smtp_users[*].password` value appear in log output on auth failure
- **STARTTLS bypass / downgrade** (FR67) — a way to issue `AUTH` or `MAIL` on a plaintext connection when `require_tls = true`

**Shared**

- **API key leakage** (NFR3, NFR21) — a way to make any configured provider credential (`POSTMARK_API_KEY`, `RESEND_API_KEY`, Mailgun key, AWS access key, outbound-SMTP password) appear in log output, error responses, metric labels, or other observable channels. The invariant applies to all five transports.
- **Submitter content in `/metrics` labels** (NFR24) — a way to make request-side strings (recipient address, subject, body fragments) appear as Prometheus label values
- **DoS via the request pipeline** — a way to make a single request consume more than the documented bounds (`max_body_size`, 10s context timeout, SMTP `max_message_size`)
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

1. **Header injection prevention** (NFR1/NFR2): every submitter-controlled value reaches outbound transports exclusively through structured fields (JSON for Postmark / Resend / SES; multipart for Mailgun; the `net/mail` library's escaped headers for outbound-SMTP). No transport constructs an SMTP header via string concatenation of submitter input. Asserted by per-transport header-injection tests covering CRLF in name/email/subject/recipients and smuggled `Bcc:`.
2. **SMTP envelope-only recipients** (NFR22): the inbound SMTP listener builds outbound `transport.Message.To` from `RCPT TO` envelope values only. Inbound MIME `To:`/`Cc:`/`Bcc:` headers in the DATA payload are stripped from recipient resolution. A malicious DATA blob cannot add recipients.
3. **API keys never appear in log output** (NFR3, NFR21): the test suite triggers failure paths with a known sentinel API-key string and asserts the sentinel does not appear in captured log output. This is verified for every transport (Postmark, Resend, Mailgun, AWS SES, outbound-SMTP) and for the API-mode endpoint `api_keys`, CSRF `csrf_secret`, and `smtp_users` passwords.
4. **Origin/Referer fail-closed** (FR6, NFR4): when `allowed_origins` is configured and both `Origin` and `Referer` headers are absent, the request is rejected with 403. The config loader rejects explicitly-empty `allowed_origins = []` at parse time.
5. **Mode/defense mutex at config parse** (ADR-10): API-mode endpoints reject `honeypot`, `allowed_origins`, `redirect_success`, `redirect_error`, and `csrf_secret` at parse time. Prevents an operator from configuring a defense they think is active but isn't.
6. **No submitter content in metrics labels** (NFR24): `/metrics` label values come only from operator-configured names (endpoint paths, transport types, error-class enum). Structurally enforced by the `metrics.Recorder` API not accepting request-side values; verified by tests.
7. **Bounded request latency** (FR22, NFR5): every HTTP request runs under a hard 10-second `context.WithTimeout`. Any retry in flight is cancelled when the deadline fires. SMTP sessions have their own per-session timeout bound.

Operator-facing summary: [posthorn.dev/security](https://posthorn.dev/security/threat-model/).

## Public disclosure track record

Once we ship the first advisory, it'll be listed here with the affected versions and resolved-in versions. Empty for now.

| ID | Date | Summary | Affected | Fixed in |
|---|---|---|---|---|
| _(none yet)_ | | | | |
