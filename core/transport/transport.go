package transport

import (
	"context"
	"fmt"
	"net/textproto"
	"strings"
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
// BodyHTML is set only for endpoints configured with body_format = "html"
// (FR71). When non-empty, BodyText carries the plain-text alternative —
// transports send both as a multipart pair. BodyText is never empty on an
// HTML send: the renderer auto-derives it when no explicit text_body
// template is configured (FR72).
type Message struct {
	From     string
	To       []string
	ReplyTo  string
	Subject  string
	BodyText string
	BodyHTML string

	// SubmissionID and Fields cross the boundary for structured sinks
	// (ADR-24 amending ADR-12; FR89). SubmissionID is Posthorn's
	// submission UUID — set by both ingresses and the retry worker so
	// at-least-once replays carry a stable dedup key. Fields is the raw
	// submitted key/values, populated by HTTP ingresses and nil from the
	// SMTP listener. Mail transports MUST ignore both, and neither may
	// ever be interpolated into any header at any layer (NFR1).
	SubmissionID string
	Fields       map[string][]string

	// Attachments carries files accepted by an opt-in
	// [endpoints.attachments] endpoint (FR90-FR92, ADR-25). ContentType
	// is the SNIFFED type of Data — enforcement and outbound declaration
	// both use it; the client-declared type never crosses this boundary.
	// Filenames are structured API/MIME parameter values only, never
	// header-line material (NFR1).
	Attachments []Attachment

	// Headers carries allowlisted list-management headers from the SMTP
	// ingress (ADR-27, FR97-FR99). Only names on
	// PassthroughHeaderAllowlist ever appear; values are CR/LF-free by the
	// time they cross (the ingress rejects the message otherwise); mail
	// transports emit them through the provider's structured custom-header
	// mechanism and, where they build header lines themselves, re-check
	// for CR/LF (NFR1, NFR32). The webhook transport ignores them. HTTP
	// ingresses leave this nil.
	Headers []Header
}

// Header is one passthrough header (see Message.Headers).
type Header struct {
	Name  string
	Value string
}

// PassthroughHeaderAllowlist is the fixed set of headers a listener may
// carry through (FR97): list-management headers only. None of them names
// a recipient or a sender, so NFR22 is untouched by construction.
var PassthroughHeaderAllowlist = []string{"List-Unsubscribe", "List-Unsubscribe-Post", "List-Id"}

// PassthroughHeader canonicalizes name and reports whether it is on the
// allowlist. Config validation and the ingress both use it, so a name
// that isn't allowed can neither be configured nor slip through.
func PassthroughHeader(name string) (string, bool) {
	canon := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(name))
	for _, allowed := range PassthroughHeaderAllowlist {
		if allowed == canon {
			return canon, true
		}
	}
	return canon, false
}

// Attachment is one file crossing to a transport.
type Attachment struct {
	Filename    string
	ContentType string
	Data        []byte
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
