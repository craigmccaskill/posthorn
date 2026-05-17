package transport

import (
	"context"
	"fmt"
)

// Message is the canonical form of an email passed to a Transport.
//
// All fields are structured data. Transport implementations MUST pass these
// fields to their underlying email API as separate, structured values — never
// concatenate them into raw protocol headers. This is the architectural
// enforcement of NFR1 (no header injection): if every transport handles
// header construction through library APIs that escape headers properly,
// submitter-controlled content cannot smuggle headers into outbound mail.
//
// BodyHTML is intentionally absent in v1.0; markdown/HTML body support is
// deferred to v2.
type Message struct {
	From     string
	To       []string
	ReplyTo  string
	Subject  string
	BodyText string
}

// SendResult is the per-call metadata returned from a successful Send.
//
// MessageID is the upstream provider's identifier for the message — e.g.,
// Postmark's response.MessageID, SES's MessageId, Mailgun's id. Posthorn
// surfaces it in the submission_sent log so an operator triaging a missing
// email can grep posthorn logs for the submission UUID and jump straight
// to the provider's UI to inspect delivery state.
//
// Empty when the transport doesn't expose a message ID, when parsing it
// failed (a non-fatal degradation), or on transports that haven't grown
// the support yet.
type SendResult struct {
	MessageID string
}

// Transport sends a Message. v1.0 ships one implementation (Postmark);
// the interface exists so Resend, Mailgun, SES, and outbound SMTP can
// be added in v1.1+ without touching handler logic or config schema (FR4).
//
// Implementations MUST return a *TransportError on failure so the handler
// can classify retries (FR18-20). Returning a bare error is a contract bug.
// On success, implementations SHOULD populate SendResult.MessageID when the
// upstream provider exposes one; an empty MessageID is acceptable.
type Transport interface {
	Send(ctx context.Context, msg Message) (SendResult, error)
}

// ErrorClass classifies transport errors for the retry policy.
//
// The handler maps each class to a retry decision:
//
//	ErrTransient    → retry once after 1s   (network errors, 5xx)
//	ErrRateLimited  → retry once after 5s   (429 from upstream)
//	ErrTerminal     → no retry, log + 502   (4xx other than 429)
//	ErrUnknown      → treated as terminal   (defensive default)
type ErrorClass int

const (
	// ErrUnknown is the zero value. A TransportError with this class is a
	// contract bug — implementations should always set a specific class.
	ErrUnknown ErrorClass = iota

	// ErrTransient is for failures that may succeed on retry: network errors,
	// connection timeouts, upstream 5xx responses.
	ErrTransient

	// ErrRateLimited is for 429 responses from the upstream provider. Retried
	// after a longer backoff to give the provider time to recover.
	ErrRateLimited

	// ErrTerminal is for failures that won't succeed on retry: 4xx responses
	// (other than 429), malformed config, authentication errors.
	ErrTerminal
)

// String returns a stable label for log fields (NFR7 error_class).
func (c ErrorClass) String() string {
	switch c {
	case ErrTransient:
		return "transient"
	case ErrRateLimited:
		return "rate_limited"
	case ErrTerminal:
		return "terminal"
	default:
		return "unknown"
	}
}

// TransportError is the error type all Transport implementations must return
// on failure. Wraps an underlying cause and carries metadata the handler
// uses for retry classification and structured logging.
type TransportError struct {
	// Class is the retry classification. Required.
	Class ErrorClass

	// Status is the upstream HTTP status code if applicable, 0 otherwise.
	Status int

	// Cause is the underlying error (network failure, JSON decode error, etc.).
	// May be nil when the error originates from a status-code mapping with no
	// underlying Go error.
	Cause error

	// Message is a short operator-facing description. Should not contain
	// secrets (API keys, request bodies). NFR3.
	Message string
}

// Error implements the error interface. Format is intentionally compact and
// safe to log: "transport: <message> (class=<class> status=<status>): <cause>".
func (e *TransportError) Error() string {
	if e == nil {
		return "<nil>"
	}
	base := fmt.Sprintf("transport: %s (class=%s", e.Message, e.Class)
	if e.Status != 0 {
		base += fmt.Sprintf(" status=%d", e.Status)
	}
	base += ")"
	if e.Cause != nil {
		base += ": " + e.Cause.Error()
	}
	return base
}

// Unwrap supports errors.Is / errors.As traversal to the underlying cause.
func (e *TransportError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}
