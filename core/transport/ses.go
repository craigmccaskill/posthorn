package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// AWS SES (Simple Email Service) HTTP API transport (FR49).
//
// Uses the SESv2 SendEmail endpoint with SigV4 authentication. Per ADR-1
// and ADR-14: bespoke client (~150 LOC) plus the bespoke SigV4 primitive
// (~200 LOC, shared across any future AWS-signed transport). The
// dependency cost of aws-sdk-go-v2 (large transitive dep tree) is the
// tradeoff we're avoiding.
//
// NFR1 enforcement: every submitter-controlled value reaches SES through
// json.Marshal of a typed struct. There is no path in this file that
// constructs HTTP headers or email content via string concatenation of
// user input. SigV4 signing happens after request construction; the
// signature is opaque material, not user input.

const (
	sesServiceName        = "ses"
	sesSendPath           = "/v2/email/outbound-emails"
	sesRequestTimeout     = 5 * time.Second
	sesResponseSizeLimit  = 64 * 1024
)

var sesHTTPClient = &http.Client{
	Timeout: sesRequestTimeout,
}

// SESTransport implements Transport against AWS SESv2.
type SESTransport struct {
	Credentials SigV4Credentials
	Region      string

	// BaseURL is the endpoint host. Empty means use the per-region default
	// (https://email.<region>.amazonaws.com). Tests override with an
	// httptest URL.
	BaseURL string

	// now is injected for deterministic tests. Production uses time.Now.
	now func() time.Time
}

// NewSESTransport constructs a transport. baseURL may be empty. region
// must be a valid AWS region (e.g., "us-east-1").
func NewSESTransport(accessKeyID, secretAccessKey, region, baseURL string) *SESTransport {
	if baseURL == "" {
		baseURL = "https://email." + region + ".amazonaws.com"
	}
	return &SESTransport{
		Credentials: SigV4Credentials{
			AccessKeyID:     accessKeyID,
			SecretAccessKey: secretAccessKey,
		},
		Region:  region,
		BaseURL: baseURL,
		now:     time.Now,
	}
}

// sesRequest is the SESv2 SendEmail request payload. Field names match
// the API spec exactly (Pascal-cased per AWS convention).
//
// SECURITY: adding a field here is a security-relevant change. Review
// NFR1 implications before adding.
type sesRequest struct {
	FromEmailAddress string         `json:"FromEmailAddress"`
	Destination      sesDestination `json:"Destination"`
	ReplyToAddresses []string       `json:"ReplyToAddresses,omitempty"`
	Content          sesContent     `json:"Content"`
}

type sesDestination struct {
	ToAddresses []string `json:"ToAddresses"`
}

type sesContent struct {
	Simple sesSimple `json:"Simple"`
}

type sesSimple struct {
	Subject sesContentField `json:"Subject"`
	Body    sesBody         `json:"Body"`
}

type sesContentField struct {
	Data string `json:"Data"`
}

type sesBody struct {
	Text sesContentField `json:"Text"`
}

// sesSuccessResponse captures the SendEmail 200 body — only the
// MessageId is read for SendResult.MessageID.
type sesSuccessResponse struct {
	MessageId string `json:"MessageId"`
}

// sesErrorResponse captures SES error body shapes. SES uses two
// conventions across error types; we capture both.
type sesErrorResponse struct {
	Type    string `json:"__type"`
	Message string `json:"message"`
	// Some error responses use "Message" with capital M instead.
	MessageAlt string `json:"Message"`
}

// Send implements Transport.
//
// Status code mapping (FR19-21 parallel to other transports):
//
//	200        → success
//	429        → ErrRateLimited (SES uses Throttling exceptions for this)
//	5xx        → ErrTransient
//	4xx (other)→ ErrTerminal
//	network/timeout/ctx → ErrTransient (caller will retry once)
func (s *SESTransport) Send(ctx context.Context, msg Message) (SendResult, error) {
	payload := sesRequest{
		FromEmailAddress: msg.From,
		Destination:      sesDestination{ToAddresses: msg.To},
		Content: sesContent{
			Simple: sesSimple{
				Subject: sesContentField{Data: msg.Subject},
				Body:    sesBody{Text: sesContentField{Data: msg.BodyText}},
			},
		},
	}
	if msg.ReplyTo != "" {
		payload.ReplyToAddresses = []string{msg.ReplyTo}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return SendResult{}, &TransportError{Class: ErrTerminal, Cause: err, Message: "encode ses request"}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.BaseURL+sesSendPath, bytes.NewReader(body))
	if err != nil {
		return SendResult{}, &TransportError{Class: ErrTerminal, Cause: err, Message: "build ses request"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	SignRequest(req, body, s.Credentials, s.Region, sesServiceName, s.now())

	resp, err := sesHTTPClient.Do(req)
	if err != nil {
		return SendResult{}, &TransportError{
			Class:   ErrTransient,
			Cause:   err,
			Message: "ses request failed",
		}
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, sesResponseSizeLimit))

	switch {
	case resp.StatusCode == http.StatusOK:
		return SendResult{MessageID: sesMessageID(respBody)}, nil

	case resp.StatusCode == http.StatusTooManyRequests:
		return SendResult{}, &TransportError{
			Class:   ErrRateLimited,
			Status:  resp.StatusCode,
			Message: sesErrorMessage(respBody, "ses rate limit"),
		}

	case resp.StatusCode >= 500:
		return SendResult{}, &TransportError{
			Class:   ErrTransient,
			Status:  resp.StatusCode,
			Message: sesErrorMessage(respBody, "ses server error"),
		}

	case resp.StatusCode >= 400:
		return SendResult{}, &TransportError{
			Class:   ErrTerminal,
			Status:  resp.StatusCode,
			Message: sesErrorMessage(respBody, "ses rejected request"),
		}

	default:
		return SendResult{}, &TransportError{
			Class:   ErrTerminal,
			Status:  resp.StatusCode,
			Message: fmt.Sprintf("unexpected ses status %d", resp.StatusCode),
		}
	}
}

// sesMessageID extracts MessageId from a 200 response body. Returns ""
// on any parse failure — a missing message ID degrades logging but must
// never fail the send.
func sesMessageID(body []byte) string {
	var resp sesSuccessResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return ""
	}
	return resp.MessageId
}

// sesErrorMessage extracts a human-readable error from a SES error
// response. SES uses both "message" and "Message" across endpoints; we
// check both.
func sesErrorMessage(body []byte, fallback string) string {
	var resp sesErrorResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fallback
	}
	if resp.Message != "" {
		return resp.Message
	}
	if resp.MessageAlt != "" {
		return resp.MessageAlt
	}
	return fallback
}

var _ Transport = (*SESTransport)(nil)

// Registry registration.
func init() {
	Register(Registration{
		Type:     "ses",
		Validate: validateSESSettings,
		Build:    buildSESFromSettings,
	})
}

func validateSESSettings(settings map[string]any) error {
	akid, ok := settings["access_key_id"].(string)
	if !ok || akid == "" {
		return fmt.Errorf("ses transport requires settings.access_key_id")
	}
	sak, ok := settings["secret_access_key"].(string)
	if !ok || sak == "" {
		return fmt.Errorf("ses transport requires settings.secret_access_key")
	}
	region, ok := settings["region"].(string)
	if !ok || region == "" {
		return fmt.Errorf("ses transport requires settings.region")
	}
	return nil
}

func buildSESFromSettings(settings map[string]any) (Transport, error) {
	akid, _ := settings["access_key_id"].(string)
	sak, _ := settings["secret_access_key"].(string)
	region, _ := settings["region"].(string)
	if akid == "" || sak == "" || region == "" {
		return nil, fmt.Errorf("ses: access_key_id, secret_access_key, and region all required")
	}
	baseURL, _ := settings["base_url"].(string)
	return NewSESTransport(akid, sak, region, baseURL), nil
}
