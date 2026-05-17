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

func goodSESMessage() Message {
	return Message{
		From:     "noreply@example.com",
		To:       []string{"craig@example.com"},
		Subject:  "Hello",
		BodyText: "Body text.",
	}
}

// newSESTestTransport wires a transport at a captureServer URL and
// pins time.Now so the SigV4 signature is deterministic across runs.
func newSESTestTransport(serverURL string) *SESTransport {
	tp := NewSESTransport("AKIATESTEXAMPLE", "secret-test-key", "us-east-1", serverURL)
	tp.now = func() time.Time { return time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC) }
	return tp
}

func TestSES_Success_200(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"MessageId":"ses-msg-abc-123"}`)
	tp := newSESTestTransport(cs.URL)

	result, err := tp.Send(context.Background(), goodSESMessage())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if cs.hits != 1 {
		t.Errorf("hits = %d, want 1", cs.hits)
	}
	if result.MessageID != "ses-msg-abc-123" {
		t.Errorf("MessageID = %q, want %q", result.MessageID, "ses-msg-abc-123")
	}
}

func TestSES_Success_NoMessageID(t *testing.T) {
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
			tp := newSESTestTransport(cs.URL)
			result, err := tp.Send(context.Background(), goodSESMessage())
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if result.MessageID != "" {
				t.Errorf("MessageID = %q, want empty", result.MessageID)
			}
		})
	}
}

func TestSES_RequestShape(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"MessageId":"x"}`)
	tp := newSESTestTransport(cs.URL)

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
	if cs.path != sesSendPath {
		t.Errorf("path = %q, want %q", cs.path, sesSendPath)
	}
	if got := cs.headers.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	// SigV4 headers must be present.
	if cs.headers.Get("Authorization") == "" {
		t.Error("Authorization header missing")
	}
	if !strings.HasPrefix(cs.headers.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		t.Errorf("Authorization prefix wrong: %s", cs.headers.Get("Authorization"))
	}
	if cs.headers.Get("X-Amz-Date") == "" {
		t.Error("X-Amz-Date header missing")
	}
	if cs.headers.Get("X-Amz-Content-Sha256") == "" {
		t.Error("X-Amz-Content-Sha256 header missing")
	}

	// SESv2 SendEmail body shape (Pascal-cased per AWS).
	var body sesRequest
	if err := json.Unmarshal(cs.body, &body); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, cs.body)
	}
	if body.FromEmailAddress != "a@example.com" {
		t.Errorf("FromEmailAddress = %q", body.FromEmailAddress)
	}
	if len(body.Destination.ToAddresses) != 2 ||
		body.Destination.ToAddresses[0] != "b@example.com" ||
		body.Destination.ToAddresses[1] != "c@example.com" {
		t.Errorf("ToAddresses = %v", body.Destination.ToAddresses)
	}
	if len(body.ReplyToAddresses) != 1 || body.ReplyToAddresses[0] != "reply@example.com" {
		t.Errorf("ReplyToAddresses = %v", body.ReplyToAddresses)
	}
	if body.Content.Simple.Subject.Data != "Subj" {
		t.Errorf("Subject.Data = %q", body.Content.Simple.Subject.Data)
	}
	if body.Content.Simple.Body.Text.Data != "Hello" {
		t.Errorf("Body.Text.Data = %q", body.Content.Simple.Body.Text.Data)
	}
}

func TestSES_OmitsEmptyReplyTo(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"MessageId":"x"}`)
	tp := newSESTestTransport(cs.URL)

	msg := goodSESMessage()
	msg.ReplyTo = ""
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var body map[string]any
	_ = json.Unmarshal(cs.body, &body)
	if v, present := body["ReplyToAddresses"]; present && v != nil {
		t.Errorf("ReplyToAddresses present when message field empty: %v", v)
	}
}

func TestSES_5xx_Transient(t *testing.T) {
	for _, status := range []int{500, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			cs := newCaptureServer(t, status, `{"__type":"InternalFailure","message":"Server is having a bad day"}`)
			tp := newSESTestTransport(cs.URL)

			_, err := tp.Send(context.Background(), goodSESMessage())
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

func TestSES_429_RateLimited(t *testing.T) {
	cs := newCaptureServer(t, http.StatusTooManyRequests, `{"__type":"ThrottlingException","message":"Rate exceeded"}`)
	tp := newSESTestTransport(cs.URL)

	_, err := tp.Send(context.Background(), goodSESMessage())
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a *TransportError: %v", err)
	}
	if te.Class != ErrRateLimited {
		t.Errorf("Class = %v, want ErrRateLimited", te.Class)
	}
}

func TestSES_4xx_Terminal(t *testing.T) {
	for _, status := range []int{400, 401, 403, 422} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			cs := newCaptureServer(t, status, `{"__type":"BadRequestException","message":"Bad request"}`)
			tp := newSESTestTransport(cs.URL)

			_, err := tp.Send(context.Background(), goodSESMessage())
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

// TestSES_ErrorMessageCapitalM covers the SES convention where some
// endpoints return "Message" (capital M) and others "message" (lowercase).
func TestSES_ErrorMessageCapitalM(t *testing.T) {
	cs := newCaptureServer(t, http.StatusBadRequest, `{"Message":"Sender not verified"}`)
	tp := newSESTestTransport(cs.URL)
	_, err := tp.Send(context.Background(), goodSESMessage())
	var te *TransportError
	errors.As(err, &te)
	if !strings.Contains(te.Message, "Sender not verified") {
		t.Errorf("upstream message not surfaced; got %q", te.Message)
	}
}

func TestSES_NetworkError_Transient(t *testing.T) {
	cs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	url := cs.URL
	cs.Close()

	tp := newSESTestTransport(url)
	_, err := tp.Send(context.Background(), goodSESMessage())

	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a *TransportError: %v", err)
	}
	if te.Class != ErrTransient {
		t.Errorf("Class = %v, want ErrTransient", te.Class)
	}
}

func TestSES_ContextDeadline_Transient(t *testing.T) {
	cs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer cs.Close()

	tp := newSESTestTransport(cs.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := tp.Send(ctx, goodSESMessage())
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

// TestSES_NoHeaderInjection is the NFR1/NFR2 / FR52 acceptance test for
// the SES transport. JSON struct fields are the structural defense.
func TestSES_NoHeaderInjection(t *testing.T) {
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
			injectField: "FromEmailAddress",
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
			injectField: "Subject.Data",
			payload:     "Hello\r\nBcc: target@victim.com",
		},
		{
			name: "crlf_in_to_recipient",
			message: Message{
				From:     "f@example.com",
				To:       []string{"r@example.com\r\nBcc: target@victim.com"},
				Subject:  "s",
				BodyText: "b",
			},
			injectField: "ToAddresses[0]",
			payload:     "r@example.com\r\nBcc: target@victim.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := newCaptureServer(t, http.StatusOK, `{"MessageId":"x"}`)
			tp := newSESTestTransport(cs.URL)

			if _, err := tp.Send(context.Background(), tt.message); err != nil {
				t.Fatalf("Send: %v", err)
			}

			var body sesRequest
			if err := json.Unmarshal(cs.body, &body); err != nil {
				t.Fatalf("body not valid JSON: %v\n%s", err, cs.body)
			}

			var got string
			switch tt.injectField {
			case "FromEmailAddress":
				got = body.FromEmailAddress
			case "Subject.Data":
				got = body.Content.Simple.Subject.Data
			case "ToAddresses[0]":
				if len(body.Destination.ToAddresses) > 0 {
					got = body.Destination.ToAddresses[0]
				}
			}
			if got != tt.payload {
				t.Errorf("%s = %q, want %q (verbatim, unsanitized)", tt.injectField, got, tt.payload)
			}
		})
	}
}

// TestSES_SecretNotInURLBodyOrLogs is the NFR3 / FR53 surface. The SES
// secret access key must never appear in URL, body, or any captured
// header. Only the access key ID (operator-facing) and the SigV4
// signature (computed) reach the wire.
func TestSES_SecretNotInURLBodyOrLogs(t *testing.T) {
	const secret = "sentinel-ses-secret-key-do-not-leak"
	cs := newCaptureServer(t, http.StatusOK, `{"MessageId":"x"}`)
	tp := NewSESTransport("AKIAEXAMPLE", secret, "us-east-1", cs.URL)
	tp.now = func() time.Time { return time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC) }

	if _, err := tp.Send(context.Background(), goodSESMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if strings.Contains(cs.path, secret) {
		t.Errorf("secret appeared in URL path: %s", cs.path)
	}
	if strings.Contains(string(cs.body), secret) {
		t.Errorf("secret appeared in request body: %s", cs.body)
	}
	for name, values := range cs.headers {
		for _, v := range values {
			if strings.Contains(v, secret) {
				t.Errorf("secret appeared in header %q: %s", name, v)
			}
		}
	}
}

func TestNewSESTransport_DefaultBaseURL(t *testing.T) {
	tp := NewSESTransport("k", "s", "us-east-1", "")
	want := "https://email.us-east-1.amazonaws.com"
	if tp.BaseURL != want {
		t.Errorf("BaseURL = %q, want %q", tp.BaseURL, want)
	}
}

func TestNewSESTransport_RegionInDefaultURL(t *testing.T) {
	for _, region := range []string{"us-west-2", "eu-west-1", "ap-southeast-1"} {
		tp := NewSESTransport("k", "s", region, "")
		want := "https://email." + region + ".amazonaws.com"
		if tp.BaseURL != want {
			t.Errorf("region %q: BaseURL = %q, want %q", region, tp.BaseURL, want)
		}
	}
}

func TestSES_UnexpectedStatus_Terminal(t *testing.T) {
	cs := newCaptureServer(t, http.StatusMultipleChoices, `{}`)
	tp := newSESTestTransport(cs.URL)

	_, err := tp.Send(context.Background(), goodSESMessage())
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a *TransportError: %v", err)
	}
	if te.Class != ErrTerminal {
		t.Errorf("Class = %v, want ErrTerminal", te.Class)
	}
}

// --- Registry integration ---

func TestSES_RegisteredAtPackageLoad(t *testing.T) {
	reg, ok := Lookup("ses")
	if !ok {
		t.Fatal("ses not registered after package init")
	}
	tests := []struct {
		name     string
		settings map[string]any
		wantErr  bool
	}{
		{"empty", map[string]any{}, true},
		{"missing_region", map[string]any{"access_key_id": "x", "secret_access_key": "y"}, true},
		{"missing_secret", map[string]any{"access_key_id": "x", "region": "us-east-1"}, true},
		{"missing_akid", map[string]any{"secret_access_key": "y", "region": "us-east-1"}, true},
		{"valid", map[string]any{"access_key_id": "x", "secret_access_key": "y", "region": "us-east-1"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := reg.Validate(tt.settings)
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate(%v) error = %v, wantErr %v", tt.settings, err, tt.wantErr)
			}
		})
	}

	tp, err := reg.Build(map[string]any{
		"access_key_id":     "AKIAEXAMPLE",
		"secret_access_key": "secret",
		"region":            "us-east-1",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := tp.(*SESTransport); !ok {
		t.Errorf("Build returned %T, want *SESTransport", tp)
	}
}
