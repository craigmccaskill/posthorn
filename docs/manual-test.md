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
