package smtp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/craigmccaskill/posthorn/storage"
	"github.com/craigmccaskill/posthorn/transport"
)

// FR77 wiring for the SMTP ingress: rows record pre-send and finalize
// on outcome; FR78's queued state answers 250 (the relay owns the
// retry) so clients don't double-send.

func attachTestGate(t *testing.T, f *smtpFixture) *storage.Gate {
	t.Helper()
	st, err := storage.Open(storage.Config{InMemory: true})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	gate := storage.NewGate(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	f.listener.AttachStorage(gate)
	return gate
}

func shortRetryDelays(t *testing.T) {
	t.Helper()
	origT, origR := transport.TransientRetryDelay, transport.RateLimitedRetryDelay
	transport.TransientRetryDelay = time.Millisecond
	transport.RateLimitedRetryDelay = time.Millisecond
	t.Cleanup(func() {
		transport.TransientRetryDelay = origT
		transport.RateLimitedRetryDelay = origR
	})
}

// deliverBasic drives EHLO→AUTH→MAIL→RCPT→DATA and returns the final
// DATA-reply code and message.
func deliverBasic(t *testing.T, f *smtpFixture) (int, string) {
	t.Helper()
	tp := f.dial()
	_ = tp.PrintfLine("EHLO client.test")
	expectMultiline(t, tp, 250)
	_ = tp.PrintfLine("AUTH PLAIN %s", authPlainCreds("user", "pass"))
	expect(t, tp, 235)
	_ = tp.PrintfLine("MAIL FROM:<noreply@example.com>")
	expect(t, tp, 250)
	_ = tp.PrintfLine("RCPT TO:<alice@somewhere.com>")
	expect(t, tp, 250)
	_ = tp.PrintfLine("DATA")
	expect(t, tp, 354)
	_ = tp.PrintfLine("From: noreply@example.com\r\nSubject: Hello\r\n\r\nBody text.\r\n.")
	code, msg := expectCode(t, tp)
	_ = tp.PrintfLine("QUIT")
	return code, msg
}

func TestSMTP_Storage_RecordsSentRow(t *testing.T) {
	f := startListener(t, baseTestConfig())
	gate := attachTestGate(t, f)
	f.mt.result = transport.SendResult{MessageID: "pm-9"}

	code, _ := deliverBasic(t, f)
	if code != 250 {
		t.Fatalf("DATA reply = %d", code)
	}

	subs, err := gate.Store().ListSubmissions(10)
	if err != nil || len(subs) != 1 {
		t.Fatalf("submissions = %d err=%v", len(subs), err)
	}
	sub := subs[0]
	if sub.Status != storage.StatusSent {
		t.Errorf("status = %q", sub.Status)
	}
	if sub.Endpoint != "smtp_listener" || sub.TransportMessageID != "pm-9" {
		t.Errorf("row = %+v", sub)
	}
	if sub.From != "noreply@example.com" || len(sub.ToAddrs) != 1 || sub.ToAddrs[0] != "alice@somewhere.com" {
		t.Errorf("envelope = %q -> %v", sub.From, sub.ToAddrs)
	}
	if sub.ClientIP == "" {
		t.Error("ClientIP empty (should match the NFR23 session log)")
	}
}

func TestSMTP_Storage_TransientExhaustion_Queues250(t *testing.T) {
	shortRetryDelays(t)
	f := startListener(t, baseTestConfig())
	gate := attachTestGate(t, f)
	transient := &transport.TransportError{Class: transport.ErrTransient, Message: "upstream 503"}
	f.mt.errQueue = []error{transient, transient} // inline attempt + FR19 retry

	code, msg := deliverBasic(t, f)
	if code != 250 {
		t.Fatalf("DATA reply = %d %q — queued submissions answer 250; a 451 would make the client double-send", code, msg)
	}
	if !strings.Contains(msg, "accepted for retry") {
		t.Errorf("reply message = %q", msg)
	}
	subs, _ := gate.Store().ListSubmissions(10)
	if len(subs) != 1 || subs[0].Status != storage.StatusQueued {
		t.Fatalf("row = %+v", subs)
	}
	if n, _ := gate.Store().QueueDepth(); n != 1 {
		t.Errorf("queue depth = %d", n)
	}
}

func TestSMTP_Storage_TerminalFailure_451AndFailedRow(t *testing.T) {
	f := startListener(t, baseTestConfig())
	gate := attachTestGate(t, f)
	f.mt.errQueue = []error{
		&transport.TransportError{Class: transport.ErrTerminal, Status: 422, Message: "bad address"},
	}

	code, _ := deliverBasic(t, f)
	if code != 451 {
		t.Fatalf("DATA reply = %d, want 451 (terminal never queues)", code)
	}
	subs, _ := gate.Store().ListSubmissions(10)
	if len(subs) != 1 || subs[0].Status != storage.StatusFailed {
		t.Fatalf("row = %+v", subs)
	}
	if n, _ := gate.Store().QueueDepth(); n != 0 {
		t.Errorf("queue depth = %d", n)
	}
}

func TestSMTP_Suppression_AllSuppressed_250NoSend(t *testing.T) {
	f := startListener(t, baseTestConfig())
	gate := attachTestGate(t, f)
	if err := gate.Store().AddSuppression("alice@somewhere.com", storage.ReasonHardBounce, "/x", time.Now()); err != nil {
		t.Fatal(err)
	}

	code, msg := deliverBasic(t, f)
	if code != 250 {
		t.Fatalf("DATA reply = %d %q — suppressed mail answers 250 so the client never retries", code, msg)
	}
	if !strings.Contains(msg, "suppressed") {
		t.Errorf("reply = %q", msg)
	}
	if got := f.mt.Sent(); len(got) != 0 {
		t.Fatalf("suppressed submission sent mail: %d", len(got))
	}
	subs, _ := gate.Store().ListSubmissions(10)
	if len(subs) != 1 || subs[0].Status != storage.StatusSuppressed {
		t.Fatalf("row = %+v", subs)
	}
}

func TestSMTP_Suppression_Mixed_SendsRemainder(t *testing.T) {
	f := startListener(t, baseTestConfig())
	gate := attachTestGate(t, f)
	if err := gate.Store().AddSuppression("bounced@somewhere.com", storage.ReasonSpamComplaint, "/x", time.Now()); err != nil {
		t.Fatal(err)
	}

	tp := f.dial()
	_ = tp.PrintfLine("EHLO client.test")
	expectMultiline(t, tp, 250)
	_ = tp.PrintfLine("AUTH PLAIN %s", authPlainCreds("user", "pass"))
	expect(t, tp, 235)
	_ = tp.PrintfLine("MAIL FROM:<noreply@example.com>")
	expect(t, tp, 250)
	_ = tp.PrintfLine("RCPT TO:<alice@somewhere.com>")
	expect(t, tp, 250)
	_ = tp.PrintfLine("RCPT TO:<bounced@somewhere.com>")
	expect(t, tp, 250)
	_ = tp.PrintfLine("DATA")
	expect(t, tp, 354)
	_ = tp.PrintfLine("From: noreply@example.com\r\nSubject: Hello\r\n\r\nBody text.\r\n.")
	expect(t, tp, 250)
	_ = tp.PrintfLine("QUIT")

	waitForSend(t, f.mt, 1, 500*time.Millisecond)
	sent := f.mt.Sent()
	if len(sent) != 1 {
		t.Fatalf("sends = %d", len(sent))
	}
	if len(sent[0].To) != 1 || sent[0].To[0] != "alice@somewhere.com" {
		t.Errorf("To = %v, want only the clean recipient", sent[0].To)
	}
}

func TestSMTP_Storage_Degraded_V1Behavior(t *testing.T) {
	shortRetryDelays(t)
	f := startListener(t, baseTestConfig())
	gate := attachTestGate(t, f)
	gate.ReportError(errors.New("disk full"))
	transient := &transport.TransportError{Class: transport.ErrTransient, Message: "upstream 503"}
	f.mt.errQueue = []error{transient, transient}

	code, _ := deliverBasic(t, f)
	if code != 451 {
		t.Fatalf("DATA reply = %d — degraded storage must reply 451 like v1.x (NFR27)", code)
	}
	if subs, _ := gate.Store().ListSubmissions(10); len(subs) != 0 {
		t.Errorf("degraded gate persisted rows: %d", len(subs))
	}
}

// TestSMTP_Storage_PassthroughHeadersSurviveTheQueue: a send that is
// queued after a provider failure must go out later with the same
// passthrough headers (FR99). Before the submission log stored them,
// the retry worker rebuilt the message without, and the mail left with
// no List-Unsubscribe while the client had been told 250.
func TestSMTP_Storage_PassthroughHeadersSurviveTheQueue(t *testing.T) {
	shortRetryDelays(t)
	cfg := baseTestConfig()
	cfg.PassthroughHeaders = []string{"List-Unsubscribe", "List-Unsubscribe-Post"}
	f := startListener(t, cfg)
	gate := attachTestGate(t, f)
	transient := &transport.TransportError{Class: transport.ErrTransient, Message: "upstream 503"}
	f.mt.errQueue = []error{transient, transient} // inline attempt + FR19 retry

	code := sendWithHeaderLines(t, f,
		"List-Unsubscribe: <https://lists.example/u/abc>\r\nList-Unsubscribe-Post: List-Unsubscribe=One-Click")
	if code != 250 {
		t.Fatalf("DATA reply = %d, want 250 (queued)", code)
	}
	want := []transport.Header{
		{Name: "List-Unsubscribe", Value: "<https://lists.example/u/abc>"},
		{Name: "List-Unsubscribe-Post", Value: "List-Unsubscribe=One-Click"},
	}

	subs, err := gate.Store().ListSubmissions(10)
	if err != nil || len(subs) != 1 || subs[0].Status != storage.StatusQueued {
		t.Fatalf("row = %+v err=%v", subs, err)
	}
	if got := subs[0].Headers; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("stored Headers = %+v, want %+v", got, want)
	}

	// Replay it the way serve does: the worker claims the row and hands
	// the rebuilt message to the listener's transport.
	var replayed []transport.Message
	worker := &storage.Worker{
		Store:  gate.Store(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return time.Now().Add(storage.RetryBackoff[0] + time.Minute) },
		Send: func(ctx context.Context, endpoint string, msg transport.Message) (transport.SendResult, error) {
			replayed = append(replayed, msg)
			return transport.SendResult{MessageID: "pm-retry"}, nil
		},
	}
	worker.ProcessDue(context.Background(), storage.Hooks{})
	if len(replayed) != 1 {
		t.Fatalf("worker replayed %d messages, want 1", len(replayed))
	}
	if got := replayed[0].Headers; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("replayed Headers = %+v, want %+v", got, want)
	}
	if err := transport.ValidateHeaders(replayed[0].Headers); err != nil {
		t.Errorf("replayed headers must pass the transports' own check: %v", err)
	}
}
