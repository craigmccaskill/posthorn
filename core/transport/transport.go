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
	// ingress (ADR-27, FR97-FR99): at most one per allowlisted name, each
	// value a single line of printable ASCII. The ingress rejects a
	// message whose value isn't, and the retry queue stores Headers and
	// replays them. Every mail transport calls ValidateHeaders at the top
	// of Send and refuses the message on failure, so nothing reaches a
	// provider on the strength of an earlier layer's check (NFR1, NFR32),
	// then emits them through the provider's structured custom-header
	// mechanism. The webhook transport ignores them. HTTP ingresses leave
	// this nil.
	Headers []Header
}

// Header is one passthrough header (see Message.Headers). The JSON names
// are the wire form Postmark and SES take, and what the submission log
// stores.
type Header struct {
	Name  string `json:"Name"`
	Value string `json:"Value"`
}

// maxHeaderLine is RFC 5322's limit on one header line, CRLF excluded.
// It is also the tightest provider limit: SESv2 caps name plus value at
// 996 characters, which is this line minus the ": " separator.
const maxHeaderLine = 998

// PassthroughNames canonicalizes a listener's passthrough_headers list
// and refuses any name that is off the allowlist or listed twice (FR97).
// The config package and the SMTP listener both call it, so the two
// can't disagree about what a valid list is.
func PassthroughNames(names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for i, name := range names {
		canon, ok := PassthroughHeader(name)
		if !ok {
			return nil, fmt.Errorf("passthrough_headers[%d] %q: not on the allowlist (%s)", i, name, strings.Join(PassthroughHeaderAllowlist, ", "))
		}
		if seen[canon] {
			return nil, fmt.Errorf("passthrough_headers[%d] %q: duplicate", i, name)
		}
		seen[canon] = true
		out = append(out, canon)
	}
	return out, nil
}

// CheckHeaderValue reports why value can't be carried as the value of
// the header name, or nil. A value must be non-empty, printable ASCII
// (0x20-0x7E), and short enough that "name: value" fits one header
// line. That excludes CR and LF, which is the header-injection check
// (NFR1), along with other control bytes and raw 8-bit data that a
// header may not contain unencoded. The ingress and every mail
// transport apply the same rule, so a message is refused at the door
// rather than by one provider and not another.
func CheckHeaderValue(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s: empty value", name)
	}
	for i := 0; i < len(value); i++ {
		switch c := value[i]; {
		case c == '\r' || c == '\n':
			return fmt.Errorf("%s: value contains CR or LF", name)
		case c < 0x20 || c > 0x7e:
			return fmt.Errorf("%s: value contains a byte outside printable ASCII (0x%02x)", name, c)
		}
	}
	if len(name)+len(": ")+len(value) > maxHeaderLine {
		return fmt.Errorf("%s: header line longer than %d characters", name, maxHeaderLine)
	}
	return nil
}

// ValidateHeaders checks Message.Headers against everything a transport
// relies on: each name is an allowlisted name in canonical form, no name
// repeats (RFC 2369, RFC 2919, and RFC 8058 allow one of each), and each
// value passes CheckHeaderValue.
func ValidateHeaders(headers []Header) error {
	seen := make(map[string]bool, len(headers))
	for _, h := range headers {
		if canon, ok := PassthroughHeader(h.Name); !ok || canon != h.Name {
			return fmt.Errorf("header %q is not on the passthrough allowlist", h.Name)
		}
		if seen[h.Name] {
			return fmt.Errorf("%s: more than one value", h.Name)
		}
		seen[h.Name] = true
		if err := CheckHeaderValue(h.Name, h.Value); err != nil {
			return err
		}
	}
	return nil
}

// checkHeaders is ValidateHeaders shaped for the top of a transport's
// Send: a terminal TransportError, or nil.
func checkHeaders(msg Message) error {
	if err := ValidateHeaders(msg.Headers); err != nil {
		return &TransportError{Class: ErrTerminal, Cause: err, Message: "passthrough header rejected"}
	}
	return nil
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
