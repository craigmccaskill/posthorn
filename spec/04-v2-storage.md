---
title: "Spec: v2 Storage Foundation"
status: draft
created: 2026-05-15
applies_to: v2.0.0
---

# Spec: v2 Storage Foundation

This document specifies the v2 storage layer — the SQLite-backed persistence that underpins suppression lists, durable idempotency, retry queues across restarts, lifecycle event callbacks, automatic unsubscribe injection, and submission logging. It is the foundation for **five separate v2 features** that all need durability.

Open issues that derive from this spec:

- [#21 storage: SQLite submission log + retry queue across restarts](https://github.com/craigmccaskill/posthorn/issues/21) — this spec is the design for it
- [#22 storage: durable idempotency cache](https://github.com/craigmccaskill/posthorn/issues/22) — uses the `idempotency` table defined here
- [#4 Suppression list](https://github.com/craigmccaskill/posthorn/issues/4) — uses the `suppressions` table
- [#8 Lifecycle callbacks](https://github.com/craigmccaskill/posthorn/issues/8) — writes to `suppressions` (auto on bounces) and reads from `submissions` to correlate
- [#9 Unsubscribe link injection](https://github.com/craigmccaskill/posthorn/issues/9) — uses the `unsubscribe_tokens` table

Anything that touches v2 storage cites this spec for the schema, interfaces, and migration story so we don't end up with five inconsistent designs.

## Scope

**In scope:**

- Storage interface and SQLite implementation
- Schema for `submissions`, `retry_queue`, `suppressions`, `idempotency`, `unsubscribe_tokens`
- Migration system (forward-only, embedded SQL files, sequential)
- Retry queue worker (background loop pulling due retries)
- Idempotency cache (v2 implementation replacing v1.1 in-memory)
- Configuration surface (`[storage]` block in TOML)
- Test architecture for storage-touching code

**Out of scope (separate specs / issues):**

- The user-facing semantics of suppression / unsubscribe / lifecycle callbacks themselves — those are in the respective issue design reviews
- File attachment storage (uses the same SQLite as metadata; binary content as BLOB or filesystem TBD in #24's review)
- Multi-output fan-out (#25) doesn't need new tables; uses `submissions` for state

## Goals

- **Submissions survive restart.** A submission accepted by Posthorn is persisted before any external send. If Posthorn is killed mid-send, the submission state is recoverable from the database.
- **Failed sends retry across restarts.** v1.x retries within the same request. v2 adds out-of-request retry: failed transports surface 502 to the caller, the submission moves to the retry queue, a background worker retries with exponential backoff for up to ~4 days.
- **Idempotency keys are durable.** The v1.1 in-memory cache is replaced with a SQLite-backed cache. Same interface, same TTL semantics, persistent across restarts.
- **Suppression and unsubscribe state persists.** A bounced address remains suppressed; an unsubscribe click survives a restart.

## Non-goals

- **Distributed deployments.** v2 is single-process. Multi-instance Posthorn with shared storage is a v3+ concern (with all the complexity that brings — sharded retry workers, leader election, distributed locks).
- **Analytics queries / dashboards.** The `submissions` table is for operator forensics (`SELECT * WHERE endpoint=X AND status='failed' AND created_at > ...` via the `sqlite3` CLI). v3+ admin UI may add a query surface.
- **PII redaction at storage.** Submissions are stored as-is. Operators wanting redaction filter at intake (post-v1.0 feature) or omit `log_failed_submissions = true` semantically — the storage layer trusts what the handler hands it.

## Design decisions

### SQLite driver: `modernc.org/sqlite` (pure-Go)

Two real choices: `mattn/go-sqlite3` (cgo) and `modernc.org/sqlite` (pure-Go transpiled from SQLite C).

| Aspect | `mattn/go-sqlite3` | `modernc.org/sqlite` |
|---|---|---|
| Performance | Native C; ~2x faster on heavy reads | Slower; close-enough for Posthorn workload |
| Build | Requires cgo, gcc/clang | Pure Go; works with `CGO_ENABLED=0` |
| Dockerfile compatibility | Breaks current distroless-static build | Works as-is |
| Maturity | de-facto standard | maintained, widely used in cgo-averse projects |
| Cross-compilation | Painful (need C toolchain per arch) | Trivial (Go cross-compile) |
| Binary size | Smaller (native C, dynamic link) | Larger (transpiled C is a lot of Go code) |

**Decision: `modernc.org/sqlite`.**

Rationale: the v1.0 Dockerfile uses `CGO_ENABLED=0` and ships to `distroless/static:nonroot`. Switching to cgo would either require leaving distroless (a regression in attack surface) or maintaining two Dockerfile variants. The performance delta is negligible at Posthorn's expected workload — per-request inserts, not analytical scans. Submissions volume is in the thousands/day per instance, not millions.

This aligns with ADR-1's preference for minimal external dependencies (one Go module added) and preserves the existing static-binary distribution model.

### Schema (initial migration `001_baseline.sql`)

```sql
-- All timestamps are unix nano (INTEGER), UTC. Bytes stored as BLOB.

CREATE TABLE submissions (
    id                    TEXT PRIMARY KEY,         -- UUIDv4 submission_id
    endpoint              TEXT NOT NULL,
    transport_type        TEXT NOT NULL,            -- "postmark", "resend", etc.
    payload_json          BLOB NOT NULL,            -- parsed submission as canonical JSON
    headers_json          BLOB NOT NULL,            -- relevant headers (Origin, User-Agent, etc.)
    created_at            INTEGER NOT NULL,         -- unix nano
    sent_at               INTEGER,                  -- unix nano; NULL if not yet sent
    status                TEXT NOT NULL,            -- "pending" | "sent" | "failed" | "failed_permanently" | "suppressed"
    last_error            TEXT,                     -- most recent error text, if any
    transport_message_id  TEXT                      -- upstream MessageID from transport
);

CREATE INDEX idx_submissions_endpoint_created
    ON submissions(endpoint, created_at);

CREATE INDEX idx_submissions_status
    ON submissions(status)
    WHERE status IN ('pending', 'failed');

-- Retry queue. One row per submission awaiting retry; deleted on
-- successful retry or move to failed_permanently.
CREATE TABLE retry_queue (
    submission_id    TEXT PRIMARY KEY REFERENCES submissions(id) ON DELETE CASCADE,
    attempt          INTEGER NOT NULL,              -- 1, 2, 3, ...
    next_attempt_at  INTEGER NOT NULL,              -- unix nano of next due retry
    last_error       TEXT
);

CREATE INDEX idx_retry_queue_due ON retry_queue(next_attempt_at);

-- Suppression list. Hard bounces, spam complaints, manual entries.
-- Per-endpoint suppression (endpoint != NULL) takes precedence over
-- global (endpoint = NULL).
CREATE TABLE suppressions (
    email             TEXT NOT NULL,
    endpoint          TEXT,                          -- NULL = global; specific = per-endpoint
    reason            TEXT NOT NULL,                 -- "hard_bounce" | "complaint" | "manual" | "unsubscribe"
    suppressed_at     INTEGER NOT NULL,
    source_message_id TEXT,                          -- upstream MessageID that caused suppression
    PRIMARY KEY (email, COALESCE(endpoint, ''))
);

CREATE INDEX idx_suppressions_email ON suppressions(email);

-- Durable idempotency cache (v2 version of v1.1 in-memory cache).
CREATE TABLE idempotency (
    api_key_hash             BLOB NOT NULL,         -- sha256 of api_key
    idempotency_key          TEXT NOT NULL,
    payload_hash             BLOB NOT NULL,         -- sha256 of canonical payload
    response_status          INTEGER NOT NULL,
    response_body            BLOB NOT NULL,
    response_headers_json    BLOB,
    submission_id            TEXT,
    stored_at                INTEGER NOT NULL,
    expires_at               INTEGER NOT NULL,      -- stored_at + ttl
    PRIMARY KEY (api_key_hash, idempotency_key)
);

CREATE INDEX idx_idempotency_expiry ON idempotency(expires_at);

-- Unsubscribe tokens for #9. Token is HMAC-signed but stored for
-- one-click consumption tracking.
CREATE TABLE unsubscribe_tokens (
    token         TEXT PRIMARY KEY,                  -- HMAC-signed token
    endpoint      TEXT NOT NULL,
    email         TEXT NOT NULL,
    issued_at     INTEGER NOT NULL,
    consumed_at   INTEGER                            -- NULL until clicked
);

CREATE INDEX idx_unsubscribe_tokens_email ON unsubscribe_tokens(email, endpoint);

-- Internal migration tracking.
CREATE TABLE _migrations (
    version    INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
);
```

The `_migrations` table tracks applied schema versions; future migrations append `INSERT INTO _migrations (version, applied_at) VALUES (...)` at the end so re-runs are idempotent.

### Migration system

Bespoke, following ADR-1's "prefer in-repo code to dependencies" principle:

- Migration files live at `core/storage/migrations/00N_name.sql`, embedded via `go:embed`
- Files named `001_baseline.sql`, `002_add_xyz.sql`, etc. — strict sequential numeric prefix
- On startup, the storage layer reads `MAX(version) FROM _migrations`, then iterates embedded files numerically and applies any whose version > applied
- All migrations run inside a single `BEGIN TRANSACTION` for safety
- **Forward-only.** No rollback files. If a migration goes wrong, restore from backup. Operators run a `sqlite3 backup` cron via `posthorn validate` or external tooling; the project provides docs.

Migration runs at startup, before any other storage access. Failure is fatal — Posthorn refuses to serve.

### Retry algorithm (out-of-request retries, v2-specific)

**Important distinction:** v1.0's retry policy (FR19-22) is **in-request**: synchronous, one retry, hard 10s timeout. The caller waits for the final outcome.

v2 adds **out-of-request** retries: after the in-request retry policy exhausts (returning 502 to the caller), the submission moves to the retry queue. A background worker tries again, asynchronously, for up to ~4 days.

Algorithm: **decorrelated jitter exponential backoff** (AWS pattern):

```
sleep = random_between(base, min(cap, prev_sleep * 3))
```

- `base` = 30s — first retry happens ~30s after the in-request failure
- `cap` = 4h — retries spread out over time but never wait longer than 4h between attempts
- Max attempts: **12** — total wait time is roughly 4 days under worst-case spread

After 12 attempts, submission status moves to `failed_permanently`. Logged at ERROR; operator manually intervenes (re-queue from `submissions` table if desired, or accept the loss).

Decorrelated jitter chosen over pure exponential because the latter has retry waves that thunder against an upstream just recovering from an outage. Decorrelated spreads the load.

### Queue persistence semantics

**At-least-once delivery.** A submission may be sent twice if Posthorn is killed mid-send (the SQL UPDATE to mark `status='sent'` may not have committed before the kill). Operators with strong dedup requirements use idempotency keys at the caller level — the same submission with the same `Idempotency-Key` won't double-send (Postmark / Resend / Mailgun all dedupe on this).

The alternative — at-most-once via "write-after-send-confirmed" — risks losing submissions if the upstream returns success but Posthorn crashes before the SQL update. At-least-once is the safer default for transactional mail.

### Background worker

One queue worker per Posthorn process. No distributed coordination — v2 is single-process.

```go
// pseudocode
func (w *Worker) Run(ctx context.Context) {
    tick := time.NewTicker(5 * time.Second)
    for {
        select {
        case <-ctx.Done():
            return
        case <-tick.C:
            w.processDueRetries(ctx)
        }
    }
}

func (w *Worker) processDueRetries(ctx context.Context) {
    rows := w.storage.DueRetries(ctx, time.Now(), batchSize=10)
    for _, r := range rows {
        // attempt the send, update queue state, log outcome
    }
}
```

5s polling cadence is plenty for Posthorn's expected volume. Batch size 10 prevents one worker run from monopolizing a slow transport.

## Interfaces

### `core/storage/storage.go`

```go
package storage

import "context"

// Store is the v2 storage interface. One implementation (SQLite via
// modernc.org/sqlite) ships in v2.0; the interface allows for in-memory
// implementations for testing.
type Store interface {
    // Submission lifecycle
    InsertSubmission(ctx context.Context, s *Submission) error
    UpdateSubmissionStatus(ctx context.Context, id string, status SubmissionStatus, lastError string, transportMessageID string) error
    GetSubmission(ctx context.Context, id string) (*Submission, error)

    // Retry queue
    EnqueueRetry(ctx context.Context, submissionID string, attempt int, nextAttemptAt time.Time, lastError string) error
    DueRetries(ctx context.Context, now time.Time, limit int) ([]RetryEntry, error)
    UpdateRetryAttempt(ctx context.Context, submissionID string, attempt int, nextAttemptAt time.Time, lastError string) error
    DeleteRetry(ctx context.Context, submissionID string) error

    // Suppression list
    IsSuppressed(ctx context.Context, email, endpoint string) (bool, *Suppression, error)
    AddSuppression(ctx context.Context, s *Suppression) error
    RemoveSuppression(ctx context.Context, email, endpoint string) error
    ListSuppressions(ctx context.Context, endpoint string, limit, offset int) ([]Suppression, error)

    // Idempotency (used by core/idempotency.Cache implementation)
    GetIdempotency(ctx context.Context, apiKeyHash []byte, idempKey string) (*IdempotencyEntry, error)
    PutIdempotency(ctx context.Context, e *IdempotencyEntry) error
    CleanupExpiredIdempotency(ctx context.Context, now time.Time) (int, error)

    // Unsubscribe tokens
    IssueUnsubscribeToken(ctx context.Context, t *UnsubscribeToken) error
    ConsumeUnsubscribeToken(ctx context.Context, token string) (*UnsubscribeToken, error)

    // Lifecycle
    Migrate(ctx context.Context) error
    Close() error
}
```

### `core/idempotency/cache.go` (v2 impl)

The `Cache` interface from #6's design review stays unchanged. The v2 implementation wraps `Store`:

```go
type DurableCache struct {
    store storage.Store
}

func (c *DurableCache) Get(ctx context.Context, apiKey, idempKey string, payloadHash []byte) (*CachedResponse, bool, bool, error) {
    apiKeyHash := sha256.Sum256([]byte(apiKey))
    entry, err := c.store.GetIdempotency(ctx, apiKeyHash[:], idempKey)
    // ... translate entry → CachedResponse, check expiry, check payload hash conflict
}

func (c *DurableCache) Put(ctx context.Context, apiKey, idempKey string, payloadHash []byte, resp *CachedResponse) error {
    apiKeyHash := sha256.Sum256([]byte(apiKey))
    return c.store.PutIdempotency(ctx, &storage.IdempotencyEntry{...})
}
```

Operators opt between in-memory (v1.1) and durable (v2) via config:

```toml
[idempotency]
backend = "memory"   # v1.1 default; "sqlite" requires v2 [storage] block configured
```

### `core/queue/worker.go`

```go
type Worker struct {
    store      storage.Store
    transports map[string]transport.Transport  // resolved from configured endpoints
    logger     *slog.Logger
    interval   time.Duration                    // 5s default
    batchSize  int                              // 10 default
}

func (w *Worker) Run(ctx context.Context) { ... }
```

The worker is started by `cmd/posthorn/main.go` if `[storage]` is configured. It runs alongside the HTTP listener; SIGTERM cancels both.

## Configuration

```toml
[storage]
backend = "sqlite"                      # only "sqlite" in v2; future "postgres" possible
path    = "/var/lib/posthorn/posthorn.db"
journal_mode = "WAL"                    # SQLite tuning; "WAL" recommended
busy_timeout = "5s"                     # how long writers wait for the lock
in_memory = false                       # tests use true; production always false

[retry_queue]
enabled = true                          # default true when [storage] is configured
worker_interval = "5s"
worker_batch_size = 10
base_backoff = "30s"
max_backoff = "4h"
max_attempts = 12

[idempotency]
backend = "sqlite"                      # "memory" (v1.1 default) or "sqlite" (v2)
cleanup_interval = "1h"                 # background cleanup of expired entries
```

`[retry_queue]` is implicitly enabled when `[storage]` is present, but operators can disable to keep submissions in storage without retrying.

## Lifecycle

### Startup

```
1. cmd/posthorn/main.go: parse CLI flags
2. config.Load(path): TOML → Config (with new [storage], [retry_queue], [idempotency] blocks)
3. storage.Open(cfg.Storage): open SQLite, run pending migrations, return Store
4. idempotency.NewCache(cfg.Idempotency, store): wire either in-memory or durable cache
5. queue.NewWorker(store, transports, logger): construct (don't start yet)
6. Build http.ServeMux with handler.New(...) per endpoint, each wired to Store + Cache
7. http.Server.ListenAndServe() in goroutine
8. queue.Worker.Run() in goroutine
9. Wait for SIGTERM/SIGINT
10. Cancel context: server.Shutdown drains HTTP; worker.Run exits its loop; storage.Close commits any open tx
11. Exit 0
```

### Per-request (synchronous send path)

```
1. ... existing v1.x pipeline up to template render ...
2. storage.InsertSubmission(status="pending", payload, headers)
3. (if idempotency configured): cache.Get → return cached if match
4. transport.Send (with in-request retry per FR19-22)
5a. Success: storage.UpdateSubmissionStatus(status="sent", transport_message_id=...); response 200
5b. Failure: storage.UpdateSubmissionStatus(status="failed", last_error=...); storage.EnqueueRetry(attempt=1, next_attempt_at=now+30s); response 502
6. (if idempotency configured): cache.Put with final response
```

The synchronous v1.x retry policy (FR19-22) is unchanged. Out-of-request retry is a NEW path layered on top — the caller still sees the original 502 if the in-request retries fail; the queue then catches the failure and retries asynchronously.

### Worker iteration

```
1. Tick (every 5s)
2. storage.DueRetries(now, limit=10) → list of submissions with next_attempt_at <= now
3. For each:
   a. transport.Send(submission.payload)
   b. Success: storage.UpdateSubmissionStatus(status="sent", ...); storage.DeleteRetry(submissionID)
   c. Failure: attempt++; if attempt > maxAttempts: status="failed_permanently"; else compute next_attempt_at via decorrelated jitter, storage.UpdateRetryAttempt
   d. Log outcome with submissionID + attempt
```

## Failure modes and recovery

| Failure | Behavior |
|---|---|
| SQLite file unwritable at startup | Posthorn refuses to start. Fatal error in `storage.Open`. Same shape as missing config field. |
| Migration fails | Posthorn refuses to start. Fatal. Operator restores from backup or fixes the schema manually. |
| SQLite locked under high write contention | `busy_timeout` (5s default) handles this; writes block up to that, then return error. With WAL journal mode and Posthorn's expected volume, contention should be rare. |
| Disk full while writing submission | `InsertSubmission` returns error; handler returns 500 to caller without sending. Submission is NOT sent. Operator alarms on `submission_storage_failed` log events. |
| Posthorn killed mid-send (in-request) | At-least-once: submission row exists with status=pending. On restart, the worker picks it up and retries. **May re-send if the in-request send actually succeeded but the status update didn't commit.** Operators with strong dedup use caller-side idempotency. |
| Posthorn killed mid-retry (out-of-request) | Same shape: retry row exists with `next_attempt_at <= now`; worker picks it up. May re-send. |
| Database file corruption | Operator restores from backup. Posthorn refuses to start if corruption detected at boot. |

## Test architecture

- **In-memory backend for unit tests.** `Store` implementation can use `:memory:` SQLite. Every test gets a fresh schema-migrated database via `t.Cleanup(store.Close)`.
- **Migration tests.** Apply migrations forward from a known-empty database; verify each migration's idempotency by re-running.
- **Retry-queue tests.** Construct a known failure stream from a mock transport, run worker iterations, assert state transitions (pending → failed → retry → sent / failed_permanently).
- **Concurrency tests under `-race`.** Multiple goroutines insert submissions while worker reads queue; assert no SQL errors and no lost retries.
- **Recovery tests.** Simulate killed-mid-send: insert submission with status=pending, kill before status update, restart, assert worker picks it up.

## Open questions

1. **Where does the SQLite file live in Docker?** Convention says `/var/lib/posthorn/posthorn.db` mounted as a volume. Docs need an explicit "production volume mount" section for the existing Dockerfile.
2. **WAL checkpoint cadence?** SQLite's WAL grows until a checkpoint flushes it to the main database file. Auto-checkpoint at 1000 pages (~1MB) is reasonable for Posthorn's write volume. Configurable via `journal_size_limit` if pushback.
3. **Backup story?** v2 ships docs showing `sqlite3 posthorn.db ".backup '/backup/posthorn-$(date +%Y%m%d).db'"` as a cron. v3+ may add a `posthorn backup` CLI subcommand that handles WAL checkpoints cleanly.
4. **What happens to `submissions` with `log_failed_submissions = false`?** The handler currently logs only field names in that case. For SQLite, do we store the payload anyway (for retry to work) and gate operator-visibility separately? Recommendation: **yes, always store the payload** (retry needs it). The `log_failed_submissions` flag controls log output, not storage. Document explicitly so GDPR-conscious operators understand the distinction — they can purge `submissions` rows after retries complete via a separate retention policy (v2.x polish).
5. **Retention / pruning?** v2.0 has no automatic pruning of `submissions`. Operators run `DELETE FROM submissions WHERE created_at < ?` manually. v2.1 may add `[storage] retention = "90d"` for auto-prune.
6. **Concurrency model — single worker or pool?** Single worker for v2. A pool requires distinguishing "this worker is handling this row" which means another column (`leased_until`) and lease-expiry logic. v3+ if needed.

## Implementation plan — sub-issues to file

Once this spec lands, file these sub-issues against the v2 milestone:

1. **storage: SQLite Store implementation + migration system** — the core foundation (existing #21)
2. **storage: in-memory Store for tests** — companion to #21
3. **storage: WAL tuning + backup docs** — operational polish (could be combined with #21)
4. **queue: background retry worker** — uses Store from #21
5. **idempotency: durable cache backend** — existing #22
6. **config: `[storage]`, `[retry_queue]`, `[idempotency]` blocks** — small, in #21
7. **cmd/posthorn: integrate storage lifecycle (open, migrate, close on SIGTERM)** — small, in #21

The other v2 features (#4 suppression, #8 lifecycle callbacks, #9 unsubscribe, #5 HTML, #23 webhook transport, #24 attachments, #25 fan-out) build on this foundation as separate efforts.

## References

- [Project brief §"Post-MVP Vision" — v2](./01-project-brief.md#post-mvp-vision) — high-level v2 scope
- [Architecture doc §"Forward compatibility / v2"](./03-architecture.md#v2-platform-maturity--persistent-state--mail-platform-features) — architectural commitments
- [ADR-1: No third-party Postmark SDK](./03-architecture.md#architectural-decisions-log) — pattern for "prefer bespoke + stdlib"
- [ADR-5: Synchronous send, not async with queue (v1.0)](./03-architecture.md#architectural-decisions-log) — v2 changes this: queue is the new outer layer, but `Transport.Send` stays the synchronous primitive
- [AWS architecture blog: "Exponential Backoff and Jitter"](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) — decorrelated jitter rationale
