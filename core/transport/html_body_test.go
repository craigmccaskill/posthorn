package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/mail"
	"strings"
	"testing"
)

// FR74: every transport carries BodyHTML structurally, and omits it
// entirely when the message is text-only so the v1.x wire format is
// byte-compatible for existing deployments.

func htmlMessage() Message {
	return Message{
		From:     "noreply@example.com",
		To:       []string{"craig@example.com"},
		Subject:  "Hello",
		BodyText: "Fallback text.",
		BodyHTML: "<p>Hello <b>there</b></p>",
	}
}

func TestPostmark_HTMLBody(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{}`)
	tp := NewPostmarkTransport("k", cs.URL)

	if _, err := tp.Send(context.Background(), htmlMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(cs.body, &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if got := body["HtmlBody"]; got != "<p>Hello <b>there</b></p>" {
		t.Errorf("HtmlBody = %v", got)
	}
	if got := body["TextBody"]; got != "Fallback text." {
		t.Errorf("TextBody = %v (text part must accompany HTML)", got)
	}
}

func TestPostmark_TextOnly_OmitsHtmlBody(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{}`)
	tp := NewPostmarkTransport("k", cs.URL)

	msg := htmlMessage()
	msg.BodyHTML = ""
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var body map[string]any
	_ = json.Unmarshal(cs.body, &body)
	if _, present := body["HtmlBody"]; present {
		t.Errorf("HtmlBody present on text-only message: %s", cs.body)
	}
}

func TestResend_HTMLBody(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"id":"x"}`)
	tp := NewResendTransport("k", cs.URL)

	if _, err := tp.Send(context.Background(), htmlMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(cs.body, &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if got := body["html"]; got != "<p>Hello <b>there</b></p>" {
		t.Errorf("html = %v", got)
	}
	if got := body["text"]; got != "Fallback text." {
		t.Errorf("text = %v", got)
	}
}

func TestResend_TextOnly_OmitsHTML(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"id":"x"}`)
	tp := NewResendTransport("k", cs.URL)

	msg := htmlMessage()
	msg.BodyHTML = ""
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var body map[string]any
	_ = json.Unmarshal(cs.body, &body)
	if _, present := body["html"]; present {
		t.Errorf("html present on text-only message: %s", cs.body)
	}
}

// parseMailgunForm decodes the captured multipart form body.
func parseMailgunForm(t *testing.T, contentType string, raw []byte) map[string][]string {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("parse content type: %v", err)
	}
	mr := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
	form := map[string][]string{}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read part: %v", err)
		}
		val, _ := io.ReadAll(part)
		form[part.FormName()] = append(form[part.FormName()], string(val))
	}
	return form
}

func TestMailgun_HTMLBody(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"id":"x"}`)
	tp := NewMailgunTransport("k", "mg.example.com", cs.URL)

	if _, err := tp.Send(context.Background(), htmlMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	form := parseMailgunForm(t, cs.headers.Get("Content-Type"), cs.body)
	if got := form["html"]; len(got) != 1 || got[0] != "<p>Hello <b>there</b></p>" {
		t.Errorf("html field = %v", got)
	}
	if got := form["text"]; len(got) != 1 || got[0] != "Fallback text." {
		t.Errorf("text field = %v", got)
	}
}

func TestMailgun_TextOnly_OmitsHTMLField(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"id":"x"}`)
	tp := NewMailgunTransport("k", "mg.example.com", cs.URL)

	msg := htmlMessage()
	msg.BodyHTML = ""
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	form := parseMailgunForm(t, cs.headers.Get("Content-Type"), cs.body)
	if _, present := form["html"]; present {
		t.Errorf("html field present on text-only message: %v", form)
	}
}

func TestSES_HTMLBody(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"MessageId":"x"}`)
	tp := NewSESTransport("AKID", "secret", "us-east-1", cs.URL)

	if _, err := tp.Send(context.Background(), htmlMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var body struct {
		Content struct {
			Simple struct {
				Body struct {
					Text *struct{ Data string }
					Html *struct{ Data string }
				}
			}
		}
	}
	if err := json.Unmarshal(cs.body, &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body.Content.Simple.Body.Html == nil || body.Content.Simple.Body.Html.Data != "<p>Hello <b>there</b></p>" {
		t.Errorf("Body.Html = %+v", body.Content.Simple.Body.Html)
	}
	if body.Content.Simple.Body.Text == nil || body.Content.Simple.Body.Text.Data != "Fallback text." {
		t.Errorf("Body.Text = %+v", body.Content.Simple.Body.Text)
	}
}

func TestSES_TextOnly_OmitsHtml(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"MessageId":"x"}`)
	tp := NewSESTransport("AKID", "secret", "us-east-1", cs.URL)

	msg := htmlMessage()
	msg.BodyHTML = ""
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var body map[string]any
	_ = json.Unmarshal(cs.body, &body)
	simple := body["Content"].(map[string]any)["Simple"].(map[string]any)
	if _, present := simple["Body"].(map[string]any)["Html"]; present {
		t.Errorf("Body.Html present on text-only message: %s", cs.body)
	}
}

// parseDATAMultipart parses a captured RFC 5322 DATA blob and returns
// the multipart/alternative parts in order as (contentType, body) pairs.
func parseDATAMultipart(t *testing.T, data []byte) []struct{ ContentType, Body string } {
	t.Helper()
	m, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parse DATA as RFC 5322: %v", err)
	}
	mediaType, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse Content-Type: %v", err)
	}
	if mediaType != "multipart/alternative" {
		t.Fatalf("Content-Type = %q, want multipart/alternative", mediaType)
	}
	mr := multipart.NewReader(m.Body, params["boundary"])
	var parts []struct{ ContentType, Body string }
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read part: %v", err)
		}
		raw, _ := io.ReadAll(p)
		parts = append(parts, struct{ ContentType, Body string }{
			ContentType: p.Header.Get("Content-Type"),
			Body:        string(raw),
		})
	}
	return parts
}

func TestMailChannels_HTMLBody(t *testing.T) {
	cs := newCaptureServer(t, http.StatusAccepted, `{"results":[{"index":0,"message_id":"x"}]}`)
	tp := NewMailChannelsTransport("k", cs.URL)

	if _, err := tp.Send(context.Background(), htmlMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var body struct {
		Content []struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"content"`
	}
	if err := json.Unmarshal(cs.body, &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if len(body.Content) != 2 {
		t.Fatalf("content = %+v, want text/plain then text/html", body.Content)
	}
	if body.Content[0].Type != "text/plain" || body.Content[0].Value != "Fallback text." {
		t.Errorf("content[0] = %+v, want the text part first", body.Content[0])
	}
	if body.Content[1].Type != "text/html" || body.Content[1].Value != "<p>Hello <b>there</b></p>" {
		t.Errorf("content[1] = %+v, want the HTML part", body.Content[1])
	}
}

func TestMailChannels_TextOnly_OmitsHTMLPart(t *testing.T) {
	cs := newCaptureServer(t, http.StatusAccepted, `{"results":[{"index":0,"message_id":"x"}]}`)
	tp := NewMailChannelsTransport("k", cs.URL)

	msg := htmlMessage()
	msg.BodyHTML = ""
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var body struct {
		Content []struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"content"`
	}
	_ = json.Unmarshal(cs.body, &body)
	if len(body.Content) != 1 || body.Content[0].Type != "text/plain" {
		t.Errorf("content = %+v, want a single text/plain part", body.Content)
	}
}

func TestSMTPOut_HTMLBody_MultipartAlternative(t *testing.T) {
	srv := startFakeSMTPServer(t)
	tp := newSMTPTestTransport(t, srv, TLSNone)

	msg := goodSMTPMessage()
	msg.BodyHTML = "<p>Hello <b>there</b></p>"
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	parts := parseDATAMultipart(t, srv.Sessions[0].Data)
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(parts))
	}
	// RFC 2046 §5.1.4: increasing preference — text first, HTML last.
	if !strings.HasPrefix(parts[0].ContentType, "text/plain") {
		t.Errorf("first part Content-Type = %q, want text/plain", parts[0].ContentType)
	}
	if !strings.Contains(parts[0].Body, "Body text.") {
		t.Errorf("text part missing body: %q", parts[0].Body)
	}
	if !strings.HasPrefix(parts[1].ContentType, "text/html") {
		t.Errorf("second part Content-Type = %q, want text/html", parts[1].ContentType)
	}
	if !strings.Contains(parts[1].Body, "<p>Hello <b>there</b></p>") {
		t.Errorf("html part missing body: %q", parts[1].Body)
	}
}

func TestSMTPOut_TextOnly_WireFormatUnchanged(t *testing.T) {
	srv := startFakeSMTPServer(t)
	tp := newSMTPTestTransport(t, srv, TLSNone)

	if _, err := tp.Send(context.Background(), goodSMTPMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	data := srv.Sessions[0].Data
	if !bytes.Contains(data, []byte(`Content-Type: text/plain; charset="utf-8"`)) {
		t.Errorf("text-only DATA lost its v1.x single-part Content-Type:\n%s", data)
	}
	if bytes.Contains(data, []byte("multipart")) {
		t.Errorf("text-only DATA became multipart:\n%s", data)
	}
}

// NFR1/FR74: CRLF sequences inside an HTML body must stay inside the
// MIME part — they must never terminate the header block early or
// smuggle a header into the message.
func TestSMTPOut_HTMLBody_NoHeaderSmuggling(t *testing.T) {
	srv := startFakeSMTPServer(t)
	tp := newSMTPTestTransport(t, srv, TLSNone)

	msg := goodSMTPMessage()
	msg.BodyHTML = "<p>x</p>\r\nBcc: evil@example.com\r\n<p>y</p>"
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	m, err := mail.ReadMessage(bytes.NewReader(srv.Sessions[0].Data))
	if err != nil {
		t.Fatalf("parse DATA: %v", err)
	}
	if got := m.Header.Get("Bcc"); got != "" {
		t.Errorf("Bcc header smuggled via HTML body: %q", got)
	}
	// The attempted header must still be present as inert body content.
	parts := parseDATAMultipart(t, srv.Sessions[0].Data)
	if !strings.Contains(parts[1].Body, "Bcc: evil@example.com") {
		t.Errorf("body content altered; injection text should survive as inert text: %q", parts[1].Body)
	}
}

// The multipart boundary is generated randomly by mime/multipart — a
// body that includes a boundary-shaped string cannot terminate its own
// part because it cannot predict the real boundary.
func TestSMTPOut_HTMLBody_BoundaryNotDerivedFromContent(t *testing.T) {
	srv := startFakeSMTPServer(t)
	tp := newSMTPTestTransport(t, srv, TLSNone)

	msg := goodSMTPMessage()
	msg.BodyHTML = "<p>fake boundary: --boundary123--</p>"
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	parts := parseDATAMultipart(t, srv.Sessions[0].Data)
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2 (content must not split parts)", len(parts))
	}
	if !strings.Contains(parts[1].Body, "--boundary123--") {
		t.Errorf("boundary-shaped content lost: %q", parts[1].Body)
	}
}
