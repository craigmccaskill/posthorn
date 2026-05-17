package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// goodResendMessage returns a baseline Message used across happy-path tests.
func goodResendMessage() Message {
	return Message{
		From:     "noreply@example.com",
		To:       []string{"craig@example.com"},
		Subject:  "Hello",
		BodyText: "Body text.",
	}
}

func TestResend_Success_200(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"id":"resend-msg-abc-123"}`)
	tp := NewResendTransport("test-key", cs.URL)

	result, err := tp.Send(context.Background(), goodResendMessage())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if cs.hits != 1 {
		t.Errorf("hits = %d, want 1", cs.hits)
	}
	if result.MessageID != "resend-msg-abc-123" {
		t.Errorf("MessageID = %q, want %q", result.MessageID, "resend-msg-abc-123")
	}
}

// TestResend_Success_NoMessageID covers the defensive degradation: when the
// response body lacks an id (or fails to parse), Send still succeeds —
// Resend already accepted the message, so a missing ID only degrades logging.
func TestResend_Success_NoMessageID(t *testing.T) {
	for _, tt := range []struct {
		name     string
		respBody string
	}{
		{"empty_object", `{}`},
		{"missing_field", `{"unrelated":"value"}`},
		{"unparseable", `not-json`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cs := newCaptureServer(t, http.StatusOK, tt.respBody)
			tp := NewResendTransport("k", cs.URL)
			result, err := tp.Send(context.Background(), goodResendMessage())
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if result.MessageID != "" {
				t.Errorf("MessageID = %q, want empty", result.MessageID)
			}
		})
	}
}

func TestResend_RequestShape(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"id":"x"}`)
	tp := NewResendTransport("re-token-abc", cs.URL)

	msg := Message{
		From:     "a@example.com",
		To:       []string{"b@example.com", "c@example.com"},
		ReplyTo:  "reply@example.com",
		Subject:  "Subj",
		BodyText: "Hello",
	}
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if cs.method != http.MethodPost {
		t.Errorf("method = %q, want POST", cs.method)
	}
	if cs.path != resendSendPath {
		t.Errorf("path = %q, want %q", cs.path, resendSendPath)
	}
	if got := cs.headers.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := cs.headers.Get("Authorization"); got != "Bearer re-token-abc" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer re-token-abc")
	}

	var body map[string]any
	if err := json.Unmarshal(cs.body, &body); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, cs.body)
	}
	if got := body["from"]; got != "a@example.com" {
		t.Errorf("from = %v", got)
	}
	to, ok := body["to"].([]any)
	if !ok || len(to) != 2 || to[0] != "b@example.com" || to[1] != "c@example.com" {
		t.Errorf("to = %v, want [b@example.com, c@example.com]", body["to"])
	}
	if got := body["reply_to"]; got != "reply@example.com" {
		t.Errorf("reply_to = %v", got)
	}
	if got := body["subject"]; got != "Subj" {
		t.Errorf("subject = %v", got)
	}
	if got := body["text"]; got != "Hello" {
		t.Errorf("text = %v", got)
	}
}

func TestResend_OmitsEmptyReplyTo(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"id":"x"}`)
	tp := NewResendTransport("k", cs.URL)

	msg := goodResendMessage()
	msg.ReplyTo = ""
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var body map[string]any
	_ = json.Unmarshal(cs.body, &body)
	if _, present := body["reply_to"]; present {
		t.Errorf("reply_to present in body when message field empty: %s", cs.body)
	}
}

func TestResend_5xx_Transient(t *testing.T) {
	for _, status := range []int{500, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			cs := newCaptureServer(t, status, `{"statusCode":500,"message":"Server is having a bad day","name":"InternalError"}`)
			tp := NewResendTransport("k", cs.URL)

			_, err := tp.Send(context.Background(), goodResendMessage())
			var te *TransportError
			if !errors.As(err, &te) {
				t.Fatalf("not a *TransportError: %v", err)
			}
			if te.Class != ErrTransient {
				t.Errorf("Class = %v, want ErrTransient", te.Class)
			}
			if te.Status != status {
				t.Errorf("Status = %d, want %d", te.Status, status)
			}
			if !strings.Contains(te.Message, "Server is having a bad day") {
				t.Errorf("Message = %q, want it to surface upstream message", te.Message)
			}
		})
	}
}

func TestResend_429_RateLimited(t *testing.T) {
	cs := newCaptureServer(t, http.StatusTooManyRequests, `{"statusCode":429,"message":"Rate limit exceeded","name":"RateLimitExceeded"}`)
	tp := NewResendTransport("k", cs.URL)

	_, err := tp.Send(context.Background(), goodResendMessage())
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a *TransportError: %v", err)
	}
	if te.Class != ErrRateLimited {
		t.Errorf("Class = %v, want ErrRateLimited", te.Class)
	}
}

func TestResend_4xx_Terminal(t *testing.T) {
	for _, status := range []int{400, 401, 403, 422} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			cs := newCaptureServer(t, status, `{"statusCode":400,"message":"Bad request","name":"ValidationError"}`)
			tp := NewResendTransport("k", cs.URL)

			_, err := tp.Send(context.Background(), goodResendMessage())
			var te *TransportError
			if !errors.As(err, &te) {
				t.Fatalf("not a *TransportError: %v", err)
			}
			if te.Class != ErrTerminal {
				t.Errorf("Class = %v, want ErrTerminal", te.Class)
			}
			if te.Status != status {
				t.Errorf("Status = %d, want %d", te.Status, status)
			}
		})
	}
}

func TestResend_NetworkError_Transient(t *testing.T) {
	cs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	url := cs.URL
	cs.Close()

	tp := NewResendTransport("k", url)
	_, err := tp.Send(context.Background(), goodResendMessage())

	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a *TransportError: %v", err)
	}
	if te.Class != ErrTransient {
		t.Errorf("Class = %v, want ErrTransient", te.Class)
	}
	if te.Cause == nil {
		t.Error("Cause is nil; want underlying network error")
	}
}

func TestResend_ContextDeadline_Transient(t *testing.T) {
	cs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer cs.Close()

	tp := NewResendTransport("k", cs.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := tp.Send(ctx, goodResendMessage())
	elapsed := time.Since(start)

	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a *TransportError: %v", err)
	}
	if te.Class != ErrTransient {
		t.Errorf("Class = %v, want ErrTransient", te.Class)
	}
	if elapsed > 1*time.Second {
		t.Errorf("Send took %v; context deadline should have fired faster", elapsed)
	}
}

// TestResend_NoHeaderInjection is the NFR1/NFR2 / FR52 acceptance test
// for the Resend transport. Identical shape to TestPostmark_NoHeaderInjection.
func TestResend_NoHeaderInjection(t *testing.T) {
	tests := []struct {
		name        string
		message     Message
		injectField string
		payload     string
	}{
		{
			name: "crlf_bcc_in_from",
			message: Message{
				From:     "attacker@evil.com\r\nBcc: target@victim.com",
				To:       []string{"r@example.com"},
				Subject:  "s",
				BodyText: "b",
			},
			injectField: "from",
			payload:     "attacker@evil.com\r\nBcc: target@victim.com",
		},
		{
			name: "crlf_bcc_in_subject",
			message: Message{
				From:     "f@example.com",
				To:       []string{"r@example.com"},
				Subject:  "Hello\r\nBcc: target@victim.com",
				BodyText: "b",
			},
			injectField: "subject",
			payload:     "Hello\r\nBcc: target@victim.com",
		},
		{
			name: "crlf_in_replyto",
			message: Message{
				From:     "f@example.com",
				To:       []string{"r@example.com"},
				ReplyTo:  "x@x.com\r\nBcc: target@victim.com",
				Subject:  "s",
				BodyText: "b",
			},
			injectField: "reply_to",
			payload:     "x@x.com\r\nBcc: target@victim.com",
		},
		{
			name: "crlf_in_to_recipient",
			message: Message{
				From:     "f@example.com",
				To:       []string{"r@example.com\r\nBcc: target@victim.com"},
				Subject:  "s",
				BodyText: "b",
			},
			injectField: "to",
			payload:     "r@example.com\r\nBcc: target@victim.com",
		},
	}

	allowedJSONKeys := map[string]bool{
		"from": true, "to": true, "reply_to": true, "subject": true, "text": true,
	}

	expectedHTTPHeaders := []string{"Content-Type", "Accept", "Authorization"}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := newCaptureServer(t, http.StatusOK, `{"id":"x"}`)
			tp := NewResendTransport("k", cs.URL)

			if _, err := tp.Send(context.Background(), tt.message); err != nil {
				t.Fatalf("Send: %v", err)
			}

			var body map[string]any
			if err := json.Unmarshal(cs.body, &body); err != nil {
				t.Fatalf("body not valid JSON: %v\n%s", err, cs.body)
			}
			for key := range body {
				if !allowedJSONKeys[key] {
					t.Errorf("smuggled JSON key %q in request body: %s", key, cs.body)
				}
			}

			// Payload preserved verbatim. Handle the "to" array case separately.
			if tt.injectField == "to" {
				to, _ := body["to"].([]any)
				if len(to) != 1 || to[0] != tt.payload {
					t.Errorf("to = %v, want [%q] (verbatim, unsanitized)", to, tt.payload)
				}
			} else {
				got, ok := body[tt.injectField].(string)
				if !ok {
					t.Fatalf("field %q missing or not a string: %v", tt.injectField, body[tt.injectField])
				}
				if got != tt.payload {
					t.Errorf("%s = %q, want %q (verbatim, unsanitized)",
						tt.injectField, got, tt.payload)
				}
			}

			// Defense in depth: outbound HTTP headers must be exactly the
			// three we set.
			for headerName := range cs.headers {
				if headerName == "Accept-Encoding" ||
					headerName == "User-Agent" ||
					headerName == "Content-Length" ||
					headerName == "Host" {
					continue
				}
				ok := false
				for _, expected := range expectedHTTPHeaders {
					if headerName == expected {
						ok = true
						break
					}
				}
				if !ok {
					t.Errorf("unexpected outbound HTTP header %q (value %q)",
						headerName, cs.headers.Get(headerName))
				}
			}
		})
	}
}

// TestResend_APIKeyNotInURLOrBody is the NFR3 / FR53 surface. The Resend
// key is sent via Authorization: Bearer, never in path or body.
func TestResend_APIKeyNotInURLOrBody(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"id":"x"}`)
	const apiKey = "very-secret-resend-token-do-not-leak"
	tp := NewResendTransport(apiKey, cs.URL)

	if _, err := tp.Send(context.Background(), goodResendMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := cs.headers.Get("Authorization"); got != "Bearer "+apiKey {
		t.Errorf("Authorization = %q, want %q", got, "Bearer "+apiKey)
	}
	if strings.Contains(cs.path, apiKey) {
		t.Errorf("API key appeared in URL path: %s", cs.path)
	}
	if strings.Contains(string(cs.body), apiKey) {
		t.Errorf("API key appeared in request body: %s", cs.body)
	}
}

func TestNewResendTransport_DefaultBaseURL(t *testing.T) {
	tp := NewResendTransport("k", "")
	if tp.BaseURL != defaultResendBaseURL {
		t.Errorf("BaseURL = %q, want %q", tp.BaseURL, defaultResendBaseURL)
	}
}

func TestNewResendTransport_CustomBaseURL(t *testing.T) {
	tp := NewResendTransport("k", "https://custom.example.com")
	if tp.BaseURL != "https://custom.example.com" {
		t.Errorf("BaseURL = %q, want override", tp.BaseURL)
	}
}

func TestResendErrorMessage(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		fallback string
		want     string
	}{
		{"valid_with_message", `{"statusCode":400,"message":"Sender not verified","name":"InvalidSender"}`, "fallback", "Sender not verified"},
		{"valid_empty_message", `{"statusCode":400,"message":"","name":"x"}`, "fallback", "fallback"},
		{"invalid_json", `<html>503</html>`, "fallback", "fallback"},
		{"empty_body", ``, "fallback", "fallback"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resendErrorMessage([]byte(tt.body), tt.fallback)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResend_UnexpectedStatus_Terminal(t *testing.T) {
	cs := newCaptureServer(t, http.StatusMultipleChoices, `{}`)
	tp := NewResendTransport("k", cs.URL)

	_, err := tp.Send(context.Background(), goodResendMessage())
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a *TransportError: %v", err)
	}
	if te.Class != ErrTerminal {
		t.Errorf("Class = %v, want ErrTerminal", te.Class)
	}
}

// --- Registry integration ---

func TestResend_RegisteredAtPackageLoad(t *testing.T) {
	reg, ok := Lookup("resend")
	if !ok {
		t.Fatal("resend not registered after package init")
	}
	if err := reg.Validate(map[string]any{}); err == nil {
		t.Error("resend Validate should reject empty settings (missing api_key)")
	}
	if err := reg.Validate(map[string]any{"api_key": "x"}); err != nil {
		t.Errorf("resend Validate rejected valid settings: %v", err)
	}
	tp, err := reg.Build(map[string]any{"api_key": "x"})
	if err != nil {
		t.Fatalf("resend Build: %v", err)
	}
	if _, ok := tp.(*ResendTransport); !ok {
		t.Errorf("resend Build returned %T, want *ResendTransport", tp)
	}
}

func TestResend_BuildErrorOnEmptyAPIKey(t *testing.T) {
	reg, _ := Lookup("resend")
	_, err := reg.Build(map[string]any{})
	if err == nil {
		t.Fatal("expected error from Build with empty api_key")
	}
}
