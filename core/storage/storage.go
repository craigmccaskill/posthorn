// Package storage is the v2.0 SQLite spine (FR76-FR81, ADR-20, ADR-21).
//
// It is entirely optional: without a `[storage]` config block nothing in
// this package is constructed and every v1.x code path runs unchanged.
// When configured, it provides the submission log, the retry queue, the
// durable idempotency backend, and the suppression list — one file, one
// process, one writer (NFR26).
//
// Failure posture (NFR27): callers treat any error from this package as
// a degrade signal, never a reason to block mail. The gateway keeps
// sending synchronously; it just stops persisting until the canary
// probe reports recovery.
//
// The driver is modernc.org/sqlite — pure Go, preserving the
// CGO_ENABLED=0 static multi-arch build. This is deliberately the
// project's largest dependency; the reasoning and named costs live in
// ADR-20. Everything SQLite touches stays behind this package.
package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/craigmccaskill/posthorn/transport"
)

// schemaVersion is stamped into SQLite's user_version pragma.
// Migrations are forward-only: Open applies every step from the file's
// current version up to this constant. A binary refuses a file stamped
// newer than it knows, so each bump is also a one-way door for
// downgrades and belongs in the CHANGELOG.
//
//	1  v2.0  initial schema
//	2  v2.1  submissions.headers (passthrough headers survive the queue)
const schemaVersion = 2

// Submission statuses. The visible lifecycle is
// sending → sent | queued → sent | failed, plus suppressed for sends
// refused at the suppression check. "sending" rows found at Open are
// the crash window: they re-enter the queue, which is the documented
// at-least-once duplicate risk (NFR28).
const (
	StatusSending    = "sending"
	StatusSent       = "sent"
	StatusQueued     = "queued"
	StatusFailed     = "failed"
	StatusSuppressed = "suppressed"
)

// Config mirrors the `[storage]` TOML block after parsing.
type Config struct {
	// Path is the SQLite file location. Required unless InMemory.
	Path string

	// InMemory uses a private in-memory database — tests only.
	InMemory bool

	// Retention bounds how long non-pending submission rows live.
	// Zero means the config default (30d) was not applied by the caller;
	// Store itself does not interpret zero specially — Prune receives
	// an explicit cutoff.
	Retention time.Duration

	// MaxSize caps the database file size via SQLite's max_page_count
	// (FR79) so Posthorn can never fill the disk on its own. Zero means
	// no cap (the config layer applies the 1GB default).
	MaxSize int64
}

// Store wraps the SQLite handle. All methods are safe for concurrent
// use within one process; the pool is capped at a single connection,
// which both serializes writers (SQLite wants that) and makes the
// single-writer assumption structural rather than conventional.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database, applies pragmas and
// forward-only migrations, and recovers crash-window rows: submissions
// stuck in "sending" from a previous process re-enter the retry queue
// with an immediate next attempt (NFR28).
func Open(cfg Config) (*Store, error) {
	dsn := cfg.Path
	if cfg.InMemory {
		dsn = ":memory:"
	}
	if dsn == "" {
		return nil, fmt.Errorf("storage: path is required unless in_memory = true")
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open %q: %w", dsn, err)
	}
	// One connection: serialized access, no SQLITE_BUSY dances, and the
	// NFR26 single-writer stance enforced by construction. Throughput is
	// not a concern at Posthorn volumes.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.init(cfg); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init(cfg Config) error {
	pragmas := []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
		// FULL over NORMAL: a power loss must not lose a committed
		// queue entry — the queue's entire promise is surviving exactly
		// that. Commit latency is irrelevant at our volumes.
		"PRAGMA synchronous = FULL",
	}
	if !isMemory(cfg) {
		pragmas = append(pragmas, "PRAGMA journal_mode = WAL")
	}
	for _, p := range pragmas {
		if _, err := s.db.Exec(p); err != nil {
			return fmt.Errorf("storage: %s: %w", p, err)
		}
	}

	if cfg.MaxSize > 0 {
		var pageSize int64
		if err := s.db.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil || pageSize <= 0 {
			return fmt.Errorf("storage: read page_size: %w", err)
		}
		pages := cfg.MaxSize / pageSize
		if pages < 16 {
			pages = 16 // floor: room for the schema itself
		}
		if _, err := s.db.Exec(fmt.Sprintf("PRAGMA max_page_count = %d", pages)); err != nil {
			return fmt.Errorf("storage: set max_page_count: %w", err)
		}
	}

	if err := s.migrate(); err != nil {
		return err
	}
	return s.recoverInFlight()
}

func isMemory(cfg Config) bool {
	return cfg.InMemory || cfg.Path == ":memory:"
}

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("storage: read user_version: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("storage: database schema version %d is newer than this binary supports (%d) — refusing to open", version, schemaVersion)
	}
	if version < 1 {
		if _, err := s.db.Exec(schemaV1); err != nil {
			return fmt.Errorf("storage: apply schema v1: %w", err)
		}
	}
	if version < 2 {
		if _, err := s.db.Exec(schemaV2); err != nil {
			return fmt.Errorf("storage: apply schema v2: %w", err)
		}
	}
	if version != schemaVersion {
		if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			return fmt.Errorf("storage: stamp user_version: %w", err)
		}
	}
	return nil
}

const schemaV1 = `
CREATE TABLE IF NOT EXISTS submissions (
  id                   TEXT PRIMARY KEY,
  endpoint             TEXT NOT NULL,
  transport            TEXT NOT NULL DEFAULT '',
  from_addr            TEXT NOT NULL DEFAULT '',
  to_addrs             TEXT NOT NULL DEFAULT '[]',
  reply_to             TEXT NOT NULL DEFAULT '',
  subject              TEXT NOT NULL DEFAULT '',
  body_text            TEXT NOT NULL DEFAULT '',
  body_html            TEXT NOT NULL DEFAULT '',
  fields               TEXT NOT NULL DEFAULT '{}',
  attachments          TEXT NOT NULL DEFAULT '[]',
  client_ip            TEXT NOT NULL DEFAULT '',
  status               TEXT NOT NULL,
  transport_message_id TEXT NOT NULL DEFAULT '',
  last_error           TEXT NOT NULL DEFAULT '',
  created_at           INTEGER NOT NULL,
  sent_at              INTEGER
);
CREATE INDEX IF NOT EXISTS idx_submissions_created ON submissions(created_at);
CREATE INDEX IF NOT EXISTS idx_submissions_msgid   ON submissions(transport_message_id);

CREATE TABLE IF NOT EXISTS retry_queue (
  submission_id   TEXT PRIMARY KEY REFERENCES submissions(id) ON DELETE CASCADE,
  attempt         INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_retry_due ON retry_queue(next_attempt_at);

CREATE TABLE IF NOT EXISTS idempotency (
  key        TEXT NOT NULL,
  endpoint   TEXT NOT NULL,
  response   BLOB NOT NULL,
  expires_at INTEGER NOT NULL,
  PRIMARY KEY (key, endpoint)
);
CREATE INDEX IF NOT EXISTS idx_idem_expiry ON idempotency(expires_at);

CREATE TABLE IF NOT EXISTS suppressions (
  email           TEXT NOT NULL,
  reason          TEXT NOT NULL,
  source_endpoint TEXT NOT NULL DEFAULT '',
  created_at      INTEGER NOT NULL,
  PRIMARY KEY (email, reason)
);

CREATE TABLE IF NOT EXISTS canary (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS lifecycle_queue (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  endpoint        TEXT NOT NULL,
  payload         BLOB NOT NULL,
  attempt         INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_lifecycle_due ON lifecycle_queue(next_attempt_at);
`

// schemaV2 adds the passthrough headers column (FR99, ADR-27). Without
// it a queued or crash-recovered SMTP submission replayed with no
// headers, which is the silent drop FR99 forbids. Existing rows get the
// empty list.
const schemaV2 = `
ALTER TABLE submissions ADD COLUMN headers TEXT NOT NULL DEFAULT '[]';
`

// recoverInFlight moves crash-window rows ("sending" at open) into the
// retry queue with an immediate due time. This is the deliberate
// at-least-once choice from ADR-21: the provider may have accepted the
// send before the crash, and a duplicate beats silent loss.
func (s *Store) recoverInFlight() error {
	_, err := s.db.Exec(`
		INSERT OR IGNORE INTO retry_queue (submission_id, attempt, next_attempt_at)
		SELECT id, 0, 0 FROM submissions WHERE status = ?`, StatusSending)
	if err != nil {
		return fmt.Errorf("storage: recover in-flight submissions: %w", err)
	}
	_, err = s.db.Exec(
		`UPDATE submissions SET status = ? WHERE status = ?`, StatusQueued, StatusSending)
	if err != nil {
		return fmt.Errorf("storage: recover in-flight submissions: %w", err)
	}
	return nil
}

// Submission is one row of the submission log (FR77). ToAddrs and
// Fields marshal to JSON columns. For endpoints with
// log_failed_submissions = false, the caller records metadata only:
// content fields stay empty and the row is queue-ineligible.
type Submission struct {
	ID                 string
	Endpoint           string
	Transport          string
	From               string
	ToAddrs            []string
	ReplyTo            string
	Subject            string
	BodyText           string
	BodyHTML           string
	Fields             map[string][]string
	Attachments        []Attachment
	Headers            []transport.Header // SMTP passthrough headers (ADR-27); nil otherwise
	ClientIP           string
	Status             string
	TransportMessageID string
	LastError          string
	CreatedAt          time.Time
	SentAt             time.Time // zero when unsent
}

// Attachment mirrors transport.Attachment for persistence (FR92 —
// queued sends must replay with their files). Data JSON-encodes as
// base64.
type Attachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Data        []byte `json:"data"`
}

// RecordSubmission inserts a new row (FR77).
func (s *Store) RecordSubmission(sub Submission) error {
	toJSON, err := json.Marshal(orEmptySlice(sub.ToAddrs))
	if err != nil {
		return fmt.Errorf("storage: marshal to_addrs: %w", err)
	}
	fieldsJSON, err := json.Marshal(orEmptyMap(sub.Fields))
	if err != nil {
		return fmt.Errorf("storage: marshal fields: %w", err)
	}
	atts := sub.Attachments
	if atts == nil {
		atts = []Attachment{}
	}
	attJSON, err := json.Marshal(atts)
	if err != nil {
		return fmt.Errorf("storage: marshal attachments: %w", err)
	}
	headers := sub.Headers
	if headers == nil {
		headers = []transport.Header{}
	}
	headersJSON, err := json.Marshal(headers)
	if err != nil {
		return fmt.Errorf("storage: marshal headers: %w", err)
	}
	var sentAt any
	if !sub.SentAt.IsZero() {
		sentAt = sub.SentAt.Unix()
	}
	_, err = s.db.Exec(`
		INSERT INTO submissions
		  (id, endpoint, transport, from_addr, to_addrs, reply_to, subject,
		   body_text, body_html, fields, attachments, headers, client_ip, status,
		   transport_message_id, last_error, created_at, sent_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sub.ID, sub.Endpoint, sub.Transport, sub.From, string(toJSON), sub.ReplyTo,
		sub.Subject, sub.BodyText, sub.BodyHTML, string(fieldsJSON), string(attJSON),
		string(headersJSON), sub.ClientIP, sub.Status, sub.TransportMessageID, sub.LastError,
		sub.CreatedAt.Unix(), sentAt)
	if err != nil {
		return fmt.Errorf("storage: record submission: %w", err)
	}
	return nil
}

// MarkSent finalizes a row after provider acceptance and removes any
// queue entry.
func (s *Store) MarkSent(id, transportMessageID string, at time.Time) error {
	return s.inTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`
			UPDATE submissions
			SET status = ?, transport_message_id = ?, sent_at = ?, last_error = ''
			WHERE id = ?`, StatusSent, transportMessageID, at.Unix(), id); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM retry_queue WHERE submission_id = ?`, id)
		return err
	})
}

// MarkStatus updates status + last_error without touching sent_at.
func (s *Store) MarkStatus(id, status, lastError string) error {
	_, err := s.db.Exec(
		`UPDATE submissions SET status = ?, last_error = ? WHERE id = ?`,
		status, lastError, id)
	if err != nil {
		return fmt.Errorf("storage: mark %s: %w", status, err)
	}
	return nil
}

// GetSubmission fetches one row by ID.
func (s *Store) GetSubmission(id string) (Submission, bool, error) {
	return s.scanOne(`WHERE id = ?`, id)
}

// FindByMessageID resolves a provider message ID back to the
// originating submission — the lifecycle-event correlation step (FR83).
func (s *Store) FindByMessageID(transportMessageID string) (Submission, bool, error) {
	if transportMessageID == "" {
		return Submission{}, false, nil
	}
	return s.scanOne(`WHERE transport_message_id = ? ORDER BY created_at DESC LIMIT 1`, transportMessageID)
}

func (s *Store) scanOne(where string, args ...any) (Submission, bool, error) {
	row := s.db.QueryRow(`
		SELECT id, endpoint, transport, from_addr, to_addrs, reply_to, subject,
		       body_text, body_html, fields, attachments, headers, client_ip, status,
		       transport_message_id, last_error, created_at, sent_at
		FROM submissions `+where, args...)

	var sub Submission
	var toJSON, fieldsJSON, attJSON, headersJSON string
	var createdAt int64
	var sentAt sql.NullInt64
	err := row.Scan(&sub.ID, &sub.Endpoint, &sub.Transport, &sub.From, &toJSON,
		&sub.ReplyTo, &sub.Subject, &sub.BodyText, &sub.BodyHTML, &fieldsJSON,
		&attJSON, &headersJSON, &sub.ClientIP, &sub.Status, &sub.TransportMessageID, &sub.LastError,
		&createdAt, &sentAt)
	if err == sql.ErrNoRows {
		return Submission{}, false, nil
	}
	if err != nil {
		return Submission{}, false, fmt.Errorf("storage: get submission: %w", err)
	}
	if err := json.Unmarshal([]byte(toJSON), &sub.ToAddrs); err != nil {
		return Submission{}, false, fmt.Errorf("storage: decode to_addrs: %w", err)
	}
	if err := json.Unmarshal([]byte(fieldsJSON), &sub.Fields); err != nil {
		return Submission{}, false, fmt.Errorf("storage: decode fields: %w", err)
	}
	if err := json.Unmarshal([]byte(attJSON), &sub.Attachments); err != nil {
		return Submission{}, false, fmt.Errorf("storage: decode attachments: %w", err)
	}
	if len(sub.Attachments) == 0 {
		sub.Attachments = nil
	}
	if err := json.Unmarshal([]byte(headersJSON), &sub.Headers); err != nil {
		return Submission{}, false, fmt.Errorf("storage: decode headers: %w", err)
	}
	if len(sub.Headers) == 0 {
		sub.Headers = nil
	}
	sub.CreatedAt = time.Unix(createdAt, 0).UTC()
	if sentAt.Valid {
		sub.SentAt = time.Unix(sentAt.Int64, 0).UTC()
	}
	return sub, true, nil
}

// ListSubmissions returns up to limit rows, newest first — FR77's
// "queryable" acceptance. Serves tests today and the CLI/admin surface
// later; operators can always query the file directly with sqlite3.
func (s *Store) ListSubmissions(limit int) ([]Submission, error) {
	rows, err := s.db.Query(
		`SELECT id FROM submissions ORDER BY created_at DESC, id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("storage: list submissions: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("storage: list submissions: %w", err)
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list submissions: %w", err)
	}
	subs := make([]Submission, 0, len(ids))
	for _, id := range ids {
		sub, ok, err := s.GetSubmission(id)
		if err != nil {
			return nil, err
		}
		if ok {
			subs = append(subs, sub)
		}
	}
	return subs, nil
}

// Prune deletes submission rows older than cutoff (FR79). Pending work
// ("sending"/"queued") survives regardless of age; suppressions are a
// different table and deliberately never pruned (ADR-23); idempotency
// expiry is time-based via PruneIdempotency.
func (s *Store) Prune(cutoff time.Time) (int64, error) {
	res, err := s.db.Exec(`
		DELETE FROM submissions
		WHERE created_at < ? AND status NOT IN (?, ?)`,
		cutoff.Unix(), StatusSending, StatusQueued)
	if err != nil {
		return 0, fmt.Errorf("storage: prune: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// Probe is the canary write (FR80): one upsert through the full write
// path — SQLite, journal, filesystem. An error means the storage layer
// is degraded; the caller flips healthz/metrics and stops persisting
// until a later probe succeeds.
func (s *Store) Probe(now time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO canary (id, at) VALUES (1, ?)
		 ON CONFLICT(id) DO UPDATE SET at = excluded.at`, now.Unix())
	if err != nil {
		return fmt.Errorf("storage: canary probe: %w", err)
	}
	return nil
}

// Close closes the underlying handle.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) inTx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("storage: begin tx: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("storage: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit: %w", err)
	}
	return nil
}

func orEmptySlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func orEmptyMap(m map[string][]string) map[string][]string {
	if m == nil {
		return map[string][]string{}
	}
	return m
}
