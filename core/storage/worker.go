package storage

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"time"

	"github.com/craigmccaskill/posthorn/transport"
)

// SendFunc performs one delivery attempt for a queued submission. The
// cmd wiring supplies it: resolve the endpoint's transport, call Send.
// The background ladder IS the retry policy — implementations should
// not add inline retries of their own.
type SendFunc func(ctx context.Context, endpoint string, msg transport.Message) (transport.SendResult, error)

// Hooks are optional observation points for metrics wiring. Nil funcs
// are skipped. Endpoint is the only label-safe value passed (NFR24/30).
type Hooks struct {
	OnSent       func(endpoint string)
	OnRetryAgain func(endpoint string)
	OnDeadLetter func(endpoint string)
}

// Worker drains the retry queue: one goroutine per process (NFR26),
// polling on a jittered interval, claiming due entries and replaying
// them through SendFunc.
//
// Failure classification mirrors FR19-22: transient / rate-limited
// errors ladder via RecordRetryFailure; terminal errors (and the
// defensive unknown class) dead-letter immediately — the provider has
// said this send will never succeed.
type Worker struct {
	Store  *Store
	Send   SendFunc
	Logger *slog.Logger

	// Interval between polls. Zero means the 30s default. Each wait is
	// jittered ±20% so the poll never phase-locks with provider-side
	// rate windows.
	Interval time.Duration

	// BatchSize caps claims per poll. Zero means 10.
	BatchSize int

	// Now is injected for tests. Nil means time.Now.
	Now func() time.Time
}

func (w *Worker) interval() time.Duration {
	if w.Interval <= 0 {
		return 30 * time.Second
	}
	return w.Interval
}

func (w *Worker) batch() int {
	if w.BatchSize <= 0 {
		return 10
	}
	return w.BatchSize
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// Run polls until ctx is canceled. Storage errors are logged and the
// loop continues — a degraded disk must never crash the process
// (NFR27); the canary probe owns surfacing that state.
func (w *Worker) Run(ctx context.Context, hooks Hooks) {
	for {
		wait := w.interval()
		jitter := time.Duration((rand.Float64()*0.4 - 0.2) * float64(wait))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait + jitter):
		}
		w.ProcessDue(ctx, hooks)
	}
}

// ProcessDue performs one poll: claim due entries, attempt each,
// record outcomes. Exposed for deterministic tests and for a final
// drain during shutdown.
func (w *Worker) ProcessDue(ctx context.Context, hooks Hooks) {
	now := w.now()
	due, err := w.Store.ClaimDue(now, w.batch())
	if err != nil {
		w.Logger.Error("queue_claim_failed", slog.String("error", err.Error()))
		if len(due) == 0 {
			return
		}
		// Partial claim: attempt what we got rather than stranding
		// rows in "sending" until a restart.
	}

	for _, entry := range due {
		if ctx.Err() != nil {
			return
		}
		w.attempt(ctx, entry, hooks)
	}
}

func (w *Worker) attempt(ctx context.Context, entry DueRetry, hooks Hooks) {
	logger := w.Logger.With(
		slog.String("submission_id", entry.ID),
		slog.String("endpoint", entry.Endpoint),
		slog.Int("attempt", entry.Attempt),
	)

	msg := transport.Message{
		From:         entry.From,
		To:           entry.ToAddrs,
		ReplyTo:      entry.ReplyTo,
		Subject:      entry.Subject,
		BodyText:     entry.BodyText,
		BodyHTML:     entry.BodyHTML,
		SubmissionID: entry.ID,
		Fields:       entry.Fields,
		Headers:      entry.Headers,
	}
	for _, a := range entry.Attachments {
		msg.Attachments = append(msg.Attachments, transport.Attachment{
			Filename: a.Filename, ContentType: a.ContentType, Data: a.Data,
		})
	}

	res, err := w.Send(ctx, entry.Endpoint, msg)
	if err == nil {
		if serr := w.Store.MarkSent(entry.ID, res.MessageID, w.now()); serr != nil {
			logger.Error("queued_send_record_failed", slog.String("error", serr.Error()))
		}
		logger.Info("queued_send_sent", slog.String("transport_message_id", res.MessageID))
		if hooks.OnSent != nil {
			hooks.OnSent(entry.Endpoint)
		}
		return
	}

	var terr *transport.TransportError
	terminal := true // defensive: a non-TransportError contract bug is terminal
	if errors.As(err, &terr) {
		terminal = terr.Class != transport.ErrTransient && terr.Class != transport.ErrRateLimited
	}

	if terminal {
		if serr := w.Store.DeadLetter(entry.ID, err.Error()); serr != nil {
			logger.Error("queued_send_record_failed", slog.String("error", serr.Error()))
		}
		logger.Error("queued_send_dead_lettered", slog.String("error", err.Error()))
		if hooks.OnDeadLetter != nil {
			hooks.OnDeadLetter(entry.Endpoint)
		}
		return
	}

	dead, serr := w.Store.RecordRetryFailure(entry.ID, err.Error(), w.now())
	if serr != nil {
		logger.Error("queued_send_record_failed", slog.String("error", serr.Error()))
		return
	}
	if dead {
		logger.Error("queued_send_dead_lettered",
			slog.String("error", err.Error()),
			slog.String("reason", "max attempts exhausted"))
		if hooks.OnDeadLetter != nil {
			hooks.OnDeadLetter(entry.Endpoint)
		}
		return
	}
	logger.Warn("queued_send_retry_scheduled", slog.String("error", err.Error()))
	if hooks.OnRetryAgain != nil {
		hooks.OnRetryAgain(entry.Endpoint)
	}
}
