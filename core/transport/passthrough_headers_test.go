package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// Block F, Story 20.2: allowlisted passthrough headers cross every mail
// transport structurally (FR99) and the injection suite covers them
// (NFR32). The CRLF payload below can only exist if the ingress check
// were bypassed; the transports must still render it inert.

var passthroughHeaders = []Header{
	{Name: "List-Unsubscribe", Value: "<https://lists.example/u/abc>, <mailto:u@lists.example>"},
	{Name: "List-Unsubscribe-Post", Value: "List-Unsubscribe=One-Click"},
}

const crlfHeaderValue = "<mailto:u@lists.example>\r\nBcc: victim@target.com"

func passthroughMessage(headers []Header) Message {
	return Message{
		From: "f@example.com", To: []string{"r@example.com"},
		Subject: "s", BodyText: "b", Headers: headers,
	}
}

func TestPostmark_PassthroughHeaders(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{}`)
	tp := NewPostmarkTransport("k", cs.URL)
	if _, err := tp.Send(context.Background(), passthroughMessage(passthroughHeaders)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var body struct {
		Headers []struct{ Name, Value string }
	}
	if err := json.Unmarshal(cs.body, &body); err != nil {
		t.Fatalf("json: %v", err)
	}
	if len(body.Headers) != 2 || body.Headers[0].Name != "List-Unsubscribe" || body.Headers[1].Value != "List-Unsubscribe=One-Click" {
		t.Errorf("Headers = %+v", body.Headers)
	}

	// Injection: the value lands verbatim inside a JSON string and never
	// as a raw CRLF on the wire.
	cs = newCaptureServer(t, http.StatusOK, `{}`)
	tp = NewPostmarkTransport("k", cs.URL)
	if _, err := tp.Send(context.Background(), passthroughMessage([]Header{{Name: "List-Unsubscribe", Value: crlfHeaderValue}})); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if strings.Contains(string(cs.body), "\r\nBcc") {
		t.Errorf("raw CRLF reached the wire: %s", cs.body)
	}
	var raw map[string]any
	_ = json.Unmarshal(cs.body, &raw)
	if _, smuggled := raw["Bcc"]; smuggled {
		t.Error("CRLF synthesized a Bcc key")
	}
}

func TestResend_PassthroughHeaders(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"id":"x"}`)
	tp := NewResendTransport("k", cs.URL)
	repeated := append(append([]Header{}, passthroughHeaders...), Header{Name: "List-Unsubscribe", Value: "<https://lists.example/u/def>"})
	if _, err := tp.Send(context.Background(), passthroughMessage(repeated)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var body struct {
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(cs.body, &body); err != nil {
		t.Fatalf("json: %v", err)
	}
	if got := body.Headers["List-Unsubscribe"]; got != "<https://lists.example/u/abc>, <mailto:u@lists.example>, <https://lists.example/u/def>" {
		t.Errorf("repeated header should join with \", \": %q", got)
	}
	if body.Headers["List-Unsubscribe-Post"] != "List-Unsubscribe=One-Click" {
		t.Errorf("headers = %v", body.Headers)
	}

	cs = newCaptureServer(t, http.StatusOK, `{"id":"x"}`)
	tp = NewResendTransport("k", cs.URL)
	if _, err := tp.Send(context.Background(), passthroughMessage([]Header{{Name: "List-Id", Value: crlfHeaderValue}})); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if strings.Contains(string(cs.body), "\r\nBcc") {
		t.Errorf("raw CRLF reached the wire: %s", cs.body)
	}
}

func TestSES_PassthroughHeaders(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"MessageId":"m"}`)
	tp := NewSESTransport("AKIAEXAMPLE", "secret-test-key", "us-east-1", cs.URL)
	if _, err := tp.Send(context.Background(), passthroughMessage(passthroughHeaders)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var body struct {
		Content struct {
			Simple struct {
				Headers []struct{ Name, Value string }
			}
		}
	}
	if err := json.Unmarshal(cs.body, &body); err != nil {
		t.Fatalf("json: %v", err)
	}
	h := body.Content.Simple.Headers
	if len(h) != 2 || h[0].Name != "List-Unsubscribe" || h[1].Name != "List-Unsubscribe-Post" {
		t.Errorf("Content.Simple.Headers = %+v", h)
	}

	cs = newCaptureServer(t, http.StatusOK, `{"MessageId":"m"}`)
	tp = NewSESTransport("AKIAEXAMPLE", "secret-test-key", "us-east-1", cs.URL)
	if _, err := tp.Send(context.Background(), passthroughMessage([]Header{{Name: "List-Id", Value: crlfHeaderValue}})); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if strings.Contains(string(cs.body), "\r\nBcc") {
		t.Errorf("raw CRLF reached the wire: %s", cs.body)
	}
}

func TestMailgun_PassthroughHeaders(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"id":"x"}`)
	tp := NewMailgunTransport("k", "d.com", cs.URL)
	if _, err := tp.Send(context.Background(), passthroughMessage(passthroughHeaders)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fields := parseMultipart(t, cs.headers.Get("Content-Type"), cs.body)
	if v := fields["h:List-Unsubscribe"]; len(v) != 1 || v[0] != passthroughHeaders[0].Value {
		t.Errorf("h:List-Unsubscribe = %v", v)
	}
	if v := fields["h:List-Unsubscribe-Post"]; len(v) != 1 || v[0] != "List-Unsubscribe=One-Click" {
		t.Errorf("h:List-Unsubscribe-Post = %v", v)
	}
	for k := range fields {
		if k == "Bcc" || k == "h:Bcc" {
			t.Errorf("smuggled field %q", k)
		}
	}

	// Injection: the CRLF stays inside the form value; it cannot start a
	// new multipart field, and no field named Bcc appears.
	cs = newCaptureServer(t, http.StatusOK, `{"id":"x"}`)
	tp = NewMailgunTransport("k", "d.com", cs.URL)
	if _, err := tp.Send(context.Background(), passthroughMessage([]Header{{Name: "List-Id", Value: crlfHeaderValue}})); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fields = parseMultipart(t, cs.headers.Get("Content-Type"), cs.body)
	if v := fields["h:List-Id"]; len(v) != 1 || v[0] != crlfHeaderValue {
		t.Errorf("value should be preserved verbatim inside the field: %v", v)
	}
	if _, smuggled := fields["Bcc"]; smuggled {
		t.Error("CRLF synthesized a Bcc field")
	}
}

func TestSMTPOut_PassthroughHeaders(t *testing.T) {
	srv := startFakeSMTPServer(t)
	tp := newSMTPTestTransport(t, srv, false)
	msg := goodSMTPMessage()
	msg.Headers = passthroughHeaders
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// The fake server's DotReader normalizes line endings; compare on LF.
	data := strings.ReplaceAll(string(srv.Sessions[0].Data), "\r\n", "\n")
	sep := strings.Index(data, "\n\n")
	if sep < 0 {
		t.Fatalf("no header/body separator in DATA:\n%s", data)
	}
	headerPart := data[:sep+1]
	for _, want := range []string{
		"\nList-Unsubscribe: <https://lists.example/u/abc>, <mailto:u@lists.example>\n",
		"\nList-Unsubscribe-Post: List-Unsubscribe=One-Click\n",
	} {
		if !strings.Contains(headerPart, want) {
			t.Errorf("DATA headers missing %q:\n%s", want, headerPart)
		}
	}

	// Injection: SMTP-out builds real header lines, so it must refuse
	// before dialing (NFR1 at this layer, NFR32).
	for _, bad := range []Header{
		{Name: "List-Id", Value: crlfHeaderValue},
		{Name: "List-Id", Value: "a\nBcc: victim@target.com"},
		{Name: "List-Id: x\r\nBcc", Value: "y"},
	} {
		srv := startFakeSMTPServer(t)
		tp := newSMTPTestTransport(t, srv, false)
		msg := goodSMTPMessage()
		msg.Headers = []Header{bad}
		_, err := tp.Send(context.Background(), msg)
		var te *TransportError
		if !errors.As(err, &te) || te.Class != ErrTerminal {
			t.Fatalf("header %+v: want terminal error, got %v", bad, err)
		}
		if len(srv.Sessions) > 0 && len(srv.Sessions[0].Data) > 0 {
			t.Errorf("header %+v: DATA was written despite injection attempt", bad)
		}
	}
}

func TestWebhook_IgnoresPassthroughHeaders(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{}`)
	tp := NewWebhookTransport(cs.URL, webhookTestSecret, nil)
	if _, err := tp.Send(context.Background(), passthroughMessage(passthroughHeaders)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if strings.Contains(string(cs.body), "List-Unsubscribe") {
		t.Errorf("webhook payload must not carry mail headers (ADR-24 precedent): %s", cs.body)
	}
}
