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

// goodMailChannelsMessage returns a baseline Message used across happy-path tests.
func goodMailChannelsMessage() Message {
	return Message{
		From:     "noreply@example.com",
		To:       []string{"craig@example.com"},
		Subject:  "Hello",
		BodyText: "Body text.",
	}
}

func TestMailChannels_Success_202(t *testing.T) {
	cs := newCaptureServer(t, http.StatusAccepted, `{"request_id":"req-1","results":[{"index":0,"message_id":"mc-msg-abc-123","status":"sent"}]}`)
	tp := NewMailChannelsTransport("test-key", cs.URL)

	result, err := tp.Send(context.Background(), goodMailChannelsMessage())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if cs.hits != 1 {
		t.Errorf("hits = %d, want 1", cs.hits)
	}
	if result.MessageID != "mc-msg-abc-123" {
		t.Errorf("MessageID = %q, want %q", result.MessageID, "mc-msg-abc-123")
	}
}

func TestMailChannels_Success_NoMessageID(t *testing.T) {
	for _, tt := range []struct {
		name     string
		respBody string
	}{
		{"empty_object", `{}`},
		{"empty_results", `{"request_id":"r","results":[]}`},
		{"missing_field", `{"results":[{"index":0,"status":"sent"}]}`},
		{"unparseable", `not-json`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cs := newCaptureServer(t, http.StatusAccepted, tt.respBody)
			tp := NewMailChannelsTransport("k", cs.URL)
			result, err := tp.Send(context.Background(), goodMailChannelsMessage())
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if result.MessageID != "" {
				t.Errorf("MessageID = %q, want empty", result.MessageID)
			}
		})
	}
}

func TestMailChannels_RequestShape(t *testing.T) {
	cs := newCaptureServer(t, http.StatusAccepted, `{"results":[{"index":0,"message_id":"x"}]}`)
	tp := NewMailChannelsTransport("mc-secret-key", cs.URL)

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
	if cs.path != mailchannelsSendPath {
		t.Errorf("path = %q, want %q", cs.path, mailchannelsSendPath)
	}
	if got := cs.headers.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := cs.headers.Get("X-Api-Key"); got != "mc-secret-key" {
		t.Errorf("X-Api-Key = %q, want the configured key", got)
	}

	var body map[string]any
	if err := json.Unmarshal(cs.body, &body); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, cs.body)
	}
	from, _ := body["from"].(map[string]any)
	if got := from["email"]; got != "a@example.com" {
		t.Errorf("from.email = %v, want a@example.com", got)
	}
	if _, present := from["name"]; present {
		t.Errorf("from.name present with no display name: %s", cs.body)
	}
	pers, _ := body["personalizations"].([]any)
	if len(pers) != 1 {
		t.Fatalf("personalizations = %v, want one", body["personalizations"])
	}
	to, _ := pers[0].(map[string]any)["to"].([]any)
	if len(to) != 2 {
		t.Fatalf("to = %v, want 2 recipients", pers[0])
	}
	if got := to[0].(map[string]any)["email"]; got != "b@example.com" {
		t.Errorf("to[0].email = %v, want b@example.com", got)
	}
	if got := to[1].(map[string]any)["email"]; got != "c@example.com" {
		t.Errorf("to[1].email = %v, want c@example.com", got)
	}
	replyTo, _ := body["reply_to"].(map[string]any)
	if got := replyTo["email"]; got != "reply@example.com" {
		t.Errorf("reply_to.email = %v, want reply@example.com", got)
	}
	if got := body["subject"]; got != "Subj" {
		t.Errorf("subject = %v, want Subj", got)
	}
	content, _ := body["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %v, want one text part", body["content"])
	}
	if got := content[0].(map[string]any)["type"]; got != "text/plain" {
		t.Errorf("content[0].type = %v, want text/plain", got)
	}
	if got := content[0].(map[string]any)["value"]; got != "Hello" {
		t.Errorf("content[0].value = %v, want Hello", got)
	}
}

// TestMailChannels_AddressSplit pins the RFC 5322 "Name <email>" split into
// MailChannels' {email, name} object, and the unparseable-address fallback:
// the raw string is carried verbatim in the email field as data.
func TestMailChannels_AddressSplit(t *testing.T) {
	cs := newCaptureServer(t, http.StatusAccepted, `{"results":[{"index":0,"message_id":"x"}]}`)
	tp := NewMailChannelsTransport("k", cs.URL)

	msg := Message{
		From:     `"Craig Mc" <craig@example.com>`,
		To:       []string{"Buddy <b@example.com>"},
		Subject:  "s",
		BodyText: "b",
	}
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var body struct {
		From struct {
			Email string `json:"email"`
			Name  string `json:"name"`
		} `json:"from"`
		Personalizations []struct {
			To []struct {
				Email string `json:"email"`
				Name  string `json:"name"`
			} `json:"to"`
		} `json:"personalizations"`
	}
	if err := json.Unmarshal(cs.body, &body); err != nil {
		t.Fatalf("json: %v", err)
	}
	if body.From.Email != "craig@example.com" || body.From.Name != "Craig Mc" {
		t.Errorf("from = %+v, want craig@example.com / Craig Mc", body.From)
	}
	got := body.Personalizations[0].To[0]
	if got.Email != "b@example.com" || got.Name != "Buddy" {
		t.Errorf("to[0] = %+v, want b@example.com / Buddy", got)
	}
}

func TestMailChannels_OmitsEmptyReplyTo(t *testing.T) {
	cs := newCaptureServer(t, http.StatusAccepted, `{"results":[{"index":0,"message_id":"x"}]}`)
	tp := NewMailChannelsTransport("k", cs.URL)

	msg := goodMailChannelsMessage()
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

func TestMailChannels_5xx_Transient(t *testing.T) {
	for _, status := range []int{500, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			cs := newCaptureServer(t, status, `{"errors":["Service is having a bad day"]}`)
			tp := NewMailChannelsTransport("k", cs.URL)

			_, err := tp.Send(context.Background(), goodMailChannelsMessage())
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
			if !strings.Contains(te.Message, "Service is having a bad day") {
				t.Errorf("Message = %q, want it to surface upstream message", te.Message)
			}
		})
	}
}

func TestMailChannels_429_RateLimited(t *testing.T) {
	cs := newCaptureServer(t, http.StatusTooManyRequests, `{"errors":["Rate limit exceeded"]}`)
	tp := NewMailChannelsTransport("k", cs.URL)

	_, err := tp.Send(context.Background(), goodMailChannelsMessage())
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a *TransportError: %v", err)
	}
	if te.Class != ErrRateLimited {
		t.Errorf("Class = %v, want ErrRateLimited", te.Class)
	}
}

// TestMailChannels_4xx_Terminal covers the MailChannels-documented rejection
// statuses (400 bad request, 403 bad key / unauthorized domain, 413 too
// large) plus the generic 4xx mapping.
func TestMailChannels_4xx_Terminal(t *testing.T) {
	for _, status := range []int{400, 401, 403, 413, 422} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			cs := newCaptureServer(t, status, `{"errors":["Bad request"]}`)
			tp := NewMailChannelsTransport("k", cs.URL)

			_, err := tp.Send(context.Background(), goodMailChannelsMessage())
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

func TestMailChannels_NetworkError_Transient(t *testing.T) {
	cs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	url := cs.URL
	cs.Close()

	tp := NewMailChannelsTransport("k", url)
	_, err := tp.Send(context.Background(), goodMailChannelsMessage())

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

func TestMailChannels_ContextDeadline_Transient(t *testing.T) {
	cs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusAccepted)
		case <-r.Context().Done():
		}
	}))
	defer cs.Close()

	tp := NewMailChannelsTransport("k", cs.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := tp.Send(ctx, goodMailChannelsMessage())
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

// TestMailChannels_NoHeaderInjection is the NFR1/NFR2 / FR52 acceptance test
// for the MailChannels transport. Identical shape to TestResend_NoHeaderInjection:
// each payload must reach the provider as verbatim data inside named JSON
// fields — never as request structure.
func TestMailChannels_NoHeaderInjection(t *testing.T) {
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
		"from": true, "to": true, "reply_to": true, "subject": true,
		"personalizations": true, "content": true,
	}

	expectedHTTPHeaders := []string{"Content-Type", "Accept", "X-Api-Key"}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := newCaptureServer(t, http.StatusAccepted, `{"results":[{"index":0,"message_id":"x"}]}`)
			tp := NewMailChannelsTransport("k", cs.URL)

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

			// Payload preserved verbatim as data in the named field. An
			// unparseable address is carried whole in the {email, name}
			// object's email field (mail.ParseAddress refuses CR/LF).
			switch tt.injectField {
			case "from":
				from, _ := body["from"].(map[string]any)
				if got := from["email"]; got != tt.payload {
					t.Errorf("from.email = %q, want %q (verbatim, unsanitized)", got, tt.payload)
				}
			case "reply_to":
				replyTo, _ := body["reply_to"].(map[string]any)
				if got := replyTo["email"]; got != tt.payload {
					t.Errorf("reply_to.email = %q, want %q (verbatim, unsanitized)", got, tt.payload)
				}
			case "subject":
				if got := body["subject"]; got != tt.payload {
					t.Errorf("subject = %q, want %q (verbatim, unsanitized)", got, tt.payload)
				}
			case "to":
				pers, _ := body["personalizations"].([]any)
				to, _ := pers[0].(map[string]any)["to"].([]any)
				if got := to[0].(map[string]any)["email"]; got != tt.payload {
					t.Errorf("to[0].email = %q, want %q (verbatim, unsanitized)", got, tt.payload)
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

// TestMailChannels_APIKeyNotInURLOrBody is the NFR3 / FR53 surface. The
// MailChannels key is sent via the X-Api-Key header, never in path or body.
func TestMailChannels_APIKeyNotInURLOrBody(t *testing.T) {
	cs := newCaptureServer(t, http.StatusAccepted, `{"results":[{"index":0,"message_id":"x"}]}`)
	const apiKey = "very-secret-mailchannels-key-do-not-leak"
	tp := NewMailChannelsTransport(apiKey, cs.URL)

	if _, err := tp.Send(context.Background(), goodMailChannelsMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := cs.headers.Get("X-Api-Key"); got != apiKey {
		t.Errorf("X-Api-Key = %q, want %q", got, apiKey)
	}
	if strings.Contains(cs.path, apiKey) {
		t.Errorf("API key appeared in URL path: %s", cs.path)
	}
	if strings.Contains(string(cs.body), apiKey) {
		t.Errorf("API key appeared in request body: %s", cs.body)
	}
}

// TestMailChannels_APIKeyNotInErrorOn403 is the NFR3 failure-path surface the
// maintainer asked for on #133: a fake key must not appear in the error the
// gateway logs when the provider rejects authentication.
func TestMailChannels_APIKeyNotInErrorOn403(t *testing.T) {
	cs := newCaptureServer(t, http.StatusForbidden, `{"errors":["The API key is invalid"]}`)
	const apiKey = "sentinel-mailchannels-key-do-not-leak"
	tp := NewMailChannelsTransport(apiKey, cs.URL)

	_, err := tp.Send(context.Background(), goodMailChannelsMessage())
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if strings.Contains(err.Error(), apiKey) {
		t.Errorf("API key leaked in error message: %v", err)
	}
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a *TransportError: %v", err)
	}
	if te.Class != ErrTerminal {
		t.Errorf("Class = %v, want ErrTerminal", te.Class)
	}
	if !strings.Contains(te.Message, "The API key is invalid") {
		t.Errorf("Message = %q, want it to surface the upstream message", te.Message)
	}
}

func TestNewMailChannelsTransport_DefaultBaseURL(t *testing.T) {
	tp := NewMailChannelsTransport("k", "")
	if tp.BaseURL != defaultMailChannelsBaseURL {
		t.Errorf("BaseURL = %q, want %q", tp.BaseURL, defaultMailChannelsBaseURL)
	}
}

func TestNewMailChannelsTransport_CustomBaseURL(t *testing.T) {
	tp := NewMailChannelsTransport("k", "https://custom.example.com")
	if tp.BaseURL != "https://custom.example.com" {
		t.Errorf("BaseURL = %q, want override", tp.BaseURL)
	}
}

func TestMailchannelsErrorMessage(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		fallback string
		want     string
	}{
		{"valid_with_errors", `{"errors":["Sender not verified"]}`, "fallback", "Sender not verified"},
		{"valid_multiple_errors", `{"errors":["bad from","bad to"]}`, "fallback", "bad from; bad to"},
		{"valid_empty_errors", `{"errors":[]}`, "fallback", "fallback"},
		{"invalid_json", `<html>503</html>`, "fallback", "fallback"},
		{"empty_body", ``, "fallback", "fallback"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mailchannelsErrorMessage([]byte(tt.body), tt.fallback)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMailChannels_UnexpectedStatus_Terminal(t *testing.T) {
	cs := newCaptureServer(t, http.StatusMultipleChoices, `{}`)
	tp := NewMailChannelsTransport("k", cs.URL)

	_, err := tp.Send(context.Background(), goodMailChannelsMessage())
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a *TransportError: %v", err)
	}
	if te.Class != ErrTerminal {
		t.Errorf("Class = %v, want ErrTerminal", te.Class)
	}
}

// --- Registry integration ---

func TestMailChannels_RegisteredAtPackageLoad(t *testing.T) {
	reg, ok := Lookup("mailchannels")
	if !ok {
		t.Fatal("mailchannels not registered after package init")
	}
	if err := reg.Validate(map[string]any{}); err == nil {
		t.Error("mailchannels Validate should reject empty settings (missing api_key)")
	}
	if err := reg.Validate(map[string]any{"api_key": "x"}); err != nil {
		t.Errorf("mailchannels Validate rejected valid settings: %v", err)
	}
	if !reg.CarriesHeaders {
		t.Error("mailchannels CarriesHeaders = false; List-Unsubscribe is accepted in its headers object")
	}
	tp, err := reg.Build(map[string]any{"api_key": "x"})
	if err != nil {
		t.Fatalf("mailchannels Build: %v", err)
	}
	if _, ok := tp.(*MailChannelsTransport); !ok {
		t.Errorf("mailchannels Build returned %T, want *MailChannelsTransport", tp)
	}
}

func TestMailChannels_BuildErrorOnEmptyAPIKey(t *testing.T) {
	reg, _ := Lookup("mailchannels")
	_, err := reg.Build(map[string]any{})
	if err == nil {
		t.Fatal("expected error from Build with empty api_key")
	}
}
