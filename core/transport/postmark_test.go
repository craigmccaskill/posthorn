package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// captureServer returns an httptest server that records the most recent
// request method, URL, headers, and body for assertions. The handler always
// responds with the configured status and body.
type captureServer struct {
	*httptest.Server
	method   string
	path     string
	headers  http.Header
	body     []byte
	hits     int
	respBody string
}

func newCaptureServer(t *testing.T, status int, respBody string) *captureServer {
	t.Helper()
	cs := &captureServer{respBody: respBody}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.hits++
		cs.method = r.Method
		cs.path = r.URL.Path
		cs.headers = r.Header.Clone()
		cs.body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(cs.Close)
	return cs
}

// goodMessage returns a baseline Message used across happy-path tests.
func goodMessage() Message {
	return Message{
		From:     "noreply@example.com",
		To:       []string{"craig@example.com"},
		Subject:  "Hello",
		BodyText: "Body text.",
	}
}

func TestPostmark_Success_200(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"ErrorCode":0,"Message":"OK","MessageID":"abc-123"}`)
	tp := NewPostmarkTransport("test-key", cs.URL)

	result, err := tp.Send(context.Background(), goodMessage())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if cs.hits != 1 {
		t.Errorf("hits = %d, want 1", cs.hits)
	}
	if result.MessageID != "abc-123" {
		t.Errorf("MessageID = %q, want %q", result.MessageID, "abc-123")
	}
}

func TestPostmark_Success_202(t *testing.T) {
	cs := newCaptureServer(t, http.StatusAccepted, `{"ErrorCode":0,"Message":"Accepted","MessageID":"queued-42"}`)
	tp := NewPostmarkTransport("test-key", cs.URL)

	result, err := tp.Send(context.Background(), goodMessage())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result.MessageID != "queued-42" {
		t.Errorf("MessageID = %q, want %q", result.MessageID, "queued-42")
	}
}

// TestPostmark_Success_NoMessageID covers the defensive degradation: when the
// response body lacks a MessageID (or fails to parse), Send still succeeds —
// Postmark accepted the message, so a missing ID only degrades logging.
func TestPostmark_Success_NoMessageID(t *testing.T) {
	for _, tt := range []struct {
		name     string
		respBody string
	}{
		{"empty_object", `{}`},
		{"missing_field", `{"ErrorCode":0,"Message":"OK"}`},
		{"unparseable", `not-json`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cs := newCaptureServer(t, http.StatusOK, tt.respBody)
			tp := NewPostmarkTransport("k", cs.URL)
			result, err := tp.Send(context.Background(), goodMessage())
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if result.MessageID != "" {
				t.Errorf("MessageID = %q, want empty", result.MessageID)
			}
		})
	}
}

func TestPostmark_RequestShape(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{}`)
	tp := NewPostmarkTransport("server-token-abc", cs.URL)

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
	if cs.path != postmarkSendPath {
		t.Errorf("path = %q, want %q", cs.path, postmarkSendPath)
	}
	if got := cs.headers.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := cs.headers.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q, want application/json", got)
	}
	if got := cs.headers.Get("X-Postmark-Server-Token"); got != "server-token-abc" {
		t.Errorf("X-Postmark-Server-Token = %q, want %q", got, "server-token-abc")
	}

	var body map[string]any
	if err := json.Unmarshal(cs.body, &body); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, cs.body)
	}
	if got := body["From"]; got != "a@example.com" {
		t.Errorf("From = %v", got)
	}
	if got := body["To"]; got != "b@example.com, c@example.com" {
		t.Errorf("To = %v, want comma-joined recipients", got)
	}
	if got := body["ReplyTo"]; got != "reply@example.com" {
		t.Errorf("ReplyTo = %v", got)
	}
	if got := body["Subject"]; got != "Subj" {
		t.Errorf("Subject = %v", got)
	}
	if got := body["TextBody"]; got != "Hello" {
		t.Errorf("TextBody = %v", got)
	}
}

func TestPostmark_OmitsEmptyReplyTo(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{}`)
	tp := NewPostmarkTransport("k", cs.URL)

	msg := goodMessage()
	msg.ReplyTo = ""
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var body map[string]any
	_ = json.Unmarshal(cs.body, &body)
	if _, present := body["ReplyTo"]; present {
		t.Errorf("ReplyTo present in body when message field empty: %s", cs.body)
	}
}

func TestPostmark_5xx_Transient(t *testing.T) {
	for _, status := range []int{500, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			cs := newCaptureServer(t, status, `{"ErrorCode":500,"Message":"Server is having a bad day"}`)
			tp := NewPostmarkTransport("k", cs.URL)

			_, err := tp.Send(context.Background(), goodMessage())
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

func TestPostmark_429_RateLimited(t *testing.T) {
	cs := newCaptureServer(t, http.StatusTooManyRequests, `{"ErrorCode":429,"Message":"Rate limit hit"}`)
	tp := NewPostmarkTransport("k", cs.URL)

	_, err := tp.Send(context.Background(), goodMessage())
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a *TransportError: %v", err)
	}
	if te.Class != ErrRateLimited {
		t.Errorf("Class = %v, want ErrRateLimited", te.Class)
	}
	if te.Status != http.StatusTooManyRequests {
		t.Errorf("Status = %d, want 429", te.Status)
	}
}

func TestPostmark_4xx_Terminal(t *testing.T) {
	// 422 is the most common — invalid sender signature, malformed payload.
	for _, status := range []int{400, 401, 403, 422} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			cs := newCaptureServer(t, status, `{"ErrorCode":10,"Message":"Bad request"}`)
			tp := NewPostmarkTransport("k", cs.URL)

			_, err := tp.Send(context.Background(), goodMessage())
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

func TestPostmark_NetworkError_Transient(t *testing.T) {
	// Point at a closed server to force a connection error.
	cs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	url := cs.URL
	cs.Close() // immediately close so dialing the URL fails

	tp := NewPostmarkTransport("k", url)
	_, err := tp.Send(context.Background(), goodMessage())

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

func TestPostmark_ContextDeadline_Transient(t *testing.T) {
	// Server that intentionally hangs longer than the test's context deadline.
	cs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer cs.Close()

	tp := NewPostmarkTransport("k", cs.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := tp.Send(ctx, goodMessage())
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

// TestPostmark_NoHeaderInjection is the NFR1/NFR2 acceptance test. For each
// payload, we send a message with CRLF-laden values and then assert two
// independent properties of the captured request:
//
//  1. The marshaled JSON body has exactly the expected key set. CRLF in a
//     value cannot create a sibling JSON key (e.g., a smuggled Bcc).
//  2. The literal string is preserved as JSON-string data within the field
//     it was injected into. No silent sanitization that could mask a real
//     issue elsewhere.
//
// Defense in depth: we also assert that the HTTP request itself has only
// the fixed set of outbound headers we set, none introduced by user input.
func TestPostmark_NoHeaderInjection(t *testing.T) {
	tests := []struct {
		name        string
		message     Message
		injectField string // JSON field where the payload should land verbatim
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
			injectField: "From",
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
			injectField: "Subject",
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
			injectField: "ReplyTo",
			payload:     "x@x.com\r\nBcc: target@victim.com",
		},
		{
			name: "lf_only_in_subject",
			message: Message{
				From:     "f@example.com",
				To:       []string{"r@example.com"},
				Subject:  "Hello\nBcc: target@victim.com",
				BodyText: "b",
			},
			injectField: "Subject",
			payload:     "Hello\nBcc: target@victim.com",
		},
		{
			name: "smuggled_xspoof_in_bodytext",
			message: Message{
				From:     "f@example.com",
				To:       []string{"r@example.com"},
				Subject:  "s",
				BodyText: "Jane\r\nX-Spoof: yes",
			},
			injectField: "TextBody",
			payload:     "Jane\r\nX-Spoof: yes",
		},
		{
			name: "crlf_in_to_recipient",
			message: Message{
				From:     "f@example.com",
				To:       []string{"r@example.com\r\nBcc: target@victim.com"},
				Subject:  "s",
				BodyText: "b",
			},
			injectField: "To",
			payload:     "r@example.com\r\nBcc: target@victim.com",
		},
	}

	allowedJSONKeys := map[string]bool{
		"From": true, "To": true, "ReplyTo": true, "Subject": true, "TextBody": true,
	}

	expectedHTTPHeaders := []string{"Content-Type", "Accept", "X-Postmark-Server-Token"}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := newCaptureServer(t, http.StatusOK, `{}`)
			tp := NewPostmarkTransport("k", cs.URL)

			if _, err := tp.Send(context.Background(), tt.message); err != nil {
				t.Fatalf("Send: %v", err)
			}

			// Property 1: JSON body has exactly the expected keys; CRLF
			// could not have synthesized a smuggled sibling key.
			var body map[string]any
			if err := json.Unmarshal(cs.body, &body); err != nil {
				t.Fatalf("body not valid JSON: %v\n%s", err, cs.body)
			}
			for key := range body {
				if !allowedJSONKeys[key] {
					t.Errorf("smuggled JSON key %q in request body: %s", key, cs.body)
				}
			}

			// Property 2: payload preserved verbatim as a JSON string value.
			got, ok := body[tt.injectField].(string)
			if !ok {
				t.Fatalf("field %q missing or not a string: %v", tt.injectField, body[tt.injectField])
			}
			if got != tt.payload {
				t.Errorf("%s = %q, want %q (verbatim, unsanitized)",
					tt.injectField, got, tt.payload)
			}

			// Defense in depth: outbound HTTP headers must be exactly the
			// three we set. CRLF in input must not have created HTTP-level
			// header smuggling.
			for headerName := range cs.headers {
				if headerName == "Accept-Encoding" ||
					headerName == "User-Agent" ||
					headerName == "Content-Length" ||
					headerName == "Host" {
					continue // set by net/http, not by us
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

// TestPostmark_APIKeyNotInURLOrBody is part of the NFR3 surface. A stronger
// "key never appears in logs" check lands when logging exists. For now we
// verify the key is only ever in the X-Postmark-Server-Token header.
func TestPostmark_APIKeyNotInURLOrBody(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{}`)
	const apiKey = "very-secret-token-do-not-leak"
	tp := NewPostmarkTransport(apiKey, cs.URL)

	if _, err := tp.Send(context.Background(), goodMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if cs.headers.Get("X-Postmark-Server-Token") != apiKey {
		t.Errorf("X-Postmark-Server-Token = %q, want %q", cs.headers.Get("X-Postmark-Server-Token"), apiKey)
	}
	if strings.Contains(cs.path, apiKey) {
		t.Errorf("API key appeared in URL path: %s", cs.path)
	}
	if strings.Contains(string(cs.body), apiKey) {
		t.Errorf("API key appeared in request body: %s", cs.body)
	}
}

func TestNewPostmarkTransport_DefaultBaseURL(t *testing.T) {
	tp := NewPostmarkTransport("k", "")
	if tp.BaseURL != defaultPostmarkBaseURL {
		t.Errorf("BaseURL = %q, want %q", tp.BaseURL, defaultPostmarkBaseURL)
	}
}

func TestNewPostmarkTransport_CustomBaseURL(t *testing.T) {
	tp := NewPostmarkTransport("k", "https://custom.example.com")
	if tp.BaseURL != "https://custom.example.com" {
		t.Errorf("BaseURL = %q, want override", tp.BaseURL)
	}
}

func TestPostmarkErrorMessage(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		fallback string
		want     string
	}{
		{"valid_with_message", `{"ErrorCode":10,"Message":"Sender not verified"}`, "fallback", "Sender not verified"},
		{"valid_empty_message", `{"ErrorCode":10,"Message":""}`, "fallback", "fallback"},
		{"invalid_json", `<html>503</html>`, "fallback", "fallback"},
		{"empty_body", ``, "fallback", "fallback"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := postmarkErrorMessage([]byte(tt.body), tt.fallback)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestPostmark_UnexpectedStatus_Terminal covers the default branch of the
// status switch (e.g., a 3xx redirect we don't follow, or a 1xx).
func TestPostmark_UnexpectedStatus_Terminal(t *testing.T) {
	cs := newCaptureServer(t, http.StatusMultipleChoices, `{}`)
	tp := NewPostmarkTransport("k", cs.URL)

	_, err := tp.Send(context.Background(), goodMessage())
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a *TransportError: %v", err)
	}
	if te.Class != ErrTerminal {
		t.Errorf("Class = %v, want ErrTerminal", te.Class)
	}
}
