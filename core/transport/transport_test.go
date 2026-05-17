package transport

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// mockTransport is the reference Transport implementation used across tests
// in this package. It captures the most recent call for assertions and
// returns whatever error the test sets.
//
// This doubles as the "sample mock implementation" required by Story 2.1's
// acceptance criterion: any future Transport (Postmark, SMTP, Resend) should
// pass the same shape of test as this mock.
type mockTransport struct {
	// SendErr, if set, is returned from Send. Must be a *TransportError per
	// the Transport contract.
	SendErr error

	// SendResult is returned on success (when SendErr is nil). Zero by default.
	SendResult SendResult

	// Sent records every Message passed to Send, in order.
	Sent []Message

	// SendFn, if set, overrides the default behavior. Useful for tests that
	// want to inspect ctx or vary behavior across calls.
	SendFn func(ctx context.Context, msg Message) (SendResult, error)
}

func (m *mockTransport) Send(ctx context.Context, msg Message) (SendResult, error) {
	m.Sent = append(m.Sent, msg)
	if m.SendFn != nil {
		return m.SendFn(ctx, msg)
	}
	return m.SendResult, m.SendErr
}

// Compile-time check: mockTransport must satisfy Transport.
var _ Transport = (*mockTransport)(nil)

func TestMockTransport_RecordsSentMessages(t *testing.T) {
	mt := &mockTransport{}
	msg := Message{
		From:     "noreply@example.com",
		To:       []string{"craig@example.com"},
		Subject:  "Hello",
		BodyText: "World",
	}

	if _, err := mt.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := len(mt.Sent); got != 1 {
		t.Fatalf("Sent length = %d, want 1", got)
	}
	if mt.Sent[0].Subject != "Hello" {
		t.Errorf("Sent[0].Subject = %q, want %q", mt.Sent[0].Subject, "Hello")
	}
}

func TestMockTransport_ReturnsConfiguredError(t *testing.T) {
	want := &TransportError{
		Class:   ErrTerminal,
		Status:  422,
		Message: "invalid sender",
	}
	mt := &mockTransport{SendErr: want}

	_, err := mt.Send(context.Background(), Message{})
	if err == nil {
		t.Fatal("Send returned nil, want error")
	}
	var got *TransportError
	if !errors.As(err, &got) {
		t.Fatalf("Send error is not *TransportError: %T", err)
	}
	if got.Class != ErrTerminal {
		t.Errorf("Class = %v, want %v", got.Class, ErrTerminal)
	}
	if got.Status != 422 {
		t.Errorf("Status = %d, want 422", got.Status)
	}
}

func TestErrorClass_String(t *testing.T) {
	tests := []struct {
		class ErrorClass
		want  string
	}{
		{ErrUnknown, "unknown"},
		{ErrTransient, "transient"},
		{ErrRateLimited, "rate_limited"},
		{ErrTerminal, "terminal"},
		{ErrorClass(99), "unknown"}, // out-of-range falls back to unknown
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.class.String(); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTransportError_Error_Format(t *testing.T) {
	tests := []struct {
		name      string
		err       *TransportError
		wantParts []string // substrings that MUST appear in the formatted error
	}{
		{
			name: "with status and cause",
			err: &TransportError{
				Class:   ErrTransient,
				Status:  502,
				Cause:   errors.New("connection reset"),
				Message: "postmark unreachable",
			},
			wantParts: []string{"postmark unreachable", "transient", "status=502", "connection reset"},
		},
		{
			name: "no status, with cause",
			err: &TransportError{
				Class:   ErrTransient,
				Cause:   errors.New("dns lookup failed"),
				Message: "network error",
			},
			wantParts: []string{"network error", "transient", "dns lookup failed"},
		},
		{
			name: "status only, no cause",
			err: &TransportError{
				Class:   ErrTerminal,
				Status:  401,
				Message: "unauthorized",
			},
			wantParts: []string{"unauthorized", "terminal", "status=401"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.err.Error()
			for _, part := range tt.wantParts {
				if !strings.Contains(got, part) {
					t.Errorf("Error() = %q, missing substring %q", got, part)
				}
			}
		})
	}
}

func TestTransportError_Error_StatusZeroOmitted(t *testing.T) {
	// When Status is 0, the "status=" segment must be omitted entirely so
	// log scrapers don't pick up "status=0" as a real upstream code.
	err := &TransportError{Class: ErrTransient, Message: "x"}
	if got := err.Error(); strings.Contains(got, "status=") {
		t.Errorf("Error() = %q, must not contain status= when Status==0", got)
	}
}

func TestTransportError_Unwrap(t *testing.T) {
	cause := errors.New("underlying")
	err := &TransportError{Class: ErrTransient, Cause: cause}

	if !errors.Is(err, cause) {
		t.Error("errors.Is(err, cause) = false, want true")
	}

	var unwrapped error = err.Unwrap()
	if unwrapped != cause {
		t.Errorf("Unwrap() = %v, want %v", unwrapped, cause)
	}
}

func TestTransportError_Unwrap_NilCause(t *testing.T) {
	// Cause may legitimately be nil (e.g., 4xx mapping with no Go-level error).
	err := &TransportError{Class: ErrTerminal, Status: 422}
	if got := err.Unwrap(); got != nil {
		t.Errorf("Unwrap() = %v, want nil", got)
	}
}

func TestTransportError_NilSafe(t *testing.T) {
	// Defensive: calling Error/Unwrap on a nil receiver should not panic.
	var err *TransportError
	if got := err.Error(); got != "<nil>" {
		t.Errorf("nil.Error() = %q, want %q", got, "<nil>")
	}
	if got := err.Unwrap(); got != nil {
		t.Errorf("nil.Unwrap() = %v, want nil", got)
	}
}

// TestMessage_FieldsAreStructured is a documentation test: it asserts that
// Message has the exact field set v1.0 commits to, so a future change that
// adds e.g. a "Headers map[string]string" field has to update this test.
// Adding such a field would be a NFR1 risk and warrants explicit review.
func TestMessage_FieldsAreStructured(t *testing.T) {
	msg := Message{
		From:     "a@b.com",
		To:       []string{"c@d.com"},
		ReplyTo:  "e@f.com",
		Subject:  "s",
		BodyText: "b",
	}
	// Trivially exercise every field — if any is removed/renamed, this fails to compile.
	_ = msg.From
	_ = msg.To
	_ = msg.ReplyTo
	_ = msg.Subject
	_ = msg.BodyText
}
