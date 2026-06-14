# Calling Posthorn from your app

Posthorn's HTTP API is a single authenticated JSON POST. You do not need a
client library; here it is in curl, Python, and JavaScript with no
dependencies.

The endpoint path and required fields come from your config (the `path` and
`required` list on each endpoint). These examples use the `/api/transactional`
endpoint from the [README](../README.md), which requires `subject_line` and
`message`.

## curl

```bash
curl -X POST https://posthorn.yourdomain.com/api/transactional \
  -H "Authorization: Bearer $WORKER_KEY_PRIMARY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: reset:user-123" \
  --data '{
    "to_override": "alice@example.com",
    "subject_line": "Reset your password",
    "message": "Click here: https://app.example.com/reset/abc"
  }'
```

## Python (standard library)

```python
import json
import os
import urllib.request

req = urllib.request.Request(
    "https://posthorn.yourdomain.com/api/transactional",
    method="POST",
    headers={
        "Authorization": f"Bearer {os.environ['WORKER_KEY_PRIMARY']}",
        "Content-Type": "application/json",
        "Idempotency-Key": "reset:user-123",
    },
    data=json.dumps({
        "to_override": "alice@example.com",
        "subject_line": "Reset your password",
        "message": "Click here: https://app.example.com/reset/abc",
    }).encode(),
)
with urllib.request.urlopen(req) as resp:
    print(resp.status, json.load(resp))  # 200 {"status": ..., "submission_id": ...}
```

## JavaScript (fetch, no dependencies)

Works in Node 18+, Cloudflare Workers, Deno, and the browser.

```javascript
const resp = await fetch("https://posthorn.yourdomain.com/api/transactional", {
  method: "POST",
  headers: {
    "Authorization": `Bearer ${process.env.WORKER_KEY_PRIMARY}`,
    "Content-Type": "application/json",
    "Idempotency-Key": "reset:user-123",
  },
  body: JSON.stringify({
    to_override: "alice@example.com",
    subject_line: "Reset your password",
    message: "Click here: https://app.example.com/reset/abc",
  }),
});
console.log(resp.status, await resp.json()); // 200 { status, submission_id }
```

## Notes

- **Recipients:** `to_override` (optional) sets the recipient for this one
  request. Without it, the endpoint's configured `to` is used. The sender is
  always fixed by config and cannot be overridden per request.
- **Idempotency:** `Idempotency-Key` (optional) makes retries safe. A duplicate
  request in flight for the same key returns `409`.
- **Success:** `200` with a JSON body of `{"status": ..., "submission_id": ...}`.
- **Errors:** `401` auth, `409` duplicate idempotency key, `422` validation
  (JSON body lists the offending fields), `429` rate limited, `502` upstream
  transport failure.
