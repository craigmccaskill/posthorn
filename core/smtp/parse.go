package smtp

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"

	"github.com/craigmccaskill/posthorn/transport"
)

// parseMIMEToMessage converts a DATA blob into a transport.Message
// (FR68, NFR22).
//
// Key NFR22 invariants:
//
//   - The transport.Message.To field is taken from envelopeRcpts (the
//     SMTP RCPT TO commands), NEVER from the MIME `To:`/`Cc:`/`Bcc:`
//     headers. A malicious client sending `Subject: hi\r\nBcc: victim`
//     can't add recipients to the outbound send — `Bcc:` lands in the
//     parsed header map but never reaches the transport.
//
//   - The MIME `From:` and `Subject:` headers are passed to the
//     transport as structured string values. The transport's own NFR1
//     defense (struct-based JSON marshaling, multipart writers) prevents
//     CRLF in those values from constructing sibling headers in the
//     outbound message.
//
//   - For multipart bodies we prefer the `text/plain` part; HTML-only
//     bodies are rejected (v2 will add HTML support).
func parseMIMEToMessage(data []byte, envelopeFrom string, envelopeRcpts []string) (transport.Message, error) {
	m, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		return transport.Message{}, fmt.Errorf("parse MIME headers: %w", err)
	}

	// Decode the Subject header (RFC 2047 unfold/decode if needed).
	dec := &mime.WordDecoder{}
	subject, err := dec.DecodeHeader(m.Header.Get("Subject"))
	if err != nil {
		// Pass through undecoded on error rather than failing the send.
		subject = m.Header.Get("Subject")
	}

	// From: header is what we'll set as the outbound From. Decode
	// any RFC 2047 encoded display name.
	fromHdr := m.Header.Get("From")
	if decoded, err := dec.DecodeHeader(fromHdr); err == nil {
		fromHdr = decoded
	}
	if fromHdr == "" {
		// Fall back to the envelope sender — RFC 5321 requires it
		// even when the message lacks a From header.
		fromHdr = envelopeFrom
	}

	// Reply-To: optional pass-through.
	replyTo := m.Header.Get("Reply-To")
	if decoded, err := dec.DecodeHeader(replyTo); err == nil {
		replyTo = decoded
	}

	body, err := extractPlainTextBody(m)
	if err != nil {
		return transport.Message{}, err
	}

	return transport.Message{
		From:     fromHdr,
		To:       append([]string(nil), envelopeRcpts...), // FR68/NFR22: envelope only
		ReplyTo:  replyTo,
		Subject:  subject,
		BodyText: body,
	}, nil
}

// extractPlainTextBody returns the text/plain content of a parsed MIME
// message. For multipart messages, walks the parts and prefers
// text/plain over text/html. HTML-only messages are rejected with a
// clear error (HTML body support is v2 scope).
func extractPlainTextBody(m *mail.Message) (string, error) {
	contentType := m.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		// No Content-Type → assume text/plain US-ASCII per RFC 822.
		buf, err := io.ReadAll(m.Body)
		if err != nil {
			return "", fmt.Errorf("read body: %w", err)
		}
		return string(buf), nil
	}

	switch {
	case mediaType == "" || strings.HasPrefix(mediaType, "text/plain"):
		buf, err := io.ReadAll(m.Body)
		if err != nil {
			return "", fmt.Errorf("read body: %w", err)
		}
		return string(buf), nil
	case strings.HasPrefix(mediaType, "text/html"):
		return "", fmt.Errorf("HTML-only message body not supported in v1.0; send multipart/alternative with a text/plain part")
	case strings.HasPrefix(mediaType, "multipart/"):
		boundary := params["boundary"]
		if boundary == "" {
			return "", fmt.Errorf("multipart Content-Type missing boundary")
		}
		return readPlainTextFromMultipart(m.Body, boundary)
	default:
		return "", fmt.Errorf("unsupported Content-Type: %s", mediaType)
	}
}

// readPlainTextFromMultipart walks the parts of a multipart body and
// returns the first text/plain content found. Returns an error if no
// text/plain part exists.
func readPlainTextFromMultipart(body io.Reader, boundary string) (string, error) {
	mr := multipart.NewReader(body, boundary)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return "", fmt.Errorf("multipart message has no text/plain part")
		}
		if err != nil {
			return "", fmt.Errorf("multipart read: %w", err)
		}
		ct := part.Header.Get("Content-Type")
		mediaType, _, _ := mime.ParseMediaType(ct)
		if mediaType == "" || strings.HasPrefix(mediaType, "text/plain") {
			buf, err := io.ReadAll(part)
			_ = part.Close()
			if err != nil {
				return "", fmt.Errorf("read text/plain part: %w", err)
			}
			return string(buf), nil
		}
		_ = part.Close()
	}
}
