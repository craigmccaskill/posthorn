package storage

import (
	"database/sql"
	"fmt"
	"time"
)

// Retry policy for the background queue (FR78, ADR-21). The queue only
// ever holds sends whose inline FR19-22 retries exhausted on a
// transient/rate-limited failure — terminal failures never enqueue.
//
// Attempt N (0-based) schedules its next try RetryBackoff[N] after the
// failure; after MaxAttempts failures the submission dead-letters as
// status "failed". No stored jitter: this is a single-instance system
// (NFR26), so there is no herd to spread — the worker's own polling
// cadence provides natural dispersion.
var RetryBackoff = []time.Duration{
	1 * time.Minute,
	4 * time.Minute,
	16 * time.Minute,
	64 * time.Minute,
	256 * time.Minute,
}

// MaxAttempts is the number of background failures before dead-letter.
// Kept equal to len(RetryBackoff) — one scheduled wait per allowed
// attempt (asserted in tests since a var can't be a const expression).
var MaxAttempts = len(RetryBackoff)

// Enqueue moves a submission into the retry queue (FR78): queue row at
// attempt 0 due after the first backoff step, submission status
// "queued". Idempotent per submission.
func (s *Store) Enqueue(id string, now time.Time) error {
	return s.inTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`
			INSERT INTO retry_queue (submission_id, attempt, next_attempt_at)
			VALUES (?, 0, ?)
			ON CONFLICT(submission_id) DO NOTHING`,
			id, now.Add(RetryBackoff[0]).Unix()); err != nil {
			return err
		}
		_, err := tx.Exec(
			`UPDATE submissions SET status = ? WHERE id = ?`, StatusQueued, id)
		return err
	})
}

// DueRetry is a claimed queue entry: the full submission plus its
// attempt counter (the number of background failures so far).
type DueRetry struct {
	Submission
	Attempt int
}

// ClaimDue returns up to limit entries whose next_attempt_at has
// passed, marking their submissions "sending". A crash after a claim
// is safe: Open's recovery moves "sending" rows straight back to the
// queue, preserving the attempt count (the queue row survives a claim
// and is only removed on MarkSent or dead-letter).
func (s *Store) ClaimDue(now time.Time, limit int) ([]DueRetry, error) {
	rows, err := s.db.Query(`
		SELECT submission_id, attempt FROM retry_queue
		WHERE next_attempt_at <= ?
		ORDER BY next_attempt_at
		LIMIT ?`, now.Unix(), limit)
	if err != nil {
		return nil, fmt.Errorf("storage: claim due: %w", err)
	}
	type entry struct {
		id      string
		attempt int
	}
	var entries []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.id, &e.attempt); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("storage: claim due: %w", err)
		}
		entries = append(entries, e)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: claim due: %w", err)
	}

	var due []DueRetry
	for _, e := range entries {
		sub, ok, err := s.GetSubmission(e.id)
		if err != nil {
			return due, err
		}
		if !ok {
			// Orphaned queue row (shouldn't happen given the FK, but a
			// missing parent must not wedge the queue): drop it.
			_, _ = s.db.Exec(`DELETE FROM retry_queue WHERE submission_id = ?`, e.id)
			continue
		}
		if err := s.MarkStatus(e.id, StatusSending, sub.LastError); err != nil {
			return due, err
		}
		sub.Status = StatusSending
		due = append(due, DueRetry{Submission: sub, Attempt: e.attempt})
	}
	return due, nil
}

// RecordRetryFailure bumps the attempt counter after a failed
// background try. Once MaxAttempts is reached the submission
// dead-letters: queue row removed, status "failed" (FR78). Otherwise
// the next attempt is scheduled per RetryBackoff and the submission
// returns to "queued". Returns whether the submission dead-lettered.
func (s *Store) RecordRetryFailure(id, lastError string, now time.Time) (deadLettered bool, err error) {
	err = s.inTx(func(tx *sql.Tx) error {
		var attempt int
		if err := tx.QueryRow(
			`SELECT attempt FROM retry_queue WHERE submission_id = ?`, id).Scan(&attempt); err != nil {
			return fmt.Errorf("read attempt: %w", err)
		}
		attempt++
		if attempt >= MaxAttempts {
			deadLettered = true
			if _, err := tx.Exec(`DELETE FROM retry_queue WHERE submission_id = ?`, id); err != nil {
				return err
			}
			_, err := tx.Exec(
				`UPDATE submissions SET status = ?, last_error = ? WHERE id = ?`,
				StatusFailed, lastError, id)
			return err
		}
		if _, err := tx.Exec(`
			UPDATE retry_queue SET attempt = ?, next_attempt_at = ? WHERE submission_id = ?`,
			attempt, now.Add(RetryBackoff[attempt]).Unix(), id); err != nil {
			return err
		}
		_, err := tx.Exec(
			`UPDATE submissions SET status = ?, last_error = ? WHERE id = ?`,
			StatusQueued, lastError, id)
		return err
	})
	return deadLettered, err
}

// DeadLetter removes a submission from the queue and marks it failed
// immediately, regardless of remaining attempts. Used when a background
// retry hits a *terminal* transport error — the provider has said this
// send will never succeed, so laddering further is noise (FR78).
func (s *Store) DeadLetter(id, lastError string) error {
	return s.inTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM retry_queue WHERE submission_id = ?`, id); err != nil {
			return err
		}
		_, err := tx.Exec(
			`UPDATE submissions SET status = ?, last_error = ? WHERE id = ?`,
			StatusFailed, lastError, id)
		return err
	})
}

// QueueDepth reports how many submissions are waiting or in flight —
// the posthorn_retry_queue_depth gauge.
func (s *Store) QueueDepth() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM retry_queue`).Scan(&n); err != nil {
		return 0, fmt.Errorf("storage: queue depth: %w", err)
	}
	return n, nil
}

// QueuedByEndpoint counts retry-queue entries per endpoint. cmd reads
// it once at startup to notice queued mail whose endpoint the new
// config no longer has.
func (s *Store) QueuedByEndpoint() (map[string]int, error) {
	rows, err := s.db.Query(`
		SELECT sub.endpoint, COUNT(*)
		FROM retry_queue q JOIN submissions sub ON sub.id = q.submission_id
		GROUP BY sub.endpoint`)
	if err != nil {
		return nil, fmt.Errorf("storage: queued by endpoint: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var endpoint string
		var n int
		if err := rows.Scan(&endpoint, &n); err != nil {
			return nil, fmt.Errorf("storage: queued by endpoint: %w", err)
		}
		out[endpoint] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: queued by endpoint: %w", err)
	}
	return out, nil
}
