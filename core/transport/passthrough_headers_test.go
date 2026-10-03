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
// (NFR32). The bad headers below can only reach a transport if the
// ingress check were bypassed (another ingress, a replayed queue row);
// each transport must refuse them itself, before anything is sent.

var passthroughHeaders = []Header{
	{Name: "List-Unsubscribe", Value: "<https://lists.example/u/abc>, <mailto:u@lists.example>"},
	{Name: "List-Unsubscribe-Post", Value: "List-Unsubscribe=One-Click"},
}

const crlfHeaderValue = "<mailto:u@lists.example>\r\nBcc: victim@target.com"

// badHeaderSets are header lists no transport may send.
var badHeaderSets = map[string][]Header{
	"CRLF in value":        {{Name: "List-Unsubscribe", Value: crlfHeaderValue}},
	"bare LF in value":     {{Name: "List-Id", Value: "a\nBcc: victim@target.com"}},
	"bare CR in value":     {{Name: "List-Id", Value: "a\rBcc: victim@target.com"}},
	"NUL in value":         {{Name: "List-Id", Value: "a\x00b"}},
	"8-bit byte in value":  {{Name: "List-Id", Value: "caf\xe9 <list.example>"}},
	"empty value":          {{Name: "List-Unsubscribe", Value: ""}},
	"value over one line":  {{Name: "List-Unsubscribe", Value: "<https://lists.example/" + strings.Repeat("a", 1000) + ">"}},
	"recipient header":     {{Name: "Bcc", Value: "victim@target.com"}},
	"name off allowlist":   {{Name: "X-Team", Value: "infra"}},
	"name not canonical":   {{Name: "list-unsubscribe", Value: "<mailto:u@lists.example>"}},
	"injection in name":    {{Name: "List-Id: x\r\nBcc", Value: "y"}},
	"same name twice":      {{Name: "List-Id", Value: "one <a.example>"}, {Name: "List-Id", Value: "two <b.example>"}},
	"good one then a Bcc":  {{Name: "List-Id", Value: "one <a.example>"}, {Name: "Bcc", Value: "victim@target.com"}},
	"DEL in value":         {{Name: "List-Id", Value: "a\x7fb"}},
	"tab in value":         {{Name: "List-Id", Value: "a\tb"}},
	"vertical tab in name": {{Name: "List-Id\v", Value: "a"}},
}

func passthroughMessage(headers []Header) Message {
	return Message{
		From: "f@example.com", To: []string{"r@example.com"},
		Subject: "s", BodyText: "b", Headers: headers,
	}
}

// assertTerminal checks a Send refused with a terminal TransportError.
func assertTerminal(t *testing.T, err error) {
	t.Helper()
	var te *TransportError
	if !errors.As(err, &te) || te.Class != ErrTerminal {
		t.Fatalf("want a terminal TransportError, got %v", err)
	}
}

func TestValidateHeaders(t *testing.T) {
	if err := ValidateHeaders(nil); err != nil {
		t.Errorf("nil headers: %v", err)
	}
	if err := ValidateHeaders(passthroughHeaders); err != nil {
		t.Errorf("good headers: %v", err)
	}
	all := []Header{
		{Name: "List-Unsubscribe", Value: "<mailto:u@lists.example>"},
		{Name: "List-Unsubscribe-Post", Value: "List-Unsubscribe=One-Click"},
		{Name: "List-Id", Value: "Weekly <weekly.lists.example>"},
	}
	if err := ValidateHeaders(all); err != nil {
		t.Errorf("one of each allowlisted name: %v", err)
	}
	for name, bad := range badHeaderSets {
		if err := ValidateHeaders(bad); err == nil {
			t.Errorf("%s: accepted %+v", name, bad)
		}
	}

	// The length rule is the RFC 5322 line limit on "Name: value".
	const name = "List-Id"
	fits := strings.Repeat("a", maxHeaderLine-len(name)-len(": "))
	if err := CheckHeaderValue(name, fits); err != nil {
		t.Errorf("a %d-character line should fit: %v", maxHeaderLine, err)
	}
	if err := CheckHeaderValue(name, fits+"a"); err == nil {
		t.Errorf("a %d-character line should be refused", maxHeaderLine+1)
	}
}

// TestRegistry_CarriesHeaders pins which transports config validation
// lets a listener pair with passthrough_headers (FR99). A transport that
// isn't listed here must not set the flag without also emitting headers.
func TestRegistry_CarriesHeaders(t *testing.T) {
	want := map[string]bool{
		"postmark": true, "resend": true, "mailgun": true, "ses": true, "smtp": true,
		"webhook": false,
	}
	for _, typ := range KnownTypes() {
		reg, _ := Lookup(typ)
		carries, known := want[typ]
		if !known {
			t.Errorf("transport %q is not covered by this test: decide whether it carries headers", typ)
			continue
		}
		if reg.CarriesHeaders != carries {
			t.Errorf("transport %q CarriesHeaders = %v, want %v", typ, reg.CarriesHeaders, carries)
		}
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

	// No Headers key at all when there is nothing to carry.
	cs = newCaptureServer(t, http.StatusOK, `{}`)
	tp = NewPostmarkTransport("k", cs.URL)
	if _, err := tp.Send(context.Background(), passthroughMessage(nil)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if strings.Contains(string(cs.body), `"Headers"`) {
		t.Errorf("request carries a Headers key with no passthrough headers: %s", cs.body)
	}

	for name, bad := range badHeaderSets {
		t.Run(name, func(t *testing.T) {
			cs := newCaptureServer(t, http.StatusOK, `{}`)
			_, err := NewPostmarkTransport("k", cs.URL).Send(context.Background(), passthroughMessage(bad))
			assertTerminal(t, err)
			if cs.hits != 0 {
				t.Errorf("request reached the provider: %s", cs.body)
			}
		})
	}
}

func TestResend_PassthroughHeaders(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"id":"x"}`)
	tp := NewResendTransport("k", cs.URL)
	if _, err := tp.Send(context.Background(), passthroughMessage(passthroughHeaders)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var body struct {
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(cs.body, &body); err != nil {
		t.Fatalf("json: %v", err)
	}
	if len(body.Headers) != 2 ||
		body.Headers["List-Unsubscribe"] != passthroughHeaders[0].Value ||
		body.Headers["List-Unsubscribe-Post"] != "List-Unsubscribe=One-Click" {
		t.Errorf("headers = %v", body.Headers)
	}

	// badHeaderSets includes a repeated name: Resend's object form has
	// one slot per name, and joining two List-Unsubscribe-Post values
	// would break one-click unsubscribe, so a repeat is refused, not
	// merged.
	for name, bad := range badHeaderSets {
		t.Run(name, func(t *testing.T) {
			cs := newCaptureServer(t, http.StatusOK, `{"id":"x"}`)
			_, err := NewResendTransport("k", cs.URL).Send(context.Background(), passthroughMessage(bad))
			assertTerminal(t, err)
			if cs.hits != 0 {
				t.Errorf("request reached the provider: %s", cs.body)
			}
		})
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
	if len(h) != 2 || h[0].Name != "List-Unsubscribe" || h[0].Value != passthroughHeaders[0].Value || h[1].Name != "List-Unsubscribe-Post" {
		t.Errorf("Content.Simple.Headers = %+v", h)
	}

	for name, bad := range badHeaderSets {
		t.Run(name, func(t *testing.T) {
			cs := newCaptureServer(t, http.StatusOK, `{"MessageId":"m"}`)
			_, err := NewSESTransport("AKIAEXAMPLE", "secret-test-key", "us-east-1", cs.URL).Send(context.Background(), passthroughMessage(bad))
			assertTerminal(t, err)
			if cs.hits != 0 {
				t.Errorf("request reached the provider: %s", cs.body)
			}
		})
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

	// Mailgun builds the header line from the h: field on its side, so a
	// CRLF value sitting safely inside the multipart form is not proof
	// of anything. The value must not leave Posthorn at all.
	for name, bad := range badHeaderSets {
		t.Run(name, func(t *testing.T) {
			cs := newCaptureServer(t, http.StatusOK, `{"id":"x"}`)
			_, err := NewMailgunTransport("k", "d.com", cs.URL).Send(context.Background(), passthroughMessage(bad))
			assertTerminal(t, err)
			if cs.hits != 0 {
				t.Errorf("request reached the provider: %s", cs.body)
			}
		})
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

	// SMTP-out writes real header lines, so it must refuse before
	// dialing (NFR1 at this layer, NFR32).
	for name, bad := range badHeaderSets {
		t.Run(name, func(t *testing.T) {
			srv := startFakeSMTPServer(t)
			tp := newSMTPTestTransport(t, srv, false)
			msg := goodSMTPMessage()
			msg.Headers = bad
			_, err := tp.Send(context.Background(), msg)
			assertTerminal(t, err)
			if len(srv.Sessions) > 0 {
				t.Errorf("the relay was dialed despite a refused header")
			}
		})
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
