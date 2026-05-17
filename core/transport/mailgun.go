package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

// Mailgun HTTP API transport (FR48).
//
// Per ADR-1: bespoke client, no Mailgun SDK. The API is small enough that
// the dependency cost outweighs the LOC saved.
//
// NFR1 enforcement: submitter-controlled values reach Mailgun exclusively
// via mime/multipart.Writer field writes. The writer handles boundary
// generation, field-name escaping, and value encoding — there is no path
// in this file that constructs HTTP headers or body bytes via string
// concatenation of user input.

const (
	defaultMailgunBaseURL    = "https://api.mailgun.net"
	mailgunSendPathPrefix    = "/v3/" // path becomes /v3/<domain>/messages
	mailgunSendPathSuffix    = "/messages"
	mailgunRequestTimeout    = 5 * time.Second
	mailgunResponseSizeLimit = 64 * 1024
)

var mailgunHTTPClient = &http.Client{
	Timeout: mailgunRequestTimeout,
}

// MailgunTransport implements Transport against Mailgun's HTTP API.
//
// Mailgun uses HTTP Basic auth with username "api" and the API key as the
// password. The token never appears in URL or body — only in the
// Authorization header set at request-construction time (NFR3).
type MailgunTransport struct {
	// APIKey is the Mailgun account API key. Sent as the Basic-auth
	// password. Never logged (NFR3).
	APIKey string

	// Domain is the sending domain registered with Mailgun. Path is
	// /v3/<Domain>/messages.
	Domain string

	// BaseURL is the API base. Empty means use the production default
	// (api.mailgun.net). Operators on the EU region set this to
	// `https://api.eu.mailgun.net`. Tests set httptest URLs.
	BaseURL string
}

// NewMailgunTransport constructs a transport. baseURL may be empty.
func NewMailgunTransport(apiKey, domain, baseURL string) *MailgunTransport {
	if baseURL == "" {
		baseURL = defaultMailgunBaseURL
	}
	return &MailgunTransport{
		APIKey:  apiKey,
		Domain:  domain,
		BaseURL: baseURL,
	}
}

// mailgunSuccessResponse captures the success body shape. Only the `id`
// is read for SendResult.MessageID; the human-readable `message` is
// intentionally ignored.
type mailgunSuccessResponse struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

// mailgunErrorResponse captures the error body shape.
type mailgunErrorResponse struct {
	Message string `json:"message"`
}

// Send implements Transport.
//
// Status code mapping (FR19-21 parallel to Postmark/Resend):
//
//	200        → success (parse `id` for MessageID)
//	429        → ErrRateLimited
//	5xx        → ErrTransient
//	4xx (other)→ ErrTerminal
//	network/timeout/ctx → ErrTransient (caller will retry once)
func (m *MailgunTransport) Send(ctx context.Context, msg Message) (SendResult, error) {
	// Build multipart body. mime/multipart.Writer escapes field names and
	// values structurally — the boundary string is generated; user values
	// are never substituted into protocol-level positions.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	if err := mw.WriteField("from", msg.From); err != nil {
		return SendResult{}, &TransportError{Class: ErrTerminal, Cause: err, Message: "encode mailgun from"}
	}
	// Mailgun accepts repeated `to` fields for multiple recipients OR a
	// comma-separated single value. We use repeated fields — the
	// multipart writer is structural, no concat ambiguity.
	for _, recipient := range msg.To {
		if err := mw.WriteField("to", recipient); err != nil {
			return SendResult{}, &TransportError{Class: ErrTerminal, Cause: err, Message: "encode mailgun to"}
		}
	}
	if msg.ReplyTo != "" {
		// Mailgun custom header convention: `h:<Header-Name>` form field
		// becomes a header on the outbound message. The h: prefix is part
		// of Mailgun's structured API, not user input.
		if err := mw.WriteField("h:Reply-To", msg.ReplyTo); err != nil {
			return SendResult{}, &TransportError{Class: ErrTerminal, Cause: err, Message: "encode mailgun reply-to"}
		}
	}
	if err := mw.WriteField("subject", msg.Subject); err != nil {
		return SendResult{}, &TransportError{Class: ErrTerminal, Cause: err, Message: "encode mailgun subject"}
	}
	if err := mw.WriteField("text", msg.BodyText); err != nil {
		return SendResult{}, &TransportError{Class: ErrTerminal, Cause: err, Message: "encode mailgun text"}
	}
	if err := mw.Close(); err != nil {
		return SendResult{}, &TransportError{Class: ErrTerminal, Cause: err, Message: "close mailgun multipart"}
	}

	url := m.BaseURL + mailgunSendPathPrefix + m.Domain + mailgunSendPathSuffix
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return SendResult{}, &TransportError{Class: ErrTerminal, Cause: err, Message: "build mailgun request"}
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth("api", m.APIKey)

	resp, err := mailgunHTTPClient.Do(req)
	if err != nil {
		return SendResult{}, &TransportError{
			Class:   ErrTransient,
			Cause:   err,
			Message: "mailgun request failed",
		}
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, mailgunResponseSizeLimit))

	switch {
	case resp.StatusCode == http.StatusOK:
		return SendResult{MessageID: mailgunMessageID(respBody)}, nil

	case resp.StatusCode == http.StatusTooManyRequests:
		return SendResult{}, &TransportError{
			Class:   ErrRateLimited,
			Status:  resp.StatusCode,
			Message: mailgunErrorMessage(respBody, "mailgun rate limit"),
		}

	case resp.StatusCode >= 500:
		return SendResult{}, &TransportError{
			Class:   ErrTransient,
			Status:  resp.StatusCode,
			Message: mailgunErrorMessage(respBody, "mailgun server error"),
		}

	case resp.StatusCode >= 400:
		return SendResult{}, &TransportError{
			Class:   ErrTerminal,
			Status:  resp.StatusCode,
			Message: mailgunErrorMessage(respBody, "mailgun rejected request"),
		}

	default:
		return SendResult{}, &TransportError{
			Class:   ErrTerminal,
			Status:  resp.StatusCode,
			Message: fmt.Sprintf("unexpected mailgun status %d", resp.StatusCode),
		}
	}
}

// mailgunMessageID extracts the `id` field from a 200 response body.
// Returns "" on any parse failure — a missing message ID degrades logging
// but must never fail the send.
func mailgunMessageID(body []byte) string {
	var resp mailgunSuccessResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return ""
	}
	return resp.ID
}

// mailgunErrorMessage extracts the `message` field from a Mailgun error
// response. Falls back to the supplied generic string when parsing fails
// or the field is empty.
func mailgunErrorMessage(body []byte, fallback string) string {
	var resp mailgunErrorResponse
	if err := json.Unmarshal(body, &resp); err != nil || resp.Message == "" {
		return fallback
	}
	return resp.Message
}

var _ Transport = (*MailgunTransport)(nil)

// Registry registration. Lets the config layer dispatch validation and
// construction without hardcoding "mailgun" — see registry.go.
func init() {
	Register(Registration{
		Type:     "mailgun",
		Validate: validateMailgunSettings,
		Build:    buildMailgunFromSettings,
	})
}

func validateMailgunSettings(settings map[string]any) error {
	apiKey, ok := settings["api_key"].(string)
	if !ok || apiKey == "" {
		return fmt.Errorf("mailgun transport requires settings.api_key")
	}
	domain, ok := settings["domain"].(string)
	if !ok || domain == "" {
		return fmt.Errorf("mailgun transport requires settings.domain")
	}
	return nil
}

func buildMailgunFromSettings(settings map[string]any) (Transport, error) {
	apiKey, _ := settings["api_key"].(string)
	if apiKey == "" {
		return nil, fmt.Errorf("mailgun: api_key is empty")
	}
	domain, _ := settings["domain"].(string)
	if domain == "" {
		return nil, fmt.Errorf("mailgun: domain is empty")
	}
	// base_url is for the EU endpoint or test servers.
	baseURL, _ := settings["base_url"].(string)
	return NewMailgunTransport(apiKey, domain, baseURL), nil
}
