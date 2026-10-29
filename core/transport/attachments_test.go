package transport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/mail"
	"strings"
	"testing"
)

// FR92: every transport maps attachments structurally.

func attachedMessage() Message {
	return Message{
		From: "a@example.com", To: []string{"b@example.com"},
		Subject: "S", BodyText: "body",
		Attachments: []Attachment{
			{Filename: "license.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.4 fake")},
			{Filename: "photo.png", ContentType: "image/png", Data: []byte("\x89PNG fake")},
		},
	}
}

func TestPostmark_Attachments(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{}`)
	tp := NewPostmarkTransport("k", cs.URL)
	if _, err := tp.Send(context.Background(), attachedMessage()); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Attachments []struct{ Name, Content, ContentType string }
	}
	if err := json.Unmarshal(cs.body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Attachments) != 2 {
		t.Fatalf("attachments = %d", len(body.Attachments))
	}
	raw, err := base64.StdEncoding.DecodeString(body.Attachments[0].Content)
	if err != nil || string(raw) != "%PDF-1.4 fake" {
		t.Errorf("content round-trip: %q err=%v", raw, err)
	}
	if body.Attachments[0].Name != "license.pdf" || body.Attachments[0].ContentType != "application/pdf" {
		t.Errorf("attachment meta = %+v", body.Attachments[0])
	}
}

func TestResend_Attachments(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"id":"x"}`)
	tp := NewResendTransport("k", cs.URL)
	if _, err := tp.Send(context.Background(), attachedMessage()); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Attachments []struct {
			Filename    string `json:"filename"`
			Content     string `json:"content"`
			ContentType string `json:"content_type"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(cs.body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Attachments) != 2 || body.Attachments[1].Filename != "photo.png" || body.Attachments[1].ContentType != "image/png" {
		t.Fatalf("attachments = %+v", body.Attachments)
	}
}

func TestMailgun_Attachments(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"id":"x"}`)
	tp := NewMailgunTransport("k", "d.example", cs.URL)
	if _, err := tp.Send(context.Background(), attachedMessage()); err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType(cs.headers.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(bytes.NewReader(cs.body), params["boundary"])
	var got []struct{ filename, contentType, data string }
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if p.FormName() != "attachment" {
			continue
		}
		data, _ := io.ReadAll(p)
		got = append(got, struct{ filename, contentType, data string }{
			p.FileName(), p.Header.Get("Content-Type"), string(data),
		})
	}
	if len(got) != 2 {
		t.Fatalf("attachment parts = %d", len(got))
	}
	if got[0].filename != "license.pdf" || got[0].contentType != "application/pdf" || got[0].data != "%PDF-1.4 fake" {
		t.Errorf("part = %+v", got[0])
	}
}

func TestSES_Attachments(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"MessageId":"x"}`)
	tp := NewSESTransport("AKID", "sec", "us-east-1", cs.URL)
	if _, err := tp.Send(context.Background(), attachedMessage()); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Content struct {
			Simple struct {
				Attachments []struct{ RawContent, FileName, ContentType string }
			}
		}
	}
	if err := json.Unmarshal(cs.body, &body); err != nil {
		t.Fatal(err)
	}
	atts := body.Content.Simple.Attachments
	if len(atts) != 2 || atts[0].FileName != "license.pdf" {
		t.Fatalf("attachments = %+v", atts)
	}
	raw, _ := base64.StdEncoding.DecodeString(atts[0].RawContent)
	if string(raw) != "%PDF-1.4 fake" {
		t.Errorf("RawContent round-trip: %q", raw)
	}
}

func TestMailChannels_Attachments(t *testing.T) {
	cs := newCaptureServer(t, http.StatusAccepted, `{"results":[{"index":0,"message_id":"x"}]}`)
	tp := NewMailChannelsTransport("k", cs.URL)
	if _, err := tp.Send(context.Background(), attachedMessage()); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Attachments []struct {
			Content  string `json:"content"`
			Filename string `json:"filename"`
			Type     string `json:"type"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(cs.body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Attachments) != 2 || body.Attachments[1].Filename != "photo.png" || body.Attachments[1].Type != "image/png" {
		t.Fatalf("attachments = %+v", body.Attachments)
	}
	raw, err := base64.StdEncoding.DecodeString(body.Attachments[0].Content)
	if err != nil || string(raw) != "%PDF-1.4 fake" {
		t.Errorf("content round-trip: %q err=%v", raw, err)
	}
	if body.Attachments[0].Filename != "license.pdf" || body.Attachments[0].Type != "application/pdf" {
		t.Errorf("attachment meta = %+v", body.Attachments[0])
	}
}

func TestSMTPOut_Attachments_MultipartMixed(t *testing.T) {
	srv := startFakeSMTPServer(t)
	tp := newSMTPTestTransport(t, srv, TLSNone)

	msg := attachedMessage()
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	m, err := mail.ReadMessage(bytes.NewReader(srv.Sessions[0].Data))
	if err != nil {
		t.Fatal(err)
	}
	mediaType, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/mixed" {
		t.Fatalf("Content-Type = %q err=%v", mediaType, err)
	}
	mr := multipart.NewReader(m.Body, params["boundary"])

	// Part 1: the text body.
	p1, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p1.Header.Get("Content-Type"), "text/plain") {
		t.Errorf("first part = %q", p1.Header.Get("Content-Type"))
	}
	b1, _ := io.ReadAll(p1)
	if !strings.Contains(string(b1), "body") {
		t.Errorf("body part = %q", b1)
	}

	// Parts 2-3: attachments, base64.
	for i, want := range []struct{ name, ct, data string }{
		{"license.pdf", "application/pdf", "%PDF-1.4 fake"},
		{"photo.png", "image/png", "\x89PNG fake"},
	} {
		p, err := mr.NextPart()
		if err != nil {
			t.Fatalf("part %d: %v", i+2, err)
		}
		if got := p.Header.Get("Content-Type"); got != want.ct {
			t.Errorf("part %d Content-Type = %q", i+2, got)
		}
		disp, dparams, _ := mime.ParseMediaType(p.Header.Get("Content-Disposition"))
		if disp != "attachment" || dparams["filename"] != want.name {
			t.Errorf("part %d disposition = %q %v", i+2, disp, dparams)
		}
		// multipart.Reader does not auto-decode; decode the base64 body.
		enc, _ := io.ReadAll(p)
		raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.ReplaceAll(string(enc), "\r", ""), "\n", ""))
		if err != nil || string(raw) != want.data {
			t.Errorf("part %d data = %q err=%v", i+2, raw, err)
		}
	}
}

func TestSMTPOut_HTMLPlusAttachments_NestedAlternative(t *testing.T) {
	srv := startFakeSMTPServer(t)
	tp := newSMTPTestTransport(t, srv, TLSNone)

	msg := attachedMessage()
	msg.BodyHTML = "<p>html body</p>"
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	m, _ := mail.ReadMessage(bytes.NewReader(srv.Sessions[0].Data))
	mediaType, params, _ := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if mediaType != "multipart/mixed" {
		t.Fatalf("top = %q", mediaType)
	}
	mr := multipart.NewReader(m.Body, params["boundary"])
	p1, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	innerType, innerParams, _ := mime.ParseMediaType(p1.Header.Get("Content-Type"))
	if innerType != "multipart/alternative" {
		t.Fatalf("first mixed part = %q, want nested alternative", innerType)
	}
	inner := multipart.NewReader(p1, innerParams["boundary"])
	ip1, _ := inner.NextPart()
	if !strings.HasPrefix(ip1.Header.Get("Content-Type"), "text/plain") {
		t.Errorf("alternative part 1 = %q", ip1.Header.Get("Content-Type"))
	}
	ip2, err := inner.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ip2.Header.Get("Content-Type"), "text/html") {
		t.Errorf("alternative part 2 = %q", ip2.Header.Get("Content-Type"))
	}
}

// FR92 injection extension: a hostile filename cannot smuggle headers
// into the DATA blob on the one transport where we build MIME by hand.
func TestSMTPOut_AttachmentFilename_NoHeaderInjection(t *testing.T) {
	srv := startFakeSMTPServer(t)
	tp := newSMTPTestTransport(t, srv, TLSNone)

	msg := Message{
		From: "a@example.com", To: []string{"b@example.com"},
		Subject: "S", BodyText: "body",
		Attachments: []Attachment{{
			Filename:    "x.pdf\r\nBcc: attacker@evil.com",
			ContentType: "application/pdf",
			Data:        []byte("%PDF"),
		}},
	}
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	m, err := mail.ReadMessage(bytes.NewReader(srv.Sessions[0].Data))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Header.Get("Bcc"); got != "" {
		t.Fatalf("Bcc smuggled via filename: %q", got)
	}
	// The part headers must not contain a raw injected header line.
	if bytes.Contains(srv.Sessions[0].Data, []byte("\r\nBcc: attacker@evil.com")) {
		t.Error("raw injected header line present in DATA")
	}
}
