# Manual end-to-end test

This is the procedure for verifying that a build of Posthorn delivers real mail end-to-end against a real Postmark account. It's the smoke test you run before tagging a release, after a Caddy/transport-relevant code change, or whenever you need to confirm that the in-process pipeline actually reaches an inbox.

CI does not automate this — Postmark sends cost cents and the live SaaS dependency makes it the wrong shape for a per-PR check. The unit tests give continuous assurance at the pipeline level; this procedure exercises the **full request flow** including the transport.

Run this when:

- A change touches `core/gateway/`, `core/transport/`, `core/template/`, or `core/config/`
- Pre-tag verification for a v1.x release
- After a Postmark or DNS configuration change you want to verify end-to-end

## Prerequisites

- A Postmark account with a verified sender signature (e.g., `noreply@example.com`)
- Postmark **server token** exported as `POSTMARK_API_KEY`
- An inbox you control to receive test mail (`you@yourdomain.com`)
- Go 1.25+ installed
- This repo checked out

## Procedure

### 1. Prepare a working directory

```bash
mkdir -p /tmp/posthorn-manual
cd /tmp/posthorn-manual
```

### 2. Write the config

```bash
cat > posthorn.toml <<'EOF'
[[endpoints]]
path = "/api/contact"
to = ["you@yourdomain.com"]
from = "Posthorn Test <noreply@yourdomain.com>"
required = ["name", "email", "message"]
honeypot = "_gotcha"
subject = "Manual test: {{.name}}"
body = """
From {{.name}} <{{.email}}>

{{.message}}
"""

[endpoints.transport]
type = "postmark"

[endpoints.transport.settings]
api_key = "${env.POSTMARK_API_KEY}"
EOF
```

Replace `you@yourdomain.com` and `noreply@yourdomain.com` with your real values.

### 3. Build and run

```bash
cd <repo-root>
go build -o /tmp/posthorn-manual/posthorn ./core/cmd/posthorn
cd /tmp/posthorn-manual
./posthorn validate --config posthorn.toml   # expect: exit 0
./posthorn serve --config posthorn.toml &
SERVE_PID=$!
sleep 1
```

### 4. Submit a real request

```bash
curl -sS -X POST http://localhost:8080/api/contact \
  -H "Content-Type: application/x-www-form-urlencoded" \
  --data-urlencode "name=Manual Test" \
  --data-urlencode "email=test@example.com" \
  --data-urlencode "message=hello from the manual end-to-end test" \
  --data-urlencode "company=Posthorn QA"
```

**Expect:** HTTP 200 with `{"status":"ok","submission_id":"..."}`.

Save the `submission_id` — you'll grep for it in the logs.

### 5. Confirm the email arrives

Open your inbox. Expect:

- Subject: `Manual test: Manual Test`
- Body starts with `From Manual Test <test@example.com>`
- Body ends with an `Additional fields:` block containing `company: Posthorn QA` (custom-fields passthrough)
- `Authentication-Results:` header shows `spf=pass`, `dkim=pass` (proves SPF/DKIM are set up correctly on your sending domain)

### 6. Confirm the structured log

```bash
# Process is still running in the background; logs are on its stdout.
# Look for the submission_id from step 4.
```

Expect a JSON line shaped like:

```json
{"time":"...","level":"INFO","msg":"submission_sent","submission_id":"...","endpoint":"/api/contact","transport":"postmark","latency_ms":312,"transport_message_id":"<postmark id>"}
```

### 7. Honeypot smoke (no email sent)

```bash
curl -sS -X POST http://localhost:8080/api/contact \
  -H "Content-Type: application/x-www-form-urlencoded" \
  --data-urlencode "name=Bot" \
  --data-urlencode "email=spam@example.com" \
  --data-urlencode "message=spam content" \
  --data-urlencode "_gotcha=trap-fired"
```

**Expect:** HTTP 200 with `{"status":"ok","submission_id":"..."}` — the body looks identical to a real success (FR5/NFR5 honeypot indistinguishability). Mail is **not** sent.

In the log, search for the new submission_id — you'll see `spam_blocked` instead of `submission_sent`.

### 8. Tear down

```bash
kill $SERVE_PID
wait $SERVE_PID 2>/dev/null
cd /tmp
rm -rf posthorn-manual
```

## Pass criteria

- Real submission: 200 response, email arrives, `submission_sent` log line
- Honeypot submission: 200 response with same body shape, no email, `spam_blocked` log line with `kind: honeypot`
- No `Bcc:`, no smuggled headers in the delivered message (header-injection invariant)
- SPF + DKIM both pass at the recipient

If everything above holds, the build is ready for tagging.

## v1.1: API mode procedure

Run this in addition to the form-mode procedure above when validating a v1.1 build. It exercises the auth, JSON ingress, and idempotency paths against a real Postmark account.

### 1. Extend the config

Append an API-mode endpoint to the same `posthorn.toml`:

```bash
cat >> posthorn.toml <<'EOF'

[[endpoints]]
path = "/api/transactional"
to = ["you@yourdomain.com"]
from = "Posthorn API Test <noreply@yourdomain.com>"
required = ["subject_line", "message"]
auth = "api-key"
api_keys = ["${env.WORKER_KEY}"]
subject = "{{.subject_line}}"
body = """
{{.message}}
"""

[endpoints.transport]
type = "postmark"

[endpoints.transport.settings]
api_key = "${env.POSTMARK_API_KEY}"
EOF
```

### 2. Restart with both env vars

```bash
kill $SERVE_PID 2>/dev/null
export WORKER_KEY="$(uuidgen)"   # one-shot test key
./posthorn validate --config posthorn.toml   # expect: 2 endpoint(s)
./posthorn serve --config posthorn.toml &
SERVE_PID=$!
sleep 1
```

### 3. Submit a JSON request

```bash
curl -sS -X POST http://localhost:8080/api/transactional \
  -H "Authorization: Bearer $WORKER_KEY" \
  -H "Content-Type: application/json" \
  --data '{
    "subject_line": "v1.1 API mode test",
    "message": "Sent via api-mode endpoint with JSON body.",
    "request_id": "manual-test-1"
  }'
```

**Expect:** HTTP 200 with `{"status":"ok","submission_id":"..."}` and an email arrives with subject `v1.1 API mode test`. The `Additional fields:` block in the body should include `request_id: manual-test-1`.

### 4. Auth failure check

```bash
curl -sS -o /dev/null -w 'HTTP %{http_code}\n' -X POST http://localhost:8080/api/transactional \
  -H "Authorization: Bearer not-the-real-key" \
  -H "Content-Type: application/json" \
  --data '{"subject_line":"x","message":"y"}'
```

**Expect:** `HTTP 401`. Then grep the server log for `auth_failed` — the line should be present, and the string `not-the-real-key` should **not** appear anywhere in the log output (NFR21 invariant).

### 5. Idempotency replay

```bash
IDEM_KEY="$(uuidgen)"

# First request: 200, email arrives.
curl -sS -X POST http://localhost:8080/api/transactional \
  -H "Authorization: Bearer $WORKER_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $IDEM_KEY" \
  --data '{"subject_line":"Idempotency test","message":"First send"}'

# Second request: same key, same body. Must NOT send another email.
curl -sS -X POST http://localhost:8080/api/transactional \
  -H "Authorization: Bearer $WORKER_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $IDEM_KEY" \
  --data '{"subject_line":"Idempotency test","message":"First send"}'
```

**Expect:**

- Both calls return identical JSON bodies (same `submission_id`)
- Only **one** email arrives in your inbox
- Server log shows `idempotent_replay` on the second call

### 6. Content-type rejection

```bash
curl -sS -o /dev/null -w 'HTTP %{http_code}\n' -X POST http://localhost:8080/api/transactional \
  -H "Authorization: Bearer $WORKER_KEY" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  --data 'subject_line=test&message=test'
```

**Expect:** `HTTP 415`. Form-encoded bodies on api-mode endpoints are rejected.

### 6a. Per-request recipient via `to_override`

```bash
# Override the endpoint's `to` list with a single recipient.
curl -sS -X POST http://localhost:8080/api/transactional \
  -H "Authorization: Bearer $WORKER_KEY" \
  -H "Content-Type: application/json" \
  --data '{
    "to_override": "craig.mccaskill@gmail.com",
    "subject_line": "to_override test",
    "message": "Sent via to_override field"
  }'

# Invalid email → 422.
curl -sS -o /dev/null -w 'HTTP %{http_code}\n' -X POST http://localhost:8080/api/transactional \
  -H "Authorization: Bearer $WORKER_KEY" \
  -H "Content-Type: application/json" \
  --data '{"to_override":"not-an-email","subject_line":"x","message":"y"}'
```

**Expect:**

- First call: HTTP 200; email arrives at the `to_override` address (not the endpoint's configured `to`)
- Second call: HTTP 422 with `to_override` named in the error body

### 7. Tear down

```bash
kill $SERVE_PID
wait $SERVE_PID 2>/dev/null
unset WORKER_KEY POSTMARK_API_KEY
cd /tmp
rm -rf posthorn-manual
```

### API-mode pass criteria

- Valid JSON request: 200, email arrives with `Additional fields:` carrying custom keys
- 401 on wrong/missing Bearer; sentinel key absent from logs
- Idempotent replay: same `submission_id`, only one email delivered, `idempotent_replay` log line
- 415 on form-encoded body
- `to_override` with valid email: 200, email arrives at the override address
- `to_override` with invalid email: 422 naming `to_override`
- `submission_sent` log line includes `transport_message_id`

## Non-Postmark transports

The procedure above uses the Postmark transport. The other four transports (Resend, Mailgun, AWS SES, outbound SMTP) ship as alternatives in the same Posthorn binary. To validate each, swap the `[endpoints.transport]` block in the test config and re-run the procedure. Everything outside the transport block — curl invocations, expected response shape, log line shape, validation behavior — is unchanged.

Each requires its own provider account, API credentials, and DNS setup. The sentinel-key NFR3 invariant applies to every transport: a failed-auth path must not surface the configured key value in captured logs.

### Resend

```toml
[endpoints.transport]
type = "resend"

[endpoints.transport.settings]
api_key = "${env.RESEND_API_KEY}"
```

Expected `submission_sent` log line: `transport_message_id` populated from Resend's response `id` field (UUID shape).

### Mailgun

```toml
[endpoints.transport]
type = "mailgun"

[endpoints.transport.settings]
api_key = "${env.MAILGUN_API_KEY}"
domain  = "mg.yourdomain.com"
# Add base_url = "https://api.eu.mailgun.net" if your account is in the EU region.
```

Expected `transport_message_id`: angle-bracketed Message-ID shape (`<msg-id@yourdomain.com>`).

### AWS SES

```toml
[endpoints.transport]
type = "ses"

[endpoints.transport.settings]
access_key_id     = "${env.AWS_ACCESS_KEY_ID}"
secret_access_key = "${env.AWS_SECRET_ACCESS_KEY}"
region            = "us-east-1"
```

**Pre-test checklist:**
- IAM user/role has `ses:SendEmail` for the verified sender identity
- Account is **out of sandbox** (or the test recipient is verified)
- DKIM CNAMEs published; SPF includes `amazonses.com`

Expected `transport_message_id`: SES UUID shape.

**Specific NFR3 check:** trigger a `403 SignatureDoesNotMatch` by deliberately using a wrong secret. Confirm the secret string does not appear in any log line.

### Outbound SMTP

```toml
[endpoints.transport]
type = "smtp"

[endpoints.transport.settings]
host        = "smtp.mailgun.org"
port        = 587
username    = "${env.SMTP_USERNAME}"
password    = "${env.SMTP_PASSWORD}"
require_tls = true
```

Expected `transport_message_id`: **empty** — stdlib `net/smtp` doesn't expose the upstream's `queued as <id>` response. Correlate by submission_id timestamp + recipient against the relay's own logs.

**Specific NFR1 check:** trigger header injection by attempting to send with `subject = "Hello\r\nBcc: victim@target.com"` (via a JSON request body that includes the CRLF). Expected: HTTP 502 from Posthorn, terminal-failure log line `smtp: header injection attempt rejected`, **no SMTP connection ever opened** (no entry in the relay's logs).

### Cross-transport pass criteria

For each non-Postmark transport tested:

- Valid request: 200, email arrives, `submission_sent` log line with `transport: <type>`
- Forced auth failure (deliberately wrong key/credentials): 502 from Posthorn, sentinel value not present in log output
- Forced bad recipient: 502 + terminal-failure log line; for SES/Mailgun, the upstream's error message surfaces in `error` field
- Same `Additional fields:` block content as the Postmark procedure

## SMTP ingress procedure

Run this when validating a build that includes `[smtp_listener]`. It exercises the inbound SMTP listener end-to-end against a real internal client (e.g., `swaks`) and a real outbound transport.

### 1. Generate a test cert (or use one you already have)

```bash
openssl req -x509 -newkey rsa:2048 -keyout /tmp/posthorn-key.pem \
  -out /tmp/posthorn-cert.pem -days 1 -nodes \
  -subj "/CN=localhost" -addext "subjectAltName=DNS:localhost,IP:127.0.0.1"
```

### 2. Extend the config

```bash
cat >> posthorn.toml <<'EOF'

[smtp_listener]
listen          = "127.0.0.1:2525"
require_tls     = true
tls_cert        = "/tmp/posthorn-cert.pem"
tls_key         = "/tmp/posthorn-key.pem"
auth_required   = "smtp-auth"
allowed_senders = ["*@craigmccaskill.com"]
max_recipients_per_session = 5
max_message_size = "1MB"

[[smtp_listener.smtp_users]]
username = "testapp"
password = "${env.SMTP_USER_PASSWORD}"

[smtp_listener.transport]
type = "postmark"

[smtp_listener.transport.settings]
api_key = "${env.POSTMARK_API_KEY}"
EOF
```

### 3. Restart Posthorn

```bash
export SMTP_USER_PASSWORD="$(uuidgen)"
kill $SERVE_PID 2>/dev/null
./posthorn validate --config posthorn.toml   # expect: parses OK
./posthorn serve --config posthorn.toml &
SERVE_PID=$!
sleep 1
```

You should see the log line `smtp_listener registered listen=127.0.0.1:2525`.

### 4. Send a message via `swaks`

```bash
swaks --server 127.0.0.1:2525 \
  --tls --tls-verify=no \
  --auth-user testapp --auth-password "$SMTP_USER_PASSWORD" \
  --from "infra@craigmccaskill.com" \
  --to "craig.mccaskill@gmail.com" \
  --header "Subject: SMTP ingress test" \
  --body "Sent through Posthorn's SMTP listener."
```

**Expect:** every step in the swaks transcript returns the right code (220 greeting, 250-EHLO, 220 STARTTLS, 235 AUTH OK, 250 MAIL OK, 250 RCPT OK, 354 DATA OK, 250 OK queued, 221 Bye). Email arrives via Postmark within seconds.

### 5. Confirm in the Posthorn log

```
smtp_session_open       remote_addr=127.0.0.1:NNNNN
smtp_tls_established
smtp_auth_ok            user=testapp
smtp_submission_sent    submission_id=<uuid> transport_message_id=<postmark-id>
smtp_session_close      reason=quit
```

### 6. Open-relay defense checks

```bash
# Sender not in allowlist → 550
swaks --server 127.0.0.1:2525 --tls --tls-verify=no \
  --auth-user testapp --auth-password "$SMTP_USER_PASSWORD" \
  --from "intruder@evil.com" --to "craig.mccaskill@gmail.com" \
  --header "Subject: x" --body "x" 2>&1 | grep "550"

# Wrong password → 535
swaks --server 127.0.0.1:2525 --tls --tls-verify=no \
  --auth-user testapp --auth-password "wrong" \
  --from "infra@craigmccaskill.com" --to "craig.mccaskill@gmail.com" \
  --header "Subject: x" --body "x" 2>&1 | grep "535"

# AUTH attempt before STARTTLS → 530
swaks --server 127.0.0.1:2525 \
  --auth-user testapp --auth-password "$SMTP_USER_PASSWORD" \
  --from "infra@craigmccaskill.com" --to "craig.mccaskill@gmail.com" \
  --header "Subject: x" --body "x" 2>&1 | grep "530"
```

### 7. NFR22 header-injection check

A malicious DATA blob with a `Bcc:` header in the MIME body must NOT result in a Bcc'd email. Send through `swaks --header "Subject: Hi" --add-header "Bcc: victim@target.com"` and verify only one email arrives — to the RCPT TO recipient, not the smuggled MIME Bcc.

### SMTP ingress pass criteria

- Authenticated TLS-only AUTH PLAIN → 235 → MAIL/RCPT/DATA → 250 → email arrives via Postmark
- Sender not in allowlist → 550 5.7.1
- Wrong password → 535 5.7.8
- AUTH attempt before STARTTLS → 530 5.7.0
- Recipient cap (set low; test with N+1 RCPT TO) → 452 4.5.3
- MIME `Bcc:` header smuggled in DATA does NOT cause delivery to the smuggled address
- `smtp_submission_sent` log line carries `submission_id` + `transport_message_id` (when upstream provides one)
