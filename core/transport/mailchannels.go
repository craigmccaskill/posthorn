package transport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"strings"
	"time"
)

// MailChannels Email API transport (#133).
//
// Per ADR-1: bespoke client, no third-party SDK. The surface is one
// endpoint (POST /tx/v1/send) and the dependency cost outweighs the LOC
// saved.
//
// NFR1 enforcement: every submitter-controlled value reaches MailChannels
// exclusively through json.Marshal of structs with named fields. There is
// no path in this file that constructs HTTP headers or email headers via
// string concatenation of user input. MailAddresses are parsed with
// net/mail into the API's {email, name} objects; an unparseable address is
// carried verbatim in the email field as inert data rather than being
// dropped or rewritten.
//
// DKIM is managed on the MailChannels side (managed keys), so no signing
// key ever lives in a Posthorn config (settled on #133).

const (
	defaultMailChannelsBaseURL = "https://api.mailchannels.net/tx/v1"
	mailchannelsSendPath       = "/send"
	mailchannelsRequestTimeout = 5 * time.Second
	// mailchannelsResponseSizeLimit caps the response body we read into
	// memory. Success bodies are tiny JSON; this prevents a hostile or
	// misbehaving upstream from forcing us to allocate.
	mailchannelsResponseSizeLimit = 64 * 1024
)

// mailchannelsHTTPClient is the package-level HTTP client shared across
// all MailChannelsTransport instances. Sharing one client lets the
// connection pool amortize across config reloads and multiple endpoints.
var mailchannelsHTTPClient = &http.Client{
	Timeout: mailchannelsRequestTimeout,
}

// MailChannelsTransport implements Transport against the MailChannels
// Email API (SendGrid-shaped JSON).
type MailChannelsTransport struct {
	// APIKey is the MailChannels API key. Sent via the X-Api-Key header.
	// Never logged (NFR3).
	APIKey string

	// BaseURL is the API base. Empty means use the production default.
	// Tests set this to an httptest.NewServer URL; production never sets it.
	BaseURL string
}

// NewMailChannelsTransport constructs a transport. baseURL may be empty.
func NewMailChannelsTransport(apiKey, baseURL string) *MailChannelsTransport {
	if baseURL == "" {
		baseURL = defaultMailChannelsBaseURL
	}
	return &MailChannelsTransport{
		APIKey:  apiKey,
		BaseURL: baseURL,
	}
}

// mailchannelsRequest is the JSON payload sent to /send. Field names
// follow MailChannels' spec exactly.
//
// SECURITY: adding a field here is a security-relevant change. The set of
// fields defines what submitter-controlled data crosses to MailChannels.
// Review NFR1 implications before adding. dkim_private_key and friends
// are deliberately absent: DKIM keys stay with MailChannels.
type mailchannelsRequest struct {
	Personalizations []mailchannelsPersonalization `json:"personalizations"`
	From             mailchannelsAddress           `json:"from"`
	ReplyTo          *mailchannelsAddress          `json:"reply_to,omitempty"`
	Subject          string                        `json:"subject"`
	Content          []mailchannelsContent         `json:"content"`
	Attachments      []mailchannelsAttachment      `json:"attachments,omitempty"`
	// Headers is the allowlisted passthrough (ADR-27, FR99). MailChannels
	// takes a name→value object and accepts List-Unsubscribe and
	// List-Unsubscribe-Post in it; checkHeaders has already refused a
	// repeated name, so nothing is overwritten or joined here.
	Headers map[string]string `json:"headers,omitempty"`
}

type mailchannelsPersonalization struct {
	To []mailchannelsAddress `json:"to"`
}

type mailchannelsAddress struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

type mailchannelsContent struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type mailchannelsAttachment struct {
	Content  string `json:"content"` // base64
	Filename string `json:"filename"`
	Type     string `json:"type"`
}

// mailchannelsSendResults is the 202 Accepted body shape. Only the first
// message ID is captured for SendResult.MessageID; other fields are
// ignored.
type mailchannelsSendResults struct {
	RequestID string                   `json:"request_id"`
	Results   []mailchannelsSendResult `json:"results"`
}

type mailchannelsSendResult struct {
	Index     int    `json:"index"`
	MessageID string `json:"message_id"`
	Status    string `json:"status"`
	Reason    string `json:"reason"`
}

// mailchannelsErrorResponse is the 4xx/5xx body shape MailChannels
// returns. Only the messages are captured for the operator-facing
// TransportError.Message.
type mailchannelsErrorResponse struct {
	Errors []string `json:"errors"`
}

// mailchannelsAddressOf converts one RFC 5322 address string into the
// API's {email, name} object. "Craig Mc <craig@example.com>" splits; a
// bare address passes through with no name. An address net/mail refuses
// (CR/LF payloads, malformed input) is carried verbatim in the email
// field: it reaches the provider as inert JSON string data — NFR1 holds
// by construction — and MailChannels rejects it terminally if it is not
// an address.
func mailchannelsAddressOf(addr string) mailchannelsAddress {
	if parsed, err := mail.ParseAddress(addr); err == nil {
		return mailchannelsAddress{Email: parsed.Address, Name: parsed.Name}
	}
	return mailchannelsAddress{Email: addr}
}

// Send implements Transport.
//
// Status code mapping (per #133 acceptance criteria):
//
//	202        → success (parse `results[0].message_id` for MessageID)
//	429        → ErrRateLimited
//	5xx        → ErrTransient
//	4xx (other)→ ErrTerminal (400 bad request, 403 bad key / unauthorized
//	             domain, 413 payload too large are the documented ones)
//	network/timeout/ctx → ErrTransient (caller will retry once)
func (m *MailChannelsTransport) Send(ctx context.Context, msg Message) (SendResult, error) {
	if err := checkHeaders(msg); err != nil {
		return SendResult{}, err
	}
	content := []mailchannelsContent{
		{Type: "text/plain", Value: msg.BodyText},
	}
	if msg.BodyHTML != "" {
		content = append(content, mailchannelsContent{
			Type:  "text/html",
			Value: msg.BodyHTML,
		})
	}
	body := mailchannelsRequest{
		Personalizations: []mailchannelsPersonalization{{
			To: make([]mailchannelsAddress, 0, len(msg.To)),
		}},
		From:    mailchannelsAddressOf(msg.From),
		Subject: msg.Subject,
		Content: content,
	}
	for _, to := range msg.To {
		body.Personalizations[0].To = append(body.Personalizations[0].To, mailchannelsAddressOf(to))
	}
	if msg.ReplyTo != "" {
		replyTo := mailchannelsAddressOf(msg.ReplyTo)
		body.ReplyTo = &replyTo
	}
	for _, h := range msg.Headers {
		if body.Headers == nil {
			body.Headers = map[string]string{}
		}
		body.Headers[h.Name] = h.Value
	}
	for _, a := range msg.Attachments {
		body.Attachments = append(body.Attachments, mailchannelsAttachment{
			Filename: a.Filename,
			Content:  base64.StdEncoding.EncodeToString(a.Data),
			Type:     a.ContentType,
		})
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return SendResult{}, &TransportError{
			Class:   ErrTerminal,
			Cause:   err,
			Message: "encode mailchannels request",
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.BaseURL+mailchannelsSendPath, bytes.NewReader(buf))
	if err != nil {
		return SendResult{}, &TransportError{
			Class:   ErrTerminal,
			Cause:   err,
			Message: "build mailchannels request",
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Api-Key", m.APIKey)

	resp, err := mailchannelsHTTPClient.Do(req)
	if err != nil {
		return SendResult{}, &TransportError{
			Class:   ErrTransient,
			Cause:   err,
			Message: "mailchannels request failed",
		}
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, mailchannelsResponseSizeLimit))

	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted:
		return SendResult{MessageID: mailchannelsMessageID(respBody)}, nil

	case resp.StatusCode == http.StatusTooManyRequests:
		return SendResult{}, &TransportError{
			Class:   ErrRateLimited,
			Status:  resp.StatusCode,
			Message: mailchannelsErrorMessage(respBody, "mailchannels rate limit"),
		}

	case resp.StatusCode >= 500:
		return SendResult{}, &TransportError{
			Class:   ErrTransient,
			Status:  resp.StatusCode,
			Message: mailchannelsErrorMessage(respBody, "mailchannels server error"),
		}

	case resp.StatusCode >= 400:
		return SendResult{}, &TransportError{
			Class:   ErrTerminal,
			Status:  resp.StatusCode,
			Message: mailchannelsErrorMessage(respBody, "mailchannels rejected request"),
		}

	default:
		return SendResult{}, &TransportError{
			Class:   ErrTerminal,
			Status:  resp.StatusCode,
			Message: fmt.Sprintf("unexpected mailchannels status %d", resp.StatusCode),
		}
	}
}

// mailchannelsMessageID extracts `results[0].message_id` from a success
// response body. Returns "" on any parse failure — a missing message ID
// degrades logging but must never fail the send (MailChannels already
// accepted the message).
func mailchannelsMessageID(body []byte) string {
	var resp mailchannelsSendResults
	if err := json.Unmarshal(body, &resp); err != nil {
		return ""
	}
	if len(resp.Results) == 0 {
		return ""
	}
	return resp.Results[0].MessageID
}

// mailchannelsErrorMessage joins the human-readable messages from a
// MailChannels error response. Falls back to the supplied generic string
// when parsing fails or the list is empty. Result is logged and may
// appear in 502 response bodies; it MUST NOT include the API key. Since
// this function only reads the upstream response body, never the request,
// that holds by construction.
func mailchannelsErrorMessage(body []byte, fallback string) string {
	var resp mailchannelsErrorResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fallback
	}
	msgs := make([]string, 0, len(resp.Errors))
	for _, e := range resp.Errors {
		if e != "" {
			msgs = append(msgs, e)
		}
	}
	if len(msgs) == 0 {
		return fallback
	}
	return strings.Join(msgs, "; ")
}

var _ Transport = (*MailChannelsTransport)(nil)

// Registry registration. Lets the config layer dispatch validation and
// construction without hardcoding "mailchannels" — see registry.go.
func init() {
	Register(Registration{
		Type:           "mailchannels",
		Validate:       validateMailChannelsSettings,
		Build:          buildMailChannelsFromSettings,
		CarriesHeaders: true,
	})
}

func validateMailChannelsSettings(settings map[string]any) error {
	apiKey, ok := settings["api_key"].(string)
	if !ok || apiKey == "" {
		return fmt.Errorf("mailchannels transport requires settings.api_key")
	}
	return nil
}

func buildMailChannelsFromSettings(settings map[string]any) (Transport, error) {
	apiKey, _ := settings["api_key"].(string)
	if apiKey == "" {
		return nil, fmt.Errorf("mailchannels: api_key is empty")
	}
	// base_url is a test-only escape hatch — production never sets it.
	baseURL, _ := settings["base_url"].(string)
	return NewMailChannelsTransport(apiKey, baseURL), nil
}
