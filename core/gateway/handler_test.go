package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	cfg := config.EndpointConfig{
		Subject: "S",
		Body:    "B",
	}
	h, err := gateway.New(cfg, &recordingTransport{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if h == nil {
		t.Fatal("New: returned nil handler with nil error")
	}
}

func TestNew_EmptyBody_Error(t *testing.T) {
	// Templating requires a non-empty body. Surfaces at construction.
	_, err := gateway.New(config.EndpointConfig{Subject: "S"}, &recordingTransport{})
	if err == nil {
		t.Fatal("expected error for empty body")
	}
}

func TestNew_TemplateParseError(t *testing.T) {
	cfg := config.EndpointConfig{Subject: "Bad: {{.x", Body: "B"}
	_, err := gateway.New(cfg, &recordingTransport{})
	if err == nil {
		t.Fatal("expected parse error")
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
	// Subject and Body templates have no template vars, so they render
	// to literal text. Form fields not named in templates appear in the
	// custom-fields passthrough block (Story 2.4 behavior).
	if msg.Subject != "Subject text" {
		t.Errorf("Subject = %q", msg.Subject)
	}
	if !strings.Contains(msg.BodyText, "Body text") {
		t.Errorf("BodyText missing template output: %q", msg.BodyText)
	}
	if !strings.Contains(msg.BodyText, "name: craig") {
		t.Errorf("BodyText missing passthrough name field: %q", msg.BodyText)
	}
	if !strings.Contains(msg.BodyText, "message: hello") {
		t.Errorf("BodyText missing passthrough message field: %q", msg.BodyText)
	}
}

// TestHandler_TemplateInterpolation verifies that subject/body templates
// receive form fields and render them.
func TestHandler_TemplateInterpolation(t *testing.T) {
	rt := &recordingTransport{}
	cfg := config.EndpointConfig{
		To:      []string{"to@example.com"},
		From:    "from@example.com",
		Subject: "Contact from {{.name}}",
		Body:    "Message: {{.message}}",
	}
	h, err := gateway.New(cfg, rt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	h.ServeHTTP(httptest.NewRecorder(), urlencodedRequest("name=craig&message=hello+there"))

	if len(rt.sent) != 1 {
		t.Fatalf("expected one Send call, got %d", len(rt.sent))
	}
	msg := rt.sent[0]
	if msg.Subject != "Contact from craig" {
		t.Errorf("Subject = %q, want interpolation", msg.Subject)
	}
	if !strings.HasPrefix(msg.BodyText, "Message: hello there") {
		t.Errorf("BodyText body section = %q", msg.BodyText)
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
	// Empty body is technically a valid form (zero fields). With no
	// `required` configured, no validation triggers. Bare handler accepts.
	rt := &recordingTransport{}
	h := newTestHandler(t, rt)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest(""))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// --- Validation: required fields ---

// handlerWithRequired builds a handler with the given required-fields list.
func handlerWithRequired(t *testing.T, rt transport.Transport, required []string, emailField string) *gateway.Handler {
	t.Helper()
	cfg := config.EndpointConfig{
		Path:       "/test",
		To:         []string{"to@example.com"},
		From:       "from@example.com",
		Subject:    "S",
		Body:       "B",
		Required:   required,
		EmailField: emailField,
	}
	h, err := gateway.New(cfg, rt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

// decodeValidationResponse decodes a 422 body into its parts for assertion.
type validationResponse struct {
	Error  string            `json:"error"`
	Code   string            `json:"code"`
	Fields map[string]string `json:"fields"`
}

func decodeValidationResponse(t *testing.T, body *bytes.Buffer) validationResponse {
	t.Helper()
	var v validationResponse
	if err := json.NewDecoder(body).Decode(&v); err != nil {
		t.Fatalf("decode 422 body: %v\nraw: %s", err, body.String())
	}
	return v
}

func TestHandler_RequiredFieldMissing_422(t *testing.T) {
	rt := &recordingTransport{}
	h := handlerWithRequired(t, rt, []string{"name", "message"}, "")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("name=craig"))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	v := decodeValidationResponse(t, rec.Body)
	if v.Code != "validation_failed" {
		t.Errorf("Code = %q", v.Code)
	}
	if v.Fields["message"] != "required" {
		t.Errorf("Fields[message] = %q, want required", v.Fields["message"])
	}
	if _, present := v.Fields["name"]; present {
		t.Errorf("Fields[name] should not be present (name was provided)")
	}
}

func TestHandler_RequiredFieldEmpty_422(t *testing.T) {
	rt := &recordingTransport{}
	h := handlerWithRequired(t, rt, []string{"name"}, "")

	// name=  (empty value, present-but-empty)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("name="))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}
	v := decodeValidationResponse(t, rec.Body)
	if v.Fields["name"] != "required" {
		t.Errorf("Fields[name] = %q, want required", v.Fields["name"])
	}
}

func TestHandler_MultipleMissing_AllReported(t *testing.T) {
	rt := &recordingTransport{}
	h := handlerWithRequired(t, rt, []string{"name", "email", "message"}, "")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest(""))

	v := decodeValidationResponse(t, rec.Body)
	for _, name := range []string{"name", "email", "message"} {
		if v.Fields[name] != "required" {
			t.Errorf("Fields[%s] = %q, want required (every missing field must be reported, not just first)", name, v.Fields[name])
		}
	}
}

func TestHandler_ValidationFailure_DoesNotCallTransport(t *testing.T) {
	rt := &recordingTransport{}
	h := handlerWithRequired(t, rt, []string{"name"}, "")

	h.ServeHTTP(httptest.NewRecorder(), urlencodedRequest(""))

	if got := len(rt.sent); got != 0 {
		t.Errorf("transport called %d times on validation failure; want 0", got)
	}
}

// --- Validation: email format ---

func TestHandler_InvalidEmailFormat_422(t *testing.T) {
	rt := &recordingTransport{}
	h := handlerWithRequired(t, rt, nil, "") // no required list; just email check

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("email=not-a-valid-email"))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	v := decodeValidationResponse(t, rec.Body)
	if v.Fields["email"] != "invalid email format" {
		t.Errorf("Fields[email] = %q", v.Fields["email"])
	}
}

func TestHandler_ValidEmail_OK(t *testing.T) {
	rt := &recordingTransport{}
	h := handlerWithRequired(t, rt, nil, "")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("email=craig@example.com"))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandler_EmailFieldOverride(t *testing.T) {
	// EmailField=contact_address means the validator looks at the
	// "contact_address" field, not "email". A literal "email" field with
	// junk should NOT trigger validation in this configuration.
	rt := &recordingTransport{}
	h := handlerWithRequired(t, rt, nil, "contact_address")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("email=junk&contact_address=valid@example.com"))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (only contact_address is validated)", rec.Code)
	}
}

func TestHandler_EmailFieldOverride_InvalidFormat(t *testing.T) {
	rt := &recordingTransport{}
	h := handlerWithRequired(t, rt, nil, "contact_address")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("contact_address=not-an-email"))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}
	v := decodeValidationResponse(t, rec.Body)
	if v.Fields["contact_address"] != "invalid email format" {
		t.Errorf("Fields[contact_address] = %q", v.Fields["contact_address"])
	}
}

func TestHandler_EmailFieldEmpty_NoEmailValidation(t *testing.T) {
	// If the configured email field is missing or empty, format validation
	// should not run — that's RequiredFields' job to catch missing-when-required.
	rt := &recordingTransport{}
	h := handlerWithRequired(t, rt, nil, "")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("name=craig"))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (no email field present, no validation triggered)", rec.Code)
	}
}

func TestHandler_RequiredAndFormatErrorTogether(t *testing.T) {
	// "email" is required AND has bad format.  Required should win for that
	// field per response.Validation precedence rule.
	rt := &recordingTransport{}
	h := handlerWithRequired(t, rt, []string{"email"}, "")

	// Empty value: required catches first.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("email="))

	v := decodeValidationResponse(t, rec.Body)
	if v.Fields["email"] != "required" {
		t.Errorf("Fields[email] = %q, want required (required check should fire before format on empty value)", v.Fields["email"])
	}
}

// --- Success response shape ---

func TestHandler_Success_JSONResponse(t *testing.T) {
	rt := &recordingTransport{}
	h := newTestHandler(t, rt)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("x=1"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

// --- Spam protection (Story 3.1 wiring) ---

// handlerWithSpam constructs a handler with spam-protection settings.
func handlerWithSpam(t *testing.T, rt transport.Transport, honeypot string, allowedOrigins []string, maxBodySize string) *gateway.Handler {
	t.Helper()
	cfg := config.EndpointConfig{
		Path:           "/test",
		To:             []string{"to@example.com"},
		From:           "from@example.com",
		Subject:        "S",
		Body:           "B",
		Honeypot:       honeypot,
		AllowedOrigins: allowedOrigins,
		MaxBodySize:    maxBodySize,
	}
	h, err := gateway.New(cfg, rt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func TestHandler_Honeypot_Triggered_Silent200(t *testing.T) {
	rt := &recordingTransport{}
	h := handlerWithSpam(t, rt, "_gotcha", nil, "")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("name=craig&_gotcha=bot"))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (silent reject)", rec.Code)
	}
	if got := len(rt.sent); got != 0 {
		t.Errorf("transport called %d times when honeypot triggered; want 0", got)
	}
}

func TestHandler_Honeypot_NotTriggered_Pass(t *testing.T) {
	rt := &recordingTransport{}
	h := handlerWithSpam(t, rt, "_gotcha", nil, "")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("name=craig"))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := len(rt.sent); got != 1 {
		t.Errorf("transport called %d times; want 1", got)
	}
}

func TestHandler_OriginAllowed(t *testing.T) {
	rt := &recordingTransport{}
	h := handlerWithSpam(t, rt, "", []string{"https://example.com"}, "")

	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader("x=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandler_OriginDenied_403(t *testing.T) {
	rt := &recordingTransport{}
	h := handlerWithSpam(t, rt, "", []string{"https://example.com"}, "")

	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader("x=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://attacker.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if got := len(rt.sent); got != 0 {
		t.Errorf("transport called %d times for denied origin; want 0", got)
	}
}

func TestHandler_OriginBothMissing_403_FailClosed(t *testing.T) {
	// NFR4: with allowed_origins configured, a request missing both
	// Origin AND Referer is rejected (fail-closed).
	rt := &recordingTransport{}
	h := handlerWithSpam(t, rt, "", []string{"https://example.com"}, "")

	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader("x=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// no Origin, no Referer
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (fail-closed when allowed_origins set)", rec.Code)
	}
}

func TestHandler_OriginUnconfigured_NoCheck(t *testing.T) {
	// FR6: with no allowed_origins, the check doesn't run — even if
	// both Origin and Referer are missing, request passes.
	rt := &recordingTransport{}
	h := handlerWithSpam(t, rt, "", nil, "")

	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader("x=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (no allow-list, fail-open)", rec.Code)
	}
}

func TestHandler_BodySize_Exceeded_413(t *testing.T) {
	rt := &recordingTransport{}
	h := handlerWithSpam(t, rt, "", nil, "100B")

	// Build a body larger than 100 bytes
	body := strings.Repeat("x=", 50) + "a=" + strings.Repeat("y", 200) // ~250 bytes
	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}

func TestHandler_BodySize_Within_OK(t *testing.T) {
	rt := &recordingTransport{}
	h := handlerWithSpam(t, rt, "", nil, "1KB")

	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader("name=craig"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandler_BodySize_Unset_NoLimit(t *testing.T) {
	// Empty MaxBodySize means no limit — large body still passes.
	rt := &recordingTransport{}
	h := handlerWithSpam(t, rt, "", nil, "")

	body := strings.Repeat("x=", 5000) + "y=z" // ~10K bytes
	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (no limit)", rec.Code)
	}
}

// --- Rate limiting (Story 3.2 wiring) ---

// makeDuration creates a config.Duration via JSON-equivalent path so the
// test reads naturally. Avoids importing internal Duration helpers.
func makeDuration(d time.Duration) config.Duration { return config.Duration(d) }

func TestHandler_RateLimit_BurstThenDenied(t *testing.T) {
	rt := &recordingTransport{}
	cfg := config.EndpointConfig{
		To:      []string{"to@example.com"},
		From:    "from@example.com",
		Subject: "S",
		Body:    "B",
		RateLimit: &config.RateLimitConfig{
			Count:    2,
			Interval: makeDuration(time.Minute),
		},
	}
	h, err := gateway.New(cfg, rt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// First 2 requests pass.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader("x=1"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "203.0.113.5:12345"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("request %d: status = %d, want 200", i+1, rec.Code)
		}
	}

	// 3rd request from same IP exceeds burst.
	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader("x=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "203.0.113.5:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", rec.Code)
	}
}

func TestHandler_RateLimit_DifferentIPsIndependent(t *testing.T) {
	rt := &recordingTransport{}
	cfg := config.EndpointConfig{
		To:      []string{"to@example.com"},
		From:    "from@example.com",
		Subject: "S",
		Body:    "B",
		RateLimit: &config.RateLimitConfig{
			Count:    1,
			Interval: makeDuration(time.Minute),
		},
	}
	h, _ := gateway.New(cfg, rt)

	// IP 1 exhausts its budget.
	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader("x=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "1.1.1.1:12345"
	h.ServeHTTP(httptest.NewRecorder(), req)

	// IP 2 has its own budget.
	req2 := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader("x=1"))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.RemoteAddr = "2.2.2.2:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req2)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (different IP, own budget)", rec.Code)
	}
}

func TestHandler_RateLimit_TrustedProxy(t *testing.T) {
	// Request comes via 10.0.0.1 (trusted) with X-F-F naming the real
	// client. Rate limit should key on the X-F-F IP, not the proxy.
	rt := &recordingTransport{}
	cfg := config.EndpointConfig{
		To:      []string{"to@example.com"},
		From:    "from@example.com",
		Subject: "S",
		Body:    "B",
		RateLimit: &config.RateLimitConfig{
			Count:    1,
			Interval: makeDuration(time.Minute),
		},
		TrustedProxies: []string{"10.0.0.0/8"},
	}
	h, _ := gateway.New(cfg, rt)

	// Real client A through proxy: budget exhausted.
	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader("x=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-For", "203.0.113.5")
	req.RemoteAddr = "10.0.0.1:55555"
	h.ServeHTTP(httptest.NewRecorder(), req)

	// Real client B through SAME proxy: should still pass — keyed on real IP.
	req2 := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader("x=1"))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.Header.Set("X-Forwarded-For", "198.51.100.1")
	req2.RemoteAddr = "10.0.0.1:55556" // same proxy, different downstream
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req2)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; rate limit should key on X-F-F when proxy is trusted", rec.Code)
	}
}
