package gateway_test

import (
	"bytes"
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/craigmccaskill/posthorn/config"
	"github.com/craigmccaskill/posthorn/gateway"
	"github.com/craigmccaskill/posthorn/transport"
)

// recordingTransport captures everything passed to Send for assertion.
type recordingTransport struct {
	sent    []transport.Message
	sendErr error
}

func (r *recordingTransport) Send(_ context.Context, msg transport.Message) error {
	r.sent = append(r.sent, msg)
	return r.sendErr
}

// newTestHandler constructs a Handler with a sensible default config and
// the provided transport. Tests that need different config call gateway.New
// directly.
func newTestHandler(t *testing.T, transportImpl transport.Transport) *gateway.Handler {
	t.Helper()
	cfg := config.EndpointConfig{
		Path:    "/test",
		To:      []string{"to@example.com"},
		From:    "from@example.com",
		Subject: "Subject text",
		Body:    "Body text",
	}
	h, err := gateway.New(cfg, transportImpl)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

// urlencodedRequest builds a POST request with form-urlencoded body.
func urlencodedRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

// --- Construction ---

func TestNew_NilTransport(t *testing.T) {
	_, err := gateway.New(config.EndpointConfig{}, nil)
	if err == nil {
		t.Fatal("New: expected error for nil transport")
	}
	if !strings.Contains(err.Error(), "transport") {
		t.Errorf("error: %v", err)
	}
}

func TestNew_OK(t *testing.T) {
	h, err := gateway.New(config.EndpointConfig{}, &recordingTransport{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if h == nil {
		t.Fatal("New: returned nil handler with nil error")
	}
}

// --- Method check ---

func TestHandler_GET_MethodNotAllowed(t *testing.T) {
	h := newTestHandler(t, &recordingTransport{})
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestHandler_PUT_MethodNotAllowed(t *testing.T) {
	h := newTestHandler(t, &recordingTransport{})
	req := httptest.NewRequest(http.MethodPut, "/test", strings.NewReader("x=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestHandler_NonPOST_DoesNotCallTransport(t *testing.T) {
	rt := &recordingTransport{}
	h := newTestHandler(t, rt)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got := len(rt.sent); got != 0 {
		t.Errorf("transport called %d times on GET; want 0", got)
	}
}

// --- Content-type check ---

func TestHandler_NoContentType_BadRequest(t *testing.T) {
	h := newTestHandler(t, &recordingTransport{})
	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader("x=1"))
	// no Content-Type header
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandler_JSONContentType_BadRequest(t *testing.T) {
	h := newTestHandler(t, &recordingTransport{})
	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(`{"x":1}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandler_FormEncodedWithCharset_OK(t *testing.T) {
	// Content-Type may include parameters like "; charset=utf-8".
	// The handler must accept this.
	rt := &recordingTransport{}
	h := newTestHandler(t, rt)
	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader("x=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandler_ContentTypeCaseInsensitive(t *testing.T) {
	// HTTP headers are case-insensitive; "APPLICATION/X-WWW-FORM-URLENCODED"
	// is technically valid even if unusual. RFC 7231 allows this.
	rt := &recordingTransport{}
	h := newTestHandler(t, rt)
	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader("x=1"))
	req.Header.Set("Content-Type", "Application/X-WWW-Form-URLEncoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// --- Multipart ---

func TestHandler_MultipartFormData_OK(t *testing.T) {
	// Build a real multipart body so the test exercises the multipart
	// path through net/http's form parser.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("name", "craig"); err != nil {
		t.Fatalf("WriteField: %v", err)
	}
	if err := mw.WriteField("message", "hello"); err != nil {
		t.Fatalf("WriteField: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rt := &recordingTransport{}
	h := newTestHandler(t, rt)
	req := httptest.NewRequest(http.MethodPost, "/test", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType()) // includes boundary
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := len(rt.sent); got != 1 {
		t.Fatalf("transport calls = %d, want 1", got)
	}
}

// --- Successful POST ---

func TestHandler_POST_Success(t *testing.T) {
	rt := &recordingTransport{}
	h := newTestHandler(t, rt)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("name=craig&message=hello"))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := len(rt.sent); got != 1 {
		t.Fatalf("transport calls = %d, want 1", got)
	}
}

func TestHandler_POST_TransportReceivesConfiguredFields(t *testing.T) {
	rt := &recordingTransport{}
	h := newTestHandler(t, rt)
	h.ServeHTTP(httptest.NewRecorder(), urlencodedRequest("name=craig&message=hello"))

	if len(rt.sent) != 1 {
		t.Fatalf("expected one Send call, got %d", len(rt.sent))
	}
	msg := rt.sent[0]
	if msg.From != "from@example.com" {
		t.Errorf("From = %q, want %q", msg.From, "from@example.com")
	}
	if len(msg.To) != 1 || msg.To[0] != "to@example.com" {
		t.Errorf("To = %v, want [to@example.com]", msg.To)
	}
	// Subject and BodyText pass through verbatim in Story 2.2; templating
	// (Story 2.4) will replace these with rendered output.
	if msg.Subject != "Subject text" {
		t.Errorf("Subject = %q", msg.Subject)
	}
	if msg.BodyText != "Body text" {
		t.Errorf("BodyText = %q", msg.BodyText)
	}
}

// --- Transport failures ---

func TestHandler_TransportError_BadGateway(t *testing.T) {
	rt := &recordingTransport{
		sendErr: &transport.TransportError{
			Class:   transport.ErrTerminal,
			Status:  401,
			Message: "unauthorized",
		},
	}
	h := newTestHandler(t, rt)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("x=1"))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestHandler_TransportError_BodyDoesNotLeakDetails(t *testing.T) {
	// Architecture doc Open Q5: 502 body must not reveal whether the failure
	// was config (4xx upstream) vs runtime (network). Pin that contract.
	rt := &recordingTransport{
		sendErr: &transport.TransportError{
			Class:   transport.ErrTerminal,
			Status:  401,
			Message: "Postmark says: API key invalid", // operator-facing detail
			Cause:   errors.New("internal"),
		},
	}
	h := newTestHandler(t, rt)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("x=1"))

	body := rec.Body.String()
	if strings.Contains(body, "Postmark") || strings.Contains(body, "401") || strings.Contains(body, "API key") {
		t.Errorf("502 body leaks upstream detail: %q", body)
	}
}

// --- Body parsing edge cases ---

func TestHandler_ParseFormError_BadRequest(t *testing.T) {
	// A malformed urlencoded body (invalid percent-encoding) should produce
	// a 400 from r.ParseForm. Use "%ZZ" which is not a valid hex escape.
	h := newTestHandler(t, &recordingTransport{})
	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader("name=%ZZ"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandler_EmptyBody_OK(t *testing.T) {
	// Empty body is technically a valid form (zero fields). Validation in
	// Story 2.3 will reject this if `required` lists fields, but the bare
	// handler accepts it.
	rt := &recordingTransport{}
	h := newTestHandler(t, rt)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest(""))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}
