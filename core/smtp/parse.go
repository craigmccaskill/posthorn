package smtp

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"

	"github.com/craigmccaskill/posthorn/template"
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
//   - For multipart bodies, the first `text/plain` part becomes BodyText
//     and the first `text/html` part becomes BodyHTML (FR75). HTML-only
//     messages — previously rejected 554 in v1.x — are accepted, with
//     BodyText auto-derived from the HTML so the outbound mail always
//     carries a readable text part (FR72 reuse).
//
//   - passthrough is the listener's validated passthrough header list
//     (FR97, FR98, ADR-27); nil copies nothing. It can only ever name
//     list-management headers, so the first invariant is untouched.
func parseMIMEToMessage(data []byte, envelopeFrom string, envelopeRcpts []string, passthrough []string) (transport.Message, error) {
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

	bodyText, bodyHTML, err := extractBody(m)
	if err != nil {
		return transport.Message{}, err
	}
	if bodyText == "" && bodyHTML != "" {
		// FR75: HTML-only inbound gets a derived text part so the
		// outbound send is always well-formed multipart.
		bodyText = template.HTMLToText(bodyHTML)
	}

	headers, err := passthroughHeaders(m.Header, passthrough)
	if err != nil {
		return transport.Message{}, err
	}

	return transport.Message{
		From:     fromHdr,
		To:       append([]string(nil), envelopeRcpts...), // FR68/NFR22: envelope only
		ReplyTo:  replyTo,
		Subject:  subject,
		BodyText: bodyText,
		BodyHTML: bodyHTML,
		Headers:  headers,
	}, nil
}

// errPassthroughValue marks a passthrough header whose value can't be
// carried. The session answers it with 554 5.6.0 (FR98) rather than the
// generic malformed-message 550.
var errPassthroughValue = errors.New("invalid passthrough header value")

// passthroughNames canonicalizes a listener's passthrough_headers and
// refuses anything off the allowlist or listed twice (FR97). The config
// package runs the same check at load; this one makes a listener built
// any other way fail in New instead of quietly carrying nothing.
func passthroughNames(names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for i, name := range names {
		canon, ok := transport.PassthroughHeader(name)
		if !ok {
			return nil, fmt.Errorf("passthrough_headers[%d] %q: not on the allowlist (%s)", i, name, strings.Join(transport.PassthroughHeaderAllowlist, ", "))
		}
		if seen[canon] {
			return nil, fmt.Errorf("passthrough_headers[%d] %q: duplicate", i, name)
		}
		seen[canon] = true
		out = append(out, canon)
	}
	return out, nil
}

// passthroughHeaders copies the configured headers off the inbound
// message in configuration order, as received (no RFC 2047 decoding).
// names is the output of passthroughNames.
//
// Each header is carried at most once: the RFCs that define these
// headers allow one of each, so the first occurrence is taken and any
// repeat is left behind. A header present with an empty value is not
// carried. A value that fails transport.CheckHeaderValue (CR or LF,
// other control or non-ASCII bytes, or too long for one header line)
// fails the whole message: the app asked for this header to reach the
// provider, and sending the mail without it would drop it silently.
func passthroughHeaders(h mail.Header, names []string) ([]transport.Header, error) {
	var out []transport.Header
	for _, name := range names {
		values := h[name]
		if len(values) == 0 || values[0] == "" {
			continue
		}
		if err := transport.CheckHeaderValue(name, values[0]); err != nil {
			return nil, fmt.Errorf("%w: %v", errPassthroughValue, err)
		}
		out = append(out, transport.Header{Name: name, Value: values[0]})
	}
	return out, nil
}

// decodeTransferEncoding wraps r so reads yield decoded content, based on
// the part's Content-Transfer-Encoding header. quoted-printable and base64
// are decoded; 7bit/8bit/binary and an absent header pass through
// unwrapped, matching RFC 2045 default (7bit).
func decodeTransferEncoding(cte string, r io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "quoted-printable":
		return quotedprintable.NewReader(r)
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, r)
	default:
		return r
	}
}

// extractBody returns the text/plain and text/html content of a parsed
// MIME message (FR75). For multipart messages, the first part of each
// type wins. Single-part text/plain fills only text; single-part
// text/html fills only html (the caller derives the text part). A
// message with neither is an error.
func extractBody(m *mail.Message) (text, html string, err error) {
	contentType := m.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		// No Content-Type → assume text/plain US-ASCII per RFC 822.
		buf, rerr := io.ReadAll(m.Body)
		if rerr != nil {
			return "", "", fmt.Errorf("read body: %w", rerr)
		}
		return string(buf), "", nil
	}

	switch {
	case mediaType == "" || strings.HasPrefix(mediaType, "text/plain"):
		buf, rerr := io.ReadAll(decodeTransferEncoding(m.Header.Get("Content-Transfer-Encoding"), m.Body))
		if rerr != nil {
			return "", "", fmt.Errorf("read body: %w", rerr)
		}
		return string(buf), "", nil
	case strings.HasPrefix(mediaType, "text/html"):
		buf, rerr := io.ReadAll(decodeTransferEncoding(m.Header.Get("Content-Transfer-Encoding"), m.Body))
		if rerr != nil {
			return "", "", fmt.Errorf("read body: %w", rerr)
		}
		return "", string(buf), nil
	case strings.HasPrefix(mediaType, "multipart/"):
		boundary := params["boundary"]
		if boundary == "" {
			return "", "", fmt.Errorf("multipart Content-Type missing boundary")
		}
		return readPartsFromMultipart(m.Body, boundary)
	default:
		return "", "", fmt.Errorf("unsupported Content-Type: %s", mediaType)
	}
}

// readPartsFromMultipart walks the parts of a multipart body and
// captures the first text/plain and the first text/html content. At
// least one of the two must exist.
func readPartsFromMultipart(body io.Reader, boundary string) (text, html string, err error) {
	mr := multipart.NewReader(body, boundary)
	for {
		part, perr := mr.NextPart()
		if perr == io.EOF {
			if text == "" && html == "" {
				return "", "", fmt.Errorf("multipart message has no text/plain or text/html part")
			}
			return text, html, nil
		}
		if perr != nil {
			return "", "", fmt.Errorf("multipart read: %w", perr)
		}
		ct := part.Header.Get("Content-Type")
		mediaType, _, _ := mime.ParseMediaType(ct)
		switch {
		case (mediaType == "" || strings.HasPrefix(mediaType, "text/plain")) && text == "":
			buf, rerr := io.ReadAll(decodeTransferEncoding(part.Header.Get("Content-Transfer-Encoding"), part))
			if rerr != nil {
				_ = part.Close()
				return "", "", fmt.Errorf("read text/plain part: %w", rerr)
			}
			text = string(buf)
		case strings.HasPrefix(mediaType, "text/html") && html == "":
			buf, rerr := io.ReadAll(decodeTransferEncoding(part.Header.Get("Content-Transfer-Encoding"), part))
			if rerr != nil {
				_ = part.Close()
				return "", "", fmt.Errorf("read text/html part: %w", rerr)
			}
			html = string(buf)
		}
		_ = part.Close()
	}
}
