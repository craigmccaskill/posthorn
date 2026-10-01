package storage

import (
	"path/filepath"
	"testing"
	"time"
)

func TestMaxAttempts_MatchesBackoffLadder(t *testing.T) {
	if MaxAttempts != len(RetryBackoff) {
		t.Fatalf("MaxAttempts = %d, RetryBackoff steps = %d — must stay equal", MaxAttempts, len(RetryBackoff))
	}
}

func TestEnqueue_SchedulesFirstBackoff(t *testing.T) {
	s := memStore(t)
	if err := s.RecordSubmission(sampleSubmission("sub-1", StatusSending)); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue("sub-1", t0); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	got, _, _ := s.GetSubmission("sub-1")
	if got.Status != StatusQueued {
		t.Errorf("status = %q", got.Status)
	}

	// Not due before the first backoff elapses.
	due, err := s.ClaimDue(t0.Add(RetryBackoff[0]-time.Second), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Errorf("claimed %d entries before backoff elapsed", len(due))
	}
	// Due after.
	due, err = s.ClaimDue(t0.Add(RetryBackoff[0]+time.Second), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ID != "sub-1" || due[0].Attempt != 0 {
		t.Fatalf("due = %+v", due)
	}
}

func TestClaimDue_MarksSendingAndCarriesMessage(t *testing.T) {
	s := memStore(t)
	sub := sampleSubmission("sub-1", StatusSending)
	if err := s.RecordSubmission(sub); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue("sub-1", t0); err != nil {
		t.Fatal(err)
	}
	due, err := s.ClaimDue(t0.Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("due = %d", len(due))
	}
	if due[0].BodyHTML != "<p>html part</p>" || len(due[0].ToAddrs) != 2 {
		t.Errorf("claimed submission lost content: %+v", due[0].Submission)
	}
	got, _, _ := s.GetSubmission("sub-1")
	if got.Status != StatusSending {
		t.Errorf("claimed status = %q, want %q", got.Status, StatusSending)
	}
}

func TestRecordRetryFailure_BacksOffThenDeadLetters(t *testing.T) {
	s := memStore(t)
	if err := s.RecordSubmission(sampleSubmission("sub-1", StatusSending)); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue("sub-1", t0); err != nil {
		t.Fatal(err)
	}

	now := t0
	for i := 1; i < MaxAttempts; i++ {
		dead, err := s.RecordRetryFailure("sub-1", "upstream 503", now)
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if dead {
			t.Fatalf("dead-lettered at attempt %d, want %d", i, MaxAttempts)
		}
		got, _, _ := s.GetSubmission("sub-1")
		if got.Status != StatusQueued || got.LastError != "upstream 503" {
			t.Fatalf("attempt %d: status=%q err=%q", i, got.Status, got.LastError)
		}
		// The next due time honors the backoff ladder.
		if due, _ := s.ClaimDue(now.Add(RetryBackoff[i]-time.Second), 10); len(due) != 0 {
			t.Fatalf("attempt %d: due before backoff %v elapsed", i, RetryBackoff[i])
		}
		if due, _ := s.ClaimDue(now.Add(RetryBackoff[i]+time.Second), 10); len(due) != 1 {
			t.Fatalf("attempt %d: not due after backoff", i)
		}
		// ClaimDue marked it sending again for the next round.
	}

	dead, err := s.RecordRetryFailure("sub-1", "upstream 503 final", now)
	if err != nil {
		t.Fatal(err)
	}
	if !dead {
		t.Fatal("expected dead-letter at MaxAttempts")
	}
	got, _, _ := s.GetSubmission("sub-1")
	if got.Status != StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if got.LastError != "upstream 503 final" {
		t.Errorf("last_error = %q", got.LastError)
	}
	if n, _ := s.QueueDepth(); n != 0 {
		t.Errorf("queue depth = %d after dead-letter", n)
	}
}

func TestMarkSent_RemovesQueueEntry(t *testing.T) {
	s := memStore(t)
	if err := s.RecordSubmission(sampleSubmission("sub-1", StatusSending)); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue("sub-1", t0); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.QueueDepth(); n != 1 {
		t.Fatalf("depth = %d", n)
	}
	if err := s.MarkSent("sub-1", "msg-1", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.QueueDepth(); n != 0 {
		t.Errorf("depth = %d after MarkSent", n)
	}
}

func TestCrashDuringClaim_RecoversWithAttemptCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "posthorn.db")
	s, err := Open(Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSubmission(sampleSubmission("sub-1", StatusSending)); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue("sub-1", t0); err != nil {
		t.Fatal(err)
	}
	// Two failures, then a claim, then a crash.
	if _, err := s.RecordRetryFailure("sub-1", "e1", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordRetryFailure("sub-1", "e2", t0); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.ClaimDue(t0.Add(24*time.Hour), 10); len(due) != 1 || due[0].Attempt != 2 {
		t.Fatalf("claim before crash: %+v", due)
	}
	_ = s.Close() // crash while "sending"

	s2, err := Open(Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()

	got, _, _ := s2.GetSubmission("sub-1")
	if got.Status != StatusQueued {
		t.Errorf("recovered status = %q", got.Status)
	}
	// Recovery preserves the existing queue row (schedule + attempt
	// count); the entry is due at its original next_attempt_at, which
	// had already passed when it was claimed.
	due, err := s2.ClaimDue(t0.Add(24*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("recovered due = %d", len(due))
	}
	if due[0].Attempt != 2 {
		t.Errorf("attempt count lost across crash: %d, want 2", due[0].Attempt)
	}
}

func TestClaimDue_RespectsLimit(t *testing.T) {
	s := memStore(t)
	for _, id := range []string{"a", "b", "c"} {
		sub := sampleSubmission(id, StatusSending)
		if err := s.RecordSubmission(sub); err != nil {
			t.Fatal(err)
		}
		if err := s.Enqueue(id, t0); err != nil {
			t.Fatal(err)
		}
	}
	due, err := s.ClaimDue(t0.Add(time.Hour), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 2 {
		t.Errorf("limit ignored: claimed %d", len(due))
	}
}

func TestQueuedByEndpoint(t *testing.T) {
	s := memStore(t)
	got, err := s.QueuedByEndpoint()
	if err != nil || len(got) != 0 {
		t.Fatalf("empty queue = %v, %v", got, err)
	}
	for id, endpoint := range map[string]string{"q1": "smtp_listener", "q2": "smtp_listener", "q3": "/contact", "sent": "/contact"} {
		sub := sampleSubmission(id, StatusSending)
		sub.Endpoint = endpoint
		if err := s.RecordSubmission(sub); err != nil {
			t.Fatal(err)
		}
		if id == "sent" {
			continue // recorded, never queued
		}
		if err := s.Enqueue(id, t0); err != nil {
			t.Fatal(err)
		}
	}
	got, err = s.QueuedByEndpoint()
	if err != nil {
		t.Fatalf("QueuedByEndpoint: %v", err)
	}
	if len(got) != 2 || got["smtp_listener"] != 2 || got["/contact"] != 1 {
		t.Errorf("QueuedByEndpoint = %v, want smtp_listener:2 /contact:1", got)
	}
}
