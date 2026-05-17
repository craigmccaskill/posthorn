package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func goodMailgunMessage() Message {
	return Message{
		From:     "noreply@example.com",
		To:       []string{"craig@example.com"},
		Subject:  "Hello",
		BodyText: "Body text.",
	}
}

// parseMultipart parses the captured multipart body back into a fields
// map for assertion. Returns map of field-name to slice-of-values to
// handle repeated fields (Mailgun's multi-recipient shape).
func parseMultipart(t *testing.T, contentType string, body []byte) map[string][]string {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("parse Content-Type: %v", err)
	}
	boundary := params["boundary"]
	if boundary == "" {
		t.Fatal("multipart Content-Type missing boundary")
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	out := map[string][]string{}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextPart: %v", err)
		}
		buf, err := io.ReadAll(part)
		if err != nil {
			t.Fatalf("read part: %v", err)
		}
		name := part.FormName()
		out[name] = append(out[name], string(buf))
		_ = part.Close()
	}
	return out
}

func TestMailgun_Success_200(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"id":"<mailgun-msg-abc-123@yourdomain.com>","message":"Queued. Thank you."}`)
	tp := NewMailgunTransport("test-key", "yourdomain.com", cs.URL)

	result, err := tp.Send(context.Background(), goodMailgunMessage())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if cs.hits != 1 {
		t.Errorf("hits = %d, want 1", cs.hits)
	}
	want := "<mailgun-msg-abc-123@yourdomain.com>"
	if result.MessageID != want {
		t.Errorf("MessageID = %q, want %q", result.MessageID, want)
	}
}

func TestMailgun_Success_NoMessageID(t *testing.T) {
	for _, tt := range []struct {
		name     string
		respBody string
	}{
		{"empty_object", `{}`},
		{"missing_id_field", `{"message":"Queued."}`},
		{"unparseable", `not-json`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cs := newCaptureServer(t, http.StatusOK, tt.respBody)
			tp := NewMailgunTransport("k", "d.com", cs.URL)
			result, err := tp.Send(context.Background(), goodMailgunMessage())
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if result.MessageID != "" {
				t.Errorf("MessageID = %q, want empty", result.MessageID)
			}
		})
	}
}

func TestMailgun_RequestShape(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"id":"x"}`)
	tp := NewMailgunTransport("mg-key-abc", "send.example.com", cs.URL)

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
	wantPath := "/v3/send.example.com/messages"
	if cs.path != wantPath {
		t.Errorf("path = %q, want %q", cs.path, wantPath)
	}
	ct := cs.headers.Get("Content-Type")
	if !strings.HasPrefix(ct, "multipart/form-data") {
		t.Errorf("Content-Type = %q, want multipart/form-data", ct)
	}
	// Basic auth: username "api", password = API key.
	user, pass, ok := basicAuthHeader(cs.headers.Get("Authorization"))
	if !ok {
		t.Fatalf("Authorization header not Basic: %q", cs.headers.Get("Authorization"))
	}
	if user != "api" {
		t.Errorf("Basic auth user = %q, want %q", user, "api")
	}
	if pass != "mg-key-abc" {
		t.Errorf("Basic auth password = %q, want %q (the API key)", pass, "mg-key-abc")
	}

	fields := parseMultipart(t, ct, cs.body)
	if got := fields["from"]; len(got) != 1 || got[0] != "a@example.com" {
		t.Errorf("from = %v", got)
	}
	if got := fields["to"]; len(got) != 2 || got[0] != "b@example.com" || got[1] != "c@example.com" {
		t.Errorf("to = %v, want [b@example.com c@example.com] as repeated fields", got)
	}
	if got := fields["h:Reply-To"]; len(got) != 1 || got[0] != "reply@example.com" {
		t.Errorf("h:Reply-To = %v", got)
	}
	if got := fields["subject"]; len(got) != 1 || got[0] != "Subj" {
		t.Errorf("subject = %v", got)
	}
	if got := fields["text"]; len(got) != 1 || got[0] != "Hello" {
		t.Errorf("text = %v", got)
	}
}

// basicAuthHeader decodes an Authorization: Basic header. Returns
// (user, pass, true) on success; (_, _, false) otherwise.
func basicAuthHeader(h string) (string, string, bool) {
	const prefix = "Basic "
	if !strings.HasPrefix(h, prefix) {
		return "", "", false
	}
	// httptest captures the literal header value; use http.Request to
	// decode for us. Build a one-off request just to call BasicAuth().
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", h)
	user, pass, ok := req.BasicAuth()
	return user, pass, ok
}

func TestMailgun_OmitsEmptyReplyTo(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"id":"x"}`)
	tp := NewMailgunTransport("k", "d.com", cs.URL)

	msg := goodMailgunMessage()
	msg.ReplyTo = ""
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	fields := parseMultipart(t, cs.headers.Get("Content-Type"), cs.body)
	if _, present := fields["h:Reply-To"]; present {
		t.Errorf("h:Reply-To present when ReplyTo empty: %v", fields)
	}
}

func TestMailgun_5xx_Transient(t *testing.T) {
	for _, status := range []int{500, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			cs := newCaptureServer(t, status, `{"message":"Server is having a bad day"}`)
			tp := NewMailgunTransport("k", "d.com", cs.URL)

			_, err := tp.Send(context.Background(), goodMailgunMessage())
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

func TestMailgun_429_RateLimited(t *testing.T) {
	cs := newCaptureServer(t, http.StatusTooManyRequests, `{"message":"Rate limit exceeded"}`)
	tp := NewMailgunTransport("k", "d.com", cs.URL)

	_, err := tp.Send(context.Background(), goodMailgunMessage())
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a *TransportError: %v", err)
	}
	if te.Class != ErrRateLimited {
		t.Errorf("Class = %v, want ErrRateLimited", te.Class)
	}
}

func TestMailgun_4xx_Terminal(t *testing.T) {
	for _, status := range []int{400, 401, 403, 422} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			cs := newCaptureServer(t, status, `{"message":"Bad request"}`)
			tp := NewMailgunTransport("k", "d.com", cs.URL)

			_, err := tp.Send(context.Background(), goodMailgunMessage())
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

func TestMailgun_NetworkError_Transient(t *testing.T) {
	cs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	url := cs.URL
	cs.Close()

	tp := NewMailgunTransport("k", "d.com", url)
	_, err := tp.Send(context.Background(), goodMailgunMessage())

	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a *TransportError: %v", err)
	}
	if te.Class != ErrTransient {
		t.Errorf("Class = %v, want ErrTransient", te.Class)
	}
}

func TestMailgun_ContextDeadline_Transient(t *testing.T) {
	cs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer cs.Close()

	tp := NewMailgunTransport("k", "d.com", cs.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := tp.Send(ctx, goodMailgunMessage())
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

// TestMailgun_NoHeaderInjection is the NFR1/NFR2 / FR52 acceptance test
// for the Mailgun transport. The multipart writer is the structural
// defense: CRLF in a field value cannot create a sibling field, and
// CRLF in the field name cannot smuggle headers (we control field names
// directly — no user input becomes a field name).
func TestMailgun_NoHeaderInjection(t *testing.T) {
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
			injectField: "h:Reply-To",
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

	// Allowed field names — anything else is a smuggled field.
	allowedFields := map[string]bool{
		"from": true, "to": true, "h:Reply-To": true, "subject": true, "text": true,
	}

	expectedHTTPHeaders := []string{"Content-Type", "Accept", "Authorization"}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := newCaptureServer(t, http.StatusOK, `{"id":"x"}`)
			tp := NewMailgunTransport("k", "d.com", cs.URL)

			if _, err := tp.Send(context.Background(), tt.message); err != nil {
				t.Fatalf("Send: %v", err)
			}

			fields := parseMultipart(t, cs.headers.Get("Content-Type"), cs.body)
			for k := range fields {
				if !allowedFields[k] {
					t.Errorf("smuggled multipart field %q in body", k)
				}
			}

			// Payload preserved verbatim in the named field.
			values, present := fields[tt.injectField]
			if !present {
				t.Fatalf("field %q missing from multipart body", tt.injectField)
			}
			found := false
			for _, v := range values {
				if v == tt.payload {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("field %q values = %v, want one to be %q (verbatim, unsanitized)",
					tt.injectField, values, tt.payload)
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

// TestMailgun_APIKeyNotInURLOrBody is the NFR3 / FR53 surface. The
// Mailgun key is sent via Basic auth password, never in URL or body.
func TestMailgun_APIKeyNotInURLOrBody(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"id":"x"}`)
	const apiKey = "very-secret-mailgun-token-do-not-leak"
	tp := NewMailgunTransport(apiKey, "d.com", cs.URL)

	if _, err := tp.Send(context.Background(), goodMailgunMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if strings.Contains(cs.path, apiKey) {
		t.Errorf("API key appeared in URL path: %s", cs.path)
	}
	if strings.Contains(string(cs.body), apiKey) {
		t.Errorf("API key appeared in request body: %s", cs.body)
	}
}

func TestNewMailgunTransport_DefaultBaseURL(t *testing.T) {
	tp := NewMailgunTransport("k", "d.com", "")
	if tp.BaseURL != defaultMailgunBaseURL {
		t.Errorf("BaseURL = %q, want %q", tp.BaseURL, defaultMailgunBaseURL)
	}
}

func TestNewMailgunTransport_CustomBaseURL(t *testing.T) {
	tp := NewMailgunTransport("k", "d.com", "https://api.eu.mailgun.net")
	if tp.BaseURL != "https://api.eu.mailgun.net" {
		t.Errorf("BaseURL = %q, want override (EU endpoint)", tp.BaseURL)
	}
}

func TestMailgunErrorMessage(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		fallback string
		want     string
	}{
		{"valid_with_message", `{"message":"Sender not verified"}`, "fallback", "Sender not verified"},
		{"valid_empty_message", `{"message":""}`, "fallback", "fallback"},
		{"invalid_json", `<html>503</html>`, "fallback", "fallback"},
		{"empty_body", ``, "fallback", "fallback"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mailgunErrorMessage([]byte(tt.body), tt.fallback)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMailgun_UnexpectedStatus_Terminal(t *testing.T) {
	cs := newCaptureServer(t, http.StatusMultipleChoices, `{}`)
	tp := NewMailgunTransport("k", "d.com", cs.URL)

	_, err := tp.Send(context.Background(), goodMailgunMessage())
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a *TransportError: %v", err)
	}
	if te.Class != ErrTerminal {
		t.Errorf("Class = %v, want ErrTerminal", te.Class)
	}
}

// --- Registry integration ---

func TestMailgun_RegisteredAtPackageLoad(t *testing.T) {
	reg, ok := Lookup("mailgun")
	if !ok {
		t.Fatal("mailgun not registered after package init")
	}
	if err := reg.Validate(map[string]any{}); err == nil {
		t.Error("mailgun Validate should reject empty settings")
	}
	if err := reg.Validate(map[string]any{"api_key": "x"}); err == nil {
		t.Error("mailgun Validate should reject settings missing domain")
	}
	if err := reg.Validate(map[string]any{"domain": "d.com"}); err == nil {
		t.Error("mailgun Validate should reject settings missing api_key")
	}
	if err := reg.Validate(map[string]any{"api_key": "x", "domain": "d.com"}); err != nil {
		t.Errorf("mailgun Validate rejected valid settings: %v", err)
	}
	tp, err := reg.Build(map[string]any{"api_key": "x", "domain": "d.com"})
	if err != nil {
		t.Fatalf("mailgun Build: %v", err)
	}
	if _, ok := tp.(*MailgunTransport); !ok {
		t.Errorf("mailgun Build returned %T, want *MailgunTransport", tp)
	}
}

func TestMailgun_BuildErrorOnEmptySettings(t *testing.T) {
	reg, _ := Lookup("mailgun")
	if _, err := reg.Build(map[string]any{}); err == nil {
		t.Error("expected error from Build with empty settings")
	}
	if _, err := reg.Build(map[string]any{"api_key": "x"}); err == nil {
		t.Error("expected error from Build with missing domain")
	}
	if _, err := reg.Build(map[string]any{"domain": "d.com"}); err == nil {
		t.Error("expected error from Build with missing api_key")
	}
}
