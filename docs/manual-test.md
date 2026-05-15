# Manual end-to-end test (Epic 6 Story 6.3)

This is the procedure for verifying that the **standalone binary** and the **Caddy adapter** deliver identical email output for identical input — the load-bearing acceptance test for the two-deployment-shape parity claim.

Run this against a real Postmark account whenever:

- A change touches `core/gateway/`, `core/transport/`, `core/template/`, or `core/config/`
- A change touches `caddy/module.go` or `caddy/caddyfile.go`
- A Caddy version bump in `caddy/go.mod`
- Pre-tag verification for a v1.x release

CI does not automate this — Postmark sends cost cents and the live SaaS dependency makes it the wrong shape for a per-PR check. The unit tests in [`caddy/caddyfile_test.go`](../caddy/caddyfile_test.go) — specifically `TestParityWithTOML` — give continuous parity assurance at the config level. This procedure exercises the **full request pipeline** including the transport.

## Prerequisites

- A Postmark account with a verified sender signature (e.g., `noreply@example.com`)
- Postmark **server token** exported as `POSTMARK_API_KEY`
- An inbox you control to receive test mail (`you@yourdomain.com`)
- Go 1.25+ installed
- [`xcaddy`](https://github.com/caddyserver/xcaddy) installed (`go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest`)
- This repo checked out

## Test plan

Submit **the same** form payload to both deployment shapes, configured with identical settings, and verify that Postmark's dashboard shows two messages with matching subject, body, recipients, and `From` — the only differences should be `MessageID` and `SubmittedAt`.

## 1. Prepare shared fixtures

In the repo root:

```bash
mkdir -p /tmp/posthorn-manual
cd /tmp/posthorn-manual
```

Create `payload.txt`:

```bash
cat > payload.txt <<'EOF'
name=Manual Test
email=test@example.com
message=hello from the manual parity test
company=Posthorn QA
EOF
```

The trailing field (`company`) exercises the custom-fields passthrough block — it should appear in the rendered email body of both shapes.

## 2. Test the standalone binary

Create `standalone.toml`:

```toml
[[endpoints]]
path = "/api/contact"
to = ["you@yourdomain.com"]
from = "Posthorn Test <noreply@yourdomain.com>"
required = ["name", "email", "message"]
honeypot = "_gotcha"
subject = "[STANDALONE] {{.name}}"
body = """
From {{.name}} <{{.email}}>

{{.message}}
"""

[endpoints.transport]
type = "postmark"

[endpoints.transport.settings]
api_key = "${env.POSTMARK_API_KEY}"
```

Build and run:

```bash
cd <repo-root>
go build -o /tmp/posthorn-manual/posthorn ./core/cmd/posthorn
cd /tmp/posthorn-manual
./posthorn validate --config standalone.toml   # expect: exit 0
./posthorn serve --config standalone.toml &
SERVE_PID=$!
sleep 1

curl -sS -X POST http://localhost:8080/api/contact \
  -H "Content-Type: application/x-www-form-urlencoded" \
  --data-urlencode "name=Manual Test" \
  --data-urlencode "email=test@example.com" \
  --data-urlencode "message=hello from the manual parity test" \
  --data-urlencode "company=Posthorn QA"

# expect: HTTP 200 with {"status":"ok","submission_id":"..."}

kill $SERVE_PID
wait $SERVE_PID 2>/dev/null
```

Confirm the email arrives in your inbox with:

- Subject: `[STANDALONE] Manual Test`
- Body starts with `From Manual Test <test@example.com>`
- Body ends with an `Additional fields:` block containing `company: Posthorn QA`

Save the standalone submission's `submission_id` from the JSON response for the comparison step.

## 3. Test the Caddy adapter

Build a Caddy with the adapter:

```bash
cd <repo-root>/caddy
xcaddy build \
  --with github.com/craigmccaskill/posthorn/caddy=. \
  --with github.com/craigmccaskill/posthorn=../core
mv caddy /tmp/posthorn-manual/caddy-with-posthorn
cd /tmp/posthorn-manual
```

Verify the module is registered:

```bash
./caddy-with-posthorn list-modules | grep posthorn
# expect: http.handlers.posthorn
```

Create `Caddyfile`:

```caddyfile
{
    auto_https off
    admin off
}

:8081 {
    posthorn /api/contact {
        to you@yourdomain.com
        from "Posthorn Test <noreply@yourdomain.com>"
        required name email message
        honeypot _gotcha
        subject "[ADAPTER] {{.name}}"
        body "From {{.name}} <{{.email}}>\n\n{{.message}}"
        transport postmark {
            api_key {env.POSTMARK_API_KEY}
        }
    }
}
```

Note the only differences from the TOML:

- Listener on `:8081` (so it doesn't collide with the standalone if you keep that running)
- `[ADAPTER]` in the subject (so you can distinguish the two messages in your inbox)
- Body is a single-line escaped string (Caddyfile doesn't accept TOML triple-quoted strings)

Run it:

```bash
./caddy-with-posthorn validate --config Caddyfile --adapter caddyfile   # expect: exit 0
./caddy-with-posthorn run --config Caddyfile --adapter caddyfile &
CADDY_PID=$!
sleep 1

curl -sS -X POST http://localhost:8081/api/contact \
  -H "Content-Type: application/x-www-form-urlencoded" \
  --data-urlencode "name=Manual Test" \
  --data-urlencode "email=test@example.com" \
  --data-urlencode "message=hello from the manual parity test" \
  --data-urlencode "company=Posthorn QA"

# expect: HTTP 200 with {"status":"ok","submission_id":"..."}

kill $CADDY_PID
wait $CADDY_PID 2>/dev/null
```

## 4. Compare the two emails

Open both messages in your inbox. Side-by-side comparison checklist:

| Field | Standalone | Adapter | Must match? |
|---|---|---|---|
| From header | `Posthorn Test <noreply@yourdomain.com>` | same | **yes** |
| To header | `you@yourdomain.com` | same | **yes** |
| Subject text after the tag | `Manual Test` | `Manual Test` | **yes** |
| Subject tag | `[STANDALONE]` | `[ADAPTER]` | no — distinguishes the run |
| Body opening line | `From Manual Test <test@example.com>` | same | **yes** |
| Body second-half: message | `hello from the manual parity test` | same | **yes** |
| Additional fields block | contains `company: Posthorn QA` | same | **yes** |
| Postmark `MessageID` | unique | unique | no — Postmark assigns |
| Postmark `SubmittedAt` | unique | unique | no |
| Inferred SPF/DKIM pass | yes | yes | **yes** |

If every "must match" row matches, parity holds. Story 6.3 acceptance: met.

## 5. Spot-check the structured logs

In both runs, the JSON log line for `submission_sent` should include:

- `submission_id` (UUIDv4, different per submission)
- `endpoint: /api/contact`
- `transport: postmark`
- `latency_ms` (integer, typically 100-500 for a healthy Postmark response)

The standalone logs go to stdout directly. The adapter logs go through Caddy's logger (zap → slog bridge) — same structured fields, different formatter shape.

## 6. Tear down

```bash
cd /tmp
rm -rf posthorn-manual
```

## Known divergences (intentional)

These are **not** parity failures; they're properties of the two syntaxes:

| Property | Standalone (TOML) | Adapter (Caddyfile) |
|---|---|---|
| Env-var syntax | `${env.X}` | `{env.X}` |
| Multi-line body | triple-quoted string `"""..."""` | single-line `"...\n..."` or external file |
| Block delimiters | TOML tables `[endpoints.transport]` | Caddyfile blocks `transport postmark { ... }` |
| Comment syntax | `#` | `#` |
| `\n` escape in strings | expanded to newline | preserved literally |

The escape-sequence divergence is the one to be careful about — if your TOML body has `\n` (which becomes a newline), the equivalent Caddyfile must use an external template file or a multi-line string construction to produce the same output.
