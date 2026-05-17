package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/craigmccaskill/posthorn/config"
	"github.com/craigmccaskill/posthorn/csrf"
	"github.com/craigmccaskill/posthorn/gateway"
	"github.com/craigmccaskill/posthorn/transport"
)

// recordingTransport captures everything passed to Send for assertion.
type recordingTransport struct {
	sent       []transport.Message
	sendErr    error
	sendResult transport.SendResult
}

func (r *recordingTransport) Send(_ context.Context, msg transport.Message) (transport.SendResult, error) {
	r.sent = append(r.sent, msg)
	return r.sendResult, r.sendErr
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

// TestHandler_HoneypotBodyMatchesSuccessShape pins the NFR5 / FR5
// invariant: a bot inspecting the 200 response body must not be able
// to tell a honeypot rejection apart from a real successful send.
// Both paths return the same JSON shape ({status, submission_id});
// only the submission_id value differs between requests.
func TestHandler_HoneypotBodyMatchesSuccessShape(t *testing.T) {
	rt := &recordingTransport{}
	h := handlerWithSpam(t, rt, "_gotcha", nil, "")

	// Real success.
	recOK := httptest.NewRecorder()
	h.ServeHTTP(recOK, urlencodedRequest("name=craig"))
	if recOK.Code != http.StatusOK {
		t.Fatalf("real-success status = %d", recOK.Code)
	}

	// Honeypot reject.
	recHP := httptest.NewRecorder()
	h.ServeHTTP(recHP, urlencodedRequest("name=craig&_gotcha=bot"))
	if recHP.Code != http.StatusOK {
		t.Fatalf("honeypot status = %d", recHP.Code)
	}

	var okBody, hpBody map[string]any
	if err := json.Unmarshal(recOK.Body.Bytes(), &okBody); err != nil {
		t.Fatalf("real-success body not JSON: %v (body=%q)", err, recOK.Body.String())
	}
	if err := json.Unmarshal(recHP.Body.Bytes(), &hpBody); err != nil {
		t.Fatalf("honeypot body not JSON: %v (body=%q)", err, recHP.Body.String())
	}

	// Same set of keys.
	if len(okBody) != len(hpBody) {
		t.Fatalf("body key count differs: ok=%d hp=%d (ok=%v hp=%v)",
			len(okBody), len(hpBody), okBody, hpBody)
	}
	for k := range okBody {
		if _, ok := hpBody[k]; !ok {
			t.Errorf("key %q present in real-success body but not honeypot body", k)
		}
	}

	// Same status value.
	if okBody["status"] != "ok" {
		t.Errorf("real-success status = %v, want ok", okBody["status"])
	}
	if hpBody["status"] != "ok" {
		t.Errorf("honeypot status = %v, want ok", hpBody["status"])
	}

	// Both have a non-empty submission_id, and they differ (fresh per
	// request — so a replay attacker can't fingerprint by ID either).
	okID, _ := okBody["submission_id"].(string)
	hpID, _ := hpBody["submission_id"].(string)
	if okID == "" {
		t.Error("real-success body missing non-empty submission_id")
	}
	if hpID == "" {
		t.Error("honeypot body missing non-empty submission_id")
	}
	if okID == hpID {
		t.Errorf("submission_id reused across requests: %q", okID)
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

// --- Retry policy (Story 4.1) ---

// scriptedTransport returns a sequence of pre-set errors.
type scriptedTransport struct {
	calls    int
	results  []error // index = call number (0-based); past end = nil
}

func (s *scriptedTransport) Send(_ context.Context, _ transport.Message) (transport.SendResult, error) {
	defer func() { s.calls++ }()
	if s.calls < len(s.results) {
		return transport.SendResult{}, s.results[s.calls]
	}
	return transport.SendResult{}, nil
}

func TestHandler_TransientError_RetriesOnce(t *testing.T) {
	restore := gateway.SetRetryDelaysForTest(1*time.Millisecond, 1*time.Millisecond, 10*time.Second)
	t.Cleanup(restore)

	st := &scriptedTransport{
		results: []error{
			&transport.TransportError{Class: transport.ErrTransient, Status: 502},
			// second call: nil → success
		},
	}
	cfg := config.EndpointConfig{
		To:      []string{"to@example.com"},
		From:    "from@example.com",
		Subject: "S",
		Body:    "B",
	}
	h, err := gateway.New(cfg, st)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("x=1"))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (second attempt succeeds)", rec.Code)
	}
	if st.calls != 2 {
		t.Errorf("transport calls = %d, want 2 (one initial + one retry)", st.calls)
	}
}

func TestHandler_RateLimitedError_RetriesOnce(t *testing.T) {
	restore := gateway.SetRetryDelaysForTest(1*time.Millisecond, 1*time.Millisecond, 10*time.Second)
	t.Cleanup(restore)

	st := &scriptedTransport{
		results: []error{
			&transport.TransportError{Class: transport.ErrRateLimited, Status: 429},
			// second call: nil → success
		},
	}
	cfg := config.EndpointConfig{
		To:      []string{"to@example.com"},
		From:    "from@example.com",
		Subject: "S",
		Body:    "B",
	}
	h, _ := gateway.New(cfg, st)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("x=1"))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if st.calls != 2 {
		t.Errorf("calls = %d, want 2", st.calls)
	}
}

func TestHandler_TerminalError_NoRetry(t *testing.T) {
	st := &scriptedTransport{
		results: []error{
			&transport.TransportError{Class: transport.ErrTerminal, Status: 401},
			// even if there's a second result, it shouldn't be reached
			nil,
		},
	}
	cfg := config.EndpointConfig{
		To:      []string{"to@example.com"},
		From:    "from@example.com",
		Subject: "S",
		Body:    "B",
	}
	h, _ := gateway.New(cfg, st)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("x=1"))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	if st.calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry on terminal)", st.calls)
	}
}

func TestHandler_BothAttemptsFail_502(t *testing.T) {
	restore := gateway.SetRetryDelaysForTest(1*time.Millisecond, 1*time.Millisecond, 10*time.Second)
	t.Cleanup(restore)

	st := &scriptedTransport{
		results: []error{
			&transport.TransportError{Class: transport.ErrTransient, Status: 502},
			&transport.TransportError{Class: transport.ErrTransient, Status: 502},
		},
	}
	cfg := config.EndpointConfig{
		To:      []string{"to@example.com"},
		From:    "from@example.com",
		Subject: "S",
		Body:    "B",
	}
	h, _ := gateway.New(cfg, st)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("x=1"))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	if st.calls != 2 {
		t.Errorf("calls = %d, want 2 (initial + retry, both fail)", st.calls)
	}
}

func TestHandler_RequestTimeout_HardCutoff(t *testing.T) {
	// Set request timeout to 50ms and retry delay to 200ms. The retry
	// should be skipped because the timeout fires first.
	restore := gateway.SetRetryDelaysForTest(200*time.Millisecond, 5*time.Second, 50*time.Millisecond)
	t.Cleanup(restore)

	st := &scriptedTransport{
		results: []error{
			&transport.TransportError{Class: transport.ErrTransient, Status: 502},
			nil, // would succeed if reached
		},
	}
	cfg := config.EndpointConfig{
		To:      []string{"to@example.com"},
		From:    "from@example.com",
		Subject: "S",
		Body:    "B",
	}
	h, _ := gateway.New(cfg, st)

	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("x=1"))
	elapsed := time.Since(start)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	if st.calls != 1 {
		t.Errorf("calls = %d, want 1 (retry skipped due to timeout)", st.calls)
	}
	if elapsed > 150*time.Millisecond {
		t.Errorf("elapsed %v; should have terminated at the timeout", elapsed)
	}
}

// --- Structured logging (Story 4.2) ---

func TestHandler_LogsSubmissionID(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	rt := &recordingTransport{}
	cfg := config.EndpointConfig{
		Path:    "/api/contact",
		To:      []string{"to@example.com"},
		From:    "from@example.com",
		Subject: "S",
		Body:    "B",
	}
	h, err := gateway.New(cfg, rt, gateway.WithLogger(logger))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	h.ServeHTTP(httptest.NewRecorder(), urlencodedRequest("x=1"))

	logs := logBuf.String()
	if !strings.Contains(logs, "submission_received") {
		t.Errorf("missing submission_received: %s", logs)
	}
	if !strings.Contains(logs, "submission_sent") {
		t.Errorf("missing submission_sent: %s", logs)
	}
	if !strings.Contains(logs, "submission_id") {
		t.Errorf("missing submission_id: %s", logs)
	}
	if !strings.Contains(logs, "/api/contact") {
		t.Errorf("missing endpoint path: %s", logs)
	}
}

// TestHandler_LogsTransportMessageID asserts the submission_sent log carries
// the upstream provider's message ID when the transport returns one. The
// field is the bridge an operator follows from posthorn logs to the
// provider's UI when triaging a missing email.
func TestHandler_LogsTransportMessageID(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	rt := &recordingTransport{sendResult: transport.SendResult{MessageID: "pm-msg-abc-123"}}
	cfg := config.EndpointConfig{
		Path:    "/api/contact",
		To:      []string{"to@example.com"},
		From:    "from@example.com",
		Subject: "S",
		Body:    "B",
	}
	h, err := gateway.New(cfg, rt, gateway.WithLogger(logger))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	h.ServeHTTP(httptest.NewRecorder(), urlencodedRequest("x=1"))

	// Find the submission_sent line and assert it carries transport_message_id.
	var sent map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logBuf.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["msg"] == "submission_sent" {
			sent = rec
			break
		}
	}
	if sent == nil {
		t.Fatalf("submission_sent line not found in logs:\n%s", logBuf.String())
	}
	if got := sent["transport_message_id"]; got != "pm-msg-abc-123" {
		t.Errorf("transport_message_id = %v, want %q", got, "pm-msg-abc-123")
	}
}

// TestHandler_OmitsEmptyTransportMessageID asserts that when the transport
// returns no MessageID (e.g., parse failure or transport without the feature),
// the submission_sent log omits the field rather than emitting an empty
// string — log scrapers expect either a useful ID or nothing.
func TestHandler_OmitsEmptyTransportMessageID(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	rt := &recordingTransport{} // sendResult zero → MessageID == ""
	cfg := config.EndpointConfig{
		Path:    "/api/contact",
		To:      []string{"to@example.com"},
		From:    "from@example.com",
		Subject: "S",
		Body:    "B",
	}
	h, err := gateway.New(cfg, rt, gateway.WithLogger(logger))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	h.ServeHTTP(httptest.NewRecorder(), urlencodedRequest("x=1"))

	for _, line := range strings.Split(strings.TrimSpace(logBuf.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["msg"] != "submission_sent" {
			continue
		}
		if _, present := rec["transport_message_id"]; present {
			t.Errorf("submission_sent has transport_message_id when transport returned none: %v", rec)
		}
	}
}

func TestHandler_LogsTerminalFailure_WithPayload(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	st := &scriptedTransport{
		results: []error{
			&transport.TransportError{Class: transport.ErrTerminal, Status: 401, Message: "unauthorized"},
		},
	}
	cfg := config.EndpointConfig{
		To:      []string{"to@example.com"},
		From:    "from@example.com",
		Subject: "S",
		Body:    "B",
		// LogFailedSubmissions left nil → defaults to true
	}
	h, _ := gateway.New(cfg, st, gateway.WithLogger(logger))

	h.ServeHTTP(httptest.NewRecorder(), urlencodedRequest("name=craig&message=hello"))

	logs := logBuf.String()
	if !strings.Contains(logs, "submission_failed") {
		t.Errorf("missing submission_failed: %s", logs)
	}
	if !strings.Contains(logs, "craig") {
		t.Errorf("payload should be in failure log when log_failed_submissions=true: %s", logs)
	}
}

func TestHandler_LogsTerminalFailure_RedactedPayload(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	st := &scriptedTransport{
		results: []error{
			&transport.TransportError{Class: transport.ErrTerminal, Status: 401},
		},
	}
	disabled := false
	cfg := config.EndpointConfig{
		To:                   []string{"to@example.com"},
		From:                 "from@example.com",
		Subject:              "S",
		Body:                 "B",
		LogFailedSubmissions: &disabled,
	}
	h, _ := gateway.New(cfg, st, gateway.WithLogger(logger))

	h.ServeHTTP(httptest.NewRecorder(), urlencodedRequest("name=craig&message=secret"))

	logs := logBuf.String()
	if !strings.Contains(logs, "submission_failed") {
		t.Errorf("missing submission_failed: %s", logs)
	}
	if strings.Contains(logs, "secret") {
		t.Errorf("payload value 'secret' should NOT appear when log_failed_submissions=false: %s", logs)
	}
	if !strings.Contains(logs, "form_fields") {
		t.Errorf("expected form_fields metadata when payload disabled: %s", logs)
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

// --- Reply-To handling (PRD Open Question 4 decision, Story 6+ patch) ---

// TestHandler_ReplyTo_DefaultsToEmailField verifies the default behavior:
// when ReplyToEmailField is unset, the resolved email field is used. A
// valid email value populates msg.ReplyTo; the receiver hits "Reply" and
// reaches the submitter, not the operator's "from" address.
func TestHandler_ReplyTo_DefaultsToEmailField(t *testing.T) {
	rt := &recordingTransport{}
	cfg := config.EndpointConfig{
		Path:    "/test",
		To:      []string{"to@example.com"},
		From:    "from@example.com",
		Subject: "S",
		Body:    "B",
	}
	h, err := gateway.New(cfg, rt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	h.ServeHTTP(httptest.NewRecorder(), urlencodedRequest("email=user@example.com&message=hi"))

	if len(rt.sent) != 1 {
		t.Fatalf("expected one Send, got %d", len(rt.sent))
	}
	if got := rt.sent[0].ReplyTo; got != "user@example.com" {
		t.Errorf("ReplyTo = %q, want user@example.com", got)
	}
}

// TestHandler_ReplyTo_HonorsExplicitField verifies that an explicit
// ReplyToEmailField points at a non-default form field.
func TestHandler_ReplyTo_HonorsExplicitField(t *testing.T) {
	rt := &recordingTransport{}
	cfg := config.EndpointConfig{
		Path:              "/test",
		To:                []string{"to@example.com"},
		From:              "from@example.com",
		Subject:           "S",
		Body:              "B",
		ReplyToEmailField: "contact_address",
	}
	h, err := gateway.New(cfg, rt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	h.ServeHTTP(httptest.NewRecorder(), urlencodedRequest("contact_address=alice@example.com&message=hi"))

	if got := rt.sent[0].ReplyTo; got != "alice@example.com" {
		t.Errorf("ReplyTo = %q, want alice@example.com", got)
	}
}

// TestHandler_ReplyTo_SkipsInvalidEmail verifies that an invalid email
// in the configured field does NOT become a Reply-To — that would let
// a malformed value (CRLF, bare string) reach the transport's header
// path. Empty Reply-To means receivers reply to the "from" address.
func TestHandler_ReplyTo_SkipsInvalidEmail(t *testing.T) {
	rt := &recordingTransport{}
	// Use a separate email_field so the main email validator passes
	// (it'd reject the request before send otherwise).
	cfg := config.EndpointConfig{
		Path:              "/test",
		To:                []string{"to@example.com"},
		From:              "from@example.com",
		Subject:           "S",
		Body:              "B",
		EmailField:        "main_email",
		ReplyToEmailField: "reply_field",
	}
	h, err := gateway.New(cfg, rt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	h.ServeHTTP(httptest.NewRecorder(), urlencodedRequest("main_email=ok@example.com&reply_field=not-an-email"))

	if got := rt.sent[0].ReplyTo; got != "" {
		t.Errorf("ReplyTo = %q, want \"\" (invalid email should not set Reply-To)", got)
	}
}

// TestHandler_ReplyTo_SkipsEmptyField verifies that a missing form
// field (no value supplied) does NOT set Reply-To.
func TestHandler_ReplyTo_SkipsEmptyField(t *testing.T) {
	rt := &recordingTransport{}
	cfg := config.EndpointConfig{
		Path:              "/test",
		To:                []string{"to@example.com"},
		From:              "from@example.com",
		Subject:           "S",
		Body:              "B",
		ReplyToEmailField: "missing_field",
	}
	h, err := gateway.New(cfg, rt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	h.ServeHTTP(httptest.NewRecorder(), urlencodedRequest("message=hi"))

	if got := rt.sent[0].ReplyTo; got != "" {
		t.Errorf("ReplyTo = %q, want \"\"", got)
	}
}

// --- v1.1: API-key auth (Story 8.2) ---

// apiModeConfig returns a minimal API-mode endpoint config. The default
// honeypot/origin/redirect fields are omitted (they would fail config
// validation; the gateway tests don't go through config.Validate but the
// handler logic still treats them as inapplicable in api mode).
func apiModeConfig(apiKeys ...string) config.EndpointConfig {
	if len(apiKeys) == 0 {
		apiKeys = []string{"valid-key"}
	}
	return config.EndpointConfig{
		Path:    "/api/transactional",
		To:      []string{"to@example.com"},
		From:    "from@example.com",
		Subject: "S",
		Body:    "B",
		Auth:    config.AuthAPIKey,
		APIKeys: apiKeys,
	}
}

// apiRequest builds a POST request with a JSON body and an
// Authorization: Bearer header (api-mode shape; FR36, FR37).
func apiRequest(jsonBody, bearerToken string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/transactional", strings.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	return req
}

func TestAPIAuth_ValidKey_Success(t *testing.T) {
	rt := &recordingTransport{}
	h, err := gateway.New(apiModeConfig("valid-key"), rt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequest(`{"message":"hi"}`, "valid-key"))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(rt.sent) != 1 {
		t.Errorf("transport.Send not called once: sent=%d", len(rt.sent))
	}
}

func TestAPIAuth_MissingHeader_401(t *testing.T) {
	h, err := gateway.New(apiModeConfig(), &recordingTransport{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequest(`{"message":"hi"}`, ""))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestAPIAuth_WrongScheme_401(t *testing.T) {
	h, _ := gateway.New(apiModeConfig("valid-key"), &recordingTransport{})
	req := apiRequest(`{"message":"hi"}`, "")
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestAPIAuth_WrongToken_401(t *testing.T) {
	h, _ := gateway.New(apiModeConfig("valid-key"), &recordingTransport{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequest(`{"message":"hi"}`, "wrong-key"))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestAPIAuth_EmptyToken_401(t *testing.T) {
	h, _ := gateway.New(apiModeConfig("valid-key"), &recordingTransport{})
	req := apiRequest(`{"message":"hi"}`, "")
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestAPIAuth_MultipleKeys_SecondMatches(t *testing.T) {
	rt := &recordingTransport{}
	h, _ := gateway.New(apiModeConfig("key-one", "key-two", "key-three"), rt)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequest(`{"message":"hi"}`, "key-two"))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(rt.sent) != 1 {
		t.Errorf("transport.Send not called: sent=%d", len(rt.sent))
	}
}

func TestAPIAuth_CaseInsensitiveScheme(t *testing.T) {
	rt := &recordingTransport{}
	h, _ := gateway.New(apiModeConfig("valid-key"), rt)
	for _, scheme := range []string{"Bearer", "bearer", "BEARER", "BeArEr"} {
		t.Run(scheme, func(t *testing.T) {
			rt.sent = nil
			req := apiRequest(`{"message":"hi"}`, "")
			req.Header.Set("Authorization", scheme+" valid-key")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("scheme=%q: status = %d, want 200", scheme, rec.Code)
			}
		})
	}
}

// TestAPIAuth_PerKeyRateLimit asserts FR35 — API-mode endpoints rate-limit
// per matched API key, not per client IP. Two callers with distinct keys
// hitting the same endpoint must have independent buckets.
func TestAPIAuth_PerKeyRateLimit(t *testing.T) {
	rt := &recordingTransport{}
	cfg := apiModeConfig("key-A", "key-B")
	cfg.RateLimit = &config.RateLimitConfig{Count: 1, Interval: config.Duration(time.Minute)}
	h, err := gateway.New(cfg, rt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// key-A's first request: 200.
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, apiRequest(`{"message":"hi"}`, "key-A"))
	if rec1.Code != http.StatusOK {
		t.Fatalf("key-A first request: status = %d, want 200; body=%s", rec1.Code, rec1.Body.String())
	}

	// key-A's second request: 429 (bucket drained).
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, apiRequest(`{"message":"hi"}`, "key-A"))
	if rec2.Code != http.StatusTooManyRequests {
		t.Errorf("key-A second request: status = %d, want 429", rec2.Code)
	}

	// key-B's first request: 200 (independent bucket).
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, apiRequest(`{"message":"hi"}`, "key-B"))
	if rec3.Code != http.StatusOK {
		t.Errorf("key-B first request: status = %d, want 200 (independent bucket); body=%s", rec3.Code, rec3.Body.String())
	}
}

// TestAPIAuth_NoOriginCheck asserts that API-mode endpoints don't enforce
// Origin/Referer even if some AllowedOrigins value sneaks into the config
// (config.Validate rejects this combination, but the handler should also
// be defensive — if someone constructs an api-mode handler directly with
// AllowedOrigins set, the check must not run).
func TestAPIAuth_NoOriginCheck(t *testing.T) {
	rt := &recordingTransport{}
	cfg := apiModeConfig("valid-key")
	cfg.AllowedOrigins = []string{"https://only-this-site.com"} // intentionally set despite api mode
	h, err := gateway.New(cfg, rt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := apiRequest(`{"message":"hi"}`, "valid-key")
	req.Header.Set("Origin", "https://different-site.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (api mode ignores Origin even when AllowedOrigins is set)", rec.Code)
	}
}

// TestAPIAuth_NoHoneypotCheck asserts that the honeypot field is not
// triggered on api-mode endpoints (FR5 is form-mode only). Same defensive
// posture as TestAPIAuth_NoOriginCheck.
func TestAPIAuth_NoHoneypotCheck(t *testing.T) {
	rt := &recordingTransport{}
	cfg := apiModeConfig("valid-key")
	cfg.Honeypot = "_gotcha"
	h, err := gateway.New(cfg, rt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := httptest.NewRecorder()
	// Body fills the honeypot field — in form mode this would silent-200
	// without calling transport.Send. In api mode it must call transport.Send.
	h.ServeHTTP(rec, apiRequest(`{"message":"hi","_gotcha":"trap"}`, "valid-key"))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if len(rt.sent) != 1 {
		t.Errorf("transport.Send not called: sent=%d (honeypot must not block api-mode)", len(rt.sent))
	}
}

// TestAPIAuth_FailedAuthDoesNotLogKey is the NFR21 test for the failed-auth
// path. The sentinel key value must not appear anywhere in the captured
// log output, even when the request is rejected.
func TestAPIAuth_FailedAuthDoesNotLogKey(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	const sentinel = "super-secret-token-do-not-leak-via-logs"
	h, err := gateway.New(apiModeConfig("the-real-key"), &recordingTransport{}, gateway.WithLogger(logger))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequest(`{"message":"hi"}`, sentinel))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if strings.Contains(logBuf.String(), sentinel) {
		t.Errorf("sentinel api-key %q appeared in logs: %s", sentinel, logBuf.String())
	}
	// Sanity: at least an auth_failed line should be present.
	if !strings.Contains(logBuf.String(), "auth_failed") {
		t.Errorf("expected auth_failed log line; got: %s", logBuf.String())
	}
}

// TestAPIAuth_SuccessfulAuthDoesNotLogKey asserts NFR21 on the happy path —
// even after a 200 success, the matched api-key value must not appear in
// any captured log line (including rate-limit logs).
func TestAPIAuth_SuccessfulAuthDoesNotLogKey(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	const sentinel = "sentinel-success-key-shhh"
	cfg := apiModeConfig(sentinel)
	cfg.RateLimit = &config.RateLimitConfig{Count: 1, Interval: config.Duration(time.Minute)}
	h, err := gateway.New(cfg, &recordingTransport{}, gateway.WithLogger(logger))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// First request: 200. Second: 429 (rate limited). Both must keep the
	// key out of logs.
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, apiRequest(`{"message":"hi"}`, sentinel))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, apiRequest(`{"message":"hi"}`, sentinel))

	if strings.Contains(logBuf.String(), sentinel) {
		t.Errorf("sentinel api-key %q appeared in logs across success+rate-limit: %s", sentinel, logBuf.String())
	}
}

// --- v1.1: JSON ingress (Story 8.3) ---

// TestAPIJSON_FormEncodedBody_415 pins FR37: form-encoded bodies on
// api-mode endpoints get 415, not silently accepted.
func TestAPIJSON_FormEncodedBody_415(t *testing.T) {
	h, _ := gateway.New(apiModeConfig("valid-key"), &recordingTransport{})
	req := httptest.NewRequest(http.MethodPost, "/api/transactional", strings.NewReader("message=hi"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer valid-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", rec.Code)
	}
}

func TestAPIJSON_PlainTextBody_415(t *testing.T) {
	h, _ := gateway.New(apiModeConfig("valid-key"), &recordingTransport{})
	req := httptest.NewRequest(http.MethodPost, "/api/transactional", strings.NewReader("hello"))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Authorization", "Bearer valid-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", rec.Code)
	}
}

func TestAPIJSON_MissingContentType_415(t *testing.T) {
	h, _ := gateway.New(apiModeConfig("valid-key"), &recordingTransport{})
	req := httptest.NewRequest(http.MethodPost, "/api/transactional", strings.NewReader(`{"message":"hi"}`))
	// no Content-Type
	req.Header.Set("Authorization", "Bearer valid-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", rec.Code)
	}
}

func TestAPIJSON_ContentTypeWithCharset_OK(t *testing.T) {
	h, _ := gateway.New(apiModeConfig("valid-key"), &recordingTransport{})
	req := httptest.NewRequest(http.MethodPost, "/api/transactional", strings.NewReader(`{"message":"hi"}`))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer valid-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAPIJSON_MalformedJSON_400(t *testing.T) {
	h, _ := gateway.New(apiModeConfig("valid-key"), &recordingTransport{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequest(`{"message": broken`, "valid-key"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestAPIJSON_RequiredField_422 pins FR38: existing required-field validation
// applies identically to api-mode JSON submissions.
func TestAPIJSON_RequiredField_422(t *testing.T) {
	cfg := apiModeConfig("valid-key")
	cfg.Required = []string{"name", "email"}
	h, _ := gateway.New(cfg, &recordingTransport{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequest(`{"name":"Alice"}`, "valid-key")) // email missing
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

// TestAPIJSON_EmailValidation_422 pins FR38 for the email-format check.
func TestAPIJSON_EmailValidation_422(t *testing.T) {
	cfg := apiModeConfig("valid-key")
	cfg.Required = []string{"email"}
	h, _ := gateway.New(cfg, &recordingTransport{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequest(`{"email":"not-an-email"}`, "valid-key"))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}
}

// TestAPIJSON_CustomFieldsPassthrough pins FR39 — keys not named in the
// template render in the "Additional fields" block, same as form mode.
func TestAPIJSON_CustomFieldsPassthrough(t *testing.T) {
	rt := &recordingTransport{}
	cfg := apiModeConfig("valid-key")
	cfg.Required = []string{"name"}
	cfg.Subject = "Hi {{.name}}"
	cfg.Body = "Name: {{.name}}\nMessage: {{.message}}"
	h, _ := gateway.New(cfg, rt)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequest(`{"name":"Alice","message":"hello","extra1":"x1","extra2":"x2"}`, "valid-key"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(rt.sent) != 1 {
		t.Fatalf("transport.Send not called: sent=%d", len(rt.sent))
	}
	body := rt.sent[0].BodyText
	if !strings.Contains(body, "extra1: x1") || !strings.Contains(body, "extra2: x2") {
		t.Errorf("custom-fields passthrough missing extra1/extra2 in body: %s", body)
	}
}

// TestAPIJSON_TypeCoercion pins the architecture doc Open Q5 decision —
// primitive types coerce to strings, integers come out without a decimal,
// booleans render as "true"/"false".
func TestAPIJSON_TypeCoercion(t *testing.T) {
	rt := &recordingTransport{}
	cfg := apiModeConfig("valid-key")
	cfg.Required = []string{"count"}
	cfg.Subject = "Order #{{.count}}"
	cfg.Body = "Count={{.count}} Confirmed={{.confirmed}} Price={{.price}}"
	h, _ := gateway.New(cfg, rt)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequest(`{"count":42,"confirmed":true,"price":19.99}`, "valid-key"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rt.sent[0].Subject; got != "Order #42" {
		t.Errorf("Subject = %q, want %q (integer must render without decimal)", got, "Order #42")
	}
	if got := rt.sent[0].BodyText; !strings.Contains(got, "Count=42") {
		t.Errorf("Body missing Count=42: %s", got)
	}
	if got := rt.sent[0].BodyText; !strings.Contains(got, "Confirmed=true") {
		t.Errorf("Body missing Confirmed=true: %s", got)
	}
	if got := rt.sent[0].BodyText; !strings.Contains(got, "Price=19.99") {
		t.Errorf("Body missing Price=19.99: %s", got)
	}
}

func TestAPIJSON_NullValueOmitted(t *testing.T) {
	cfg := apiModeConfig("valid-key")
	cfg.Required = []string{"name"}
	h, _ := gateway.New(cfg, &recordingTransport{})
	rec := httptest.NewRecorder()
	// `name` is null → treated as absent → required-field failure → 422.
	h.ServeHTTP(rec, apiRequest(`{"name":null}`, "valid-key"))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 (null treated as absent)", rec.Code)
	}
}

func TestAPIJSON_ArrayPrimitives_Multivalue(t *testing.T) {
	rt := &recordingTransport{}
	cfg := apiModeConfig("valid-key")
	cfg.Required = []string{"tags"}
	cfg.Subject = "Tags"
	cfg.Body = "Tags={{.tags}}"
	h, _ := gateway.New(cfg, rt)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequest(`{"tags":["urgent","support"]}`, "valid-key"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAPIJSON_NestedObject_400(t *testing.T) {
	h, _ := gateway.New(apiModeConfig("valid-key"), &recordingTransport{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequest(`{"user":{"name":"alice"}}`, "valid-key"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (nested objects not supported)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "nested objects") {
		t.Errorf("error body should mention nested objects: %s", rec.Body.String())
	}
}

func TestAPIJSON_TopLevelArray_400(t *testing.T) {
	h, _ := gateway.New(apiModeConfig("valid-key"), &recordingTransport{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequest(`[1,2,3]`, "valid-key"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (top-level must be object)", rec.Code)
	}
}

func TestAPIJSON_ArrayContainingObject_400(t *testing.T) {
	h, _ := gateway.New(apiModeConfig("valid-key"), &recordingTransport{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequest(`{"items":[{"a":1}]}`, "valid-key"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (array of objects not supported)", rec.Code)
	}
}

func TestAPIJSON_TrailingContent_400(t *testing.T) {
	h, _ := gateway.New(apiModeConfig("valid-key"), &recordingTransport{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequest(`{"a":1}{"b":2}`, "valid-key"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (trailing JSON not allowed)", rec.Code)
	}
}

// --- v1.1: Idempotency cache (Story 8.4) ---

// apiRequestWithIdemKey builds an api-mode request carrying an
// Idempotency-Key header.
func apiRequestWithIdemKey(jsonBody, bearerToken, idemKey string) *http.Request {
	req := apiRequest(jsonBody, bearerToken)
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	return req
}

// TestIdempotency_FirstRequest_NormalFlow asserts an idempotency-keyed
// request runs through the pipeline normally on first sight — no cache
// hit on a fresh cache.
func TestIdempotency_FirstRequest_NormalFlow(t *testing.T) {
	rt := &recordingTransport{sendResult: transport.SendResult{MessageID: "pm-1"}}
	h, _ := gateway.New(apiModeConfig("valid-key"), rt)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequestWithIdemKey(`{"message":"hi"}`, "valid-key", "idem-abc-123"))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(rt.sent) != 1 {
		t.Errorf("transport.Send not called: sent=%d", len(rt.sent))
	}
}

// TestIdempotency_ReplayByteIdentical pins NFR20: a second request with
// the same key replays the cached response byte-for-byte.
func TestIdempotency_ReplayByteIdentical(t *testing.T) {
	rt := &recordingTransport{sendResult: transport.SendResult{MessageID: "pm-msg-replay-test"}}
	h, _ := gateway.New(apiModeConfig("valid-key"), rt)

	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, apiRequestWithIdemKey(`{"message":"hi"}`, "valid-key", "key-X"))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request: status = %d", rec1.Code)
	}
	firstBody := rec1.Body.String()
	firstContentType := rec1.Header().Get("Content-Type")

	// Second request: same key, transport must NOT be called again.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, apiRequestWithIdemKey(`{"message":"hi"}`, "valid-key", "key-X"))

	if rec2.Code != rec1.Code {
		t.Errorf("replay status = %d, want %d", rec2.Code, rec1.Code)
	}
	if rec2.Body.String() != firstBody {
		t.Errorf("replay body differs:\n  got  %q\n  want %q", rec2.Body.String(), firstBody)
	}
	if rec2.Header().Get("Content-Type") != firstContentType {
		t.Errorf("replay Content-Type = %q, want %q", rec2.Header().Get("Content-Type"), firstContentType)
	}
	if len(rt.sent) != 1 {
		t.Errorf("transport.Send called %d times on replay; want 1 (replay must not re-send)", len(rt.sent))
	}
}

// TestIdempotency_DifferentKeys_NoSharing asserts that distinct keys
// don't collide — each gets its own fresh execution.
func TestIdempotency_DifferentKeys_NoSharing(t *testing.T) {
	rt := &recordingTransport{}
	h, _ := gateway.New(apiModeConfig("valid-key"), rt)

	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, apiRequestWithIdemKey(`{"message":"hi"}`, "valid-key", "key-A"))

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, apiRequestWithIdemKey(`{"message":"hi"}`, "valid-key", "key-B"))

	if len(rt.sent) != 2 {
		t.Errorf("transport.Send called %d times; want 2 (distinct keys = distinct executions)", len(rt.sent))
	}
}

// TestIdempotency_PerEndpointScope pins FR41: the same Idempotency-Key
// hitting two different endpoints does NOT collide. Two endpoints each
// have their own cache instance.
func TestIdempotency_PerEndpointScope(t *testing.T) {
	rt1 := &recordingTransport{}
	rt2 := &recordingTransport{}
	cfg1 := apiModeConfig("valid-key")
	cfg1.Path = "/api/one"
	cfg2 := apiModeConfig("valid-key")
	cfg2.Path = "/api/two"
	h1, _ := gateway.New(cfg1, rt1)
	h2, _ := gateway.New(cfg2, rt2)

	const sharedKey = "same-key"

	// Endpoint 1 receives the key.
	rec1 := httptest.NewRecorder()
	h1.ServeHTTP(rec1, apiRequestWithIdemKey(`{"message":"hi"}`, "valid-key", sharedKey))
	if rec1.Code != http.StatusOK {
		t.Fatalf("endpoint 1 first: status = %d", rec1.Code)
	}

	// Endpoint 2 receives the same key — must NOT replay endpoint 1's
	// response. Each endpoint has its own cache (FR41).
	rec2 := httptest.NewRecorder()
	h2.ServeHTTP(rec2, apiRequestWithIdemKey(`{"message":"hi"}`, "valid-key", sharedKey))
	if rec2.Code != http.StatusOK {
		t.Fatalf("endpoint 2 first: status = %d", rec2.Code)
	}
	if len(rt2.sent) != 1 {
		t.Errorf("endpoint 2 transport.Send count = %d, want 1 (per-endpoint cache must not share)", len(rt2.sent))
	}
}

// Empty Idempotency-Key (literally `Idempotency-Key:` with no value) is
// indistinguishable from the header being absent in Go's net/http — Get
// returns "" in both cases. We treat that as "no idempotency requested,"
// which is a reasonable defensive interpretation since real HTTP clients
// don't emit empty header values. ValidateKey's empty-string rejection
// (covered in core/idempotency/cache_test.go) protects against the
// non-HTTP code path where ValidateKey is invoked directly.

func TestIdempotency_KeyValidation_TooLong(t *testing.T) {
	h, _ := gateway.New(apiModeConfig("valid-key"), &recordingTransport{})
	tooLong := strings.Repeat("x", 256)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequestWithIdemKey(`{"message":"hi"}`, "valid-key", tooLong))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestIdempotency_KeyValidation_NonPrintable(t *testing.T) {
	h, _ := gateway.New(apiModeConfig("valid-key"), &recordingTransport{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequestWithIdemKey(`{"message":"hi"}`, "valid-key", "bad\nkey"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestIdempotency_NoKey_NoCache asserts that absent Idempotency-Key skips
// the cache machinery entirely — normal flow, no replay possible.
func TestIdempotency_NoKey_NoCache(t *testing.T) {
	rt := &recordingTransport{}
	h, _ := gateway.New(apiModeConfig("valid-key"), rt)

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, apiRequest(`{"message":"hi"}`, "valid-key"))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d", i, rec.Code)
		}
	}
	if len(rt.sent) != 3 {
		t.Errorf("transport.Send called %d times; want 3 (no Idempotency-Key = no replay)", len(rt.sent))
	}
}

// TestIdempotency_ValidationError_422_Cached pins that 422 validation
// failures are cacheable — same key, same body → same 422.
func TestIdempotency_ValidationError_Cached(t *testing.T) {
	rt := &recordingTransport{}
	cfg := apiModeConfig("valid-key")
	cfg.Required = []string{"name"}
	h, _ := gateway.New(cfg, rt)

	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, apiRequestWithIdemKey(`{}`, "valid-key", "key-Y"))
	if rec1.Code != http.StatusUnprocessableEntity {
		t.Fatalf("first request: status = %d, want 422", rec1.Code)
	}
	firstBody := rec1.Body.String()

	// Replay: same 422, byte-identical.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, apiRequestWithIdemKey(`{}`, "valid-key", "key-Y"))
	if rec2.Code != http.StatusUnprocessableEntity {
		t.Errorf("replay status = %d, want 422", rec2.Code)
	}
	if rec2.Body.String() != firstBody {
		t.Errorf("replay body differs from cached 422")
	}
}

// TestIdempotency_TransportFailure_NotCached pins that 5xx terminal
// failures are NOT cached — a transient failure shouldn't freeze for 24h.
func TestIdempotency_TransportFailure_NotCached(t *testing.T) {
	terminalErr := &transport.TransportError{Class: transport.ErrTerminal, Status: 401, Message: "bad postmark key"}
	st := &scriptedTransport{
		results: []error{terminalErr, terminalErr, nil}, // two terminal then one success
	}
	h, _ := gateway.New(apiModeConfig("valid-key"), st)

	// First request: 502 (terminal transport failure).
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, apiRequestWithIdemKey(`{"message":"hi"}`, "valid-key", "retry-me"))
	if rec1.Code != http.StatusBadGateway {
		t.Fatalf("first request: status = %d, want 502", rec1.Code)
	}

	// Second request: must NOT be the cached 502; must hit transport again.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, apiRequestWithIdemKey(`{"message":"hi"}`, "valid-key", "retry-me"))
	if rec2.Code != http.StatusBadGateway {
		t.Errorf("second request: status = %d, want 502 (fresh)", rec2.Code)
	}
	if st.calls < 2 {
		t.Errorf("transport.Send called %d times; want at least 2 (5xx must not be cached)", st.calls)
	}
}

// blockingTransport coordinates an in-flight test — Send blocks on
// release until the test signals.
type blockingTransport struct {
	started chan struct{} // closed when Send is entered
	release chan struct{} // close to unblock Send
	result  transport.SendResult
	err     error
}

func (b *blockingTransport) Send(_ context.Context, _ transport.Message) (transport.SendResult, error) {
	close(b.started)
	<-b.release
	return b.result, b.err
}

// TestIdempotency_InFlightCollision_409 pins FR44 — a duplicate key
// arriving while the first request is still in flight gets 409.
func TestIdempotency_InFlightCollision_409(t *testing.T) {
	bt := &blockingTransport{
		started: make(chan struct{}),
		release: make(chan struct{}),
		result:  transport.SendResult{MessageID: "pm-block"},
	}
	h, _ := gateway.New(apiModeConfig("valid-key"), bt)

	const key = "inflight-key"
	done := make(chan struct{})
	rec1 := httptest.NewRecorder()
	go func() {
		defer close(done)
		h.ServeHTTP(rec1, apiRequestWithIdemKey(`{"message":"hi"}`, "valid-key", key))
	}()

	// Wait until request 1 is in flight at the transport.Send call.
	<-bt.started

	// Request 2 with same key should get 409 immediately.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, apiRequestWithIdemKey(`{"message":"hi"}`, "valid-key", key))
	if rec2.Code != http.StatusConflict {
		t.Errorf("in-flight collision status = %d, want 409", rec2.Code)
	}

	// Release request 1; verify it completes successfully.
	close(bt.release)
	<-done
	if rec1.Code != http.StatusOK {
		t.Errorf("request 1 final status = %d, want 200", rec1.Code)
	}
}

// TestIdempotency_FormModeIgnoresHeader asserts that the Idempotency-Key
// header has no effect on form-mode endpoints — they don't have an
// idempotency cache, and the header is silently ignored.
func TestIdempotency_FormModeIgnoresHeader(t *testing.T) {
	rt := &recordingTransport{}
	h := newTestHandler(t, rt)
	req := urlencodedRequest("message=hi")
	req.Header.Set("Idempotency-Key", "should-be-ignored")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if len(rt.sent) != 1 {
		t.Errorf("transport.Send not called: sent=%d", len(rt.sent))
	}
}

// --- v1.1: Per-request to_override (FR46, ADR-11) ---

// TestToOverride_SingleString_AsArray covers the single-string shape
// that parseJSONBody coerces into a one-element slice.
func TestToOverride_SingleString_AsArray(t *testing.T) {
	rt := &recordingTransport{}
	h, _ := gateway.New(apiModeConfig("valid-key"), rt)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequest(`{"message":"hi","to_override":"alice@example.com"}`, "valid-key"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(rt.sent) != 1 {
		t.Fatalf("transport.Send not called: sent=%d", len(rt.sent))
	}
	got := rt.sent[0].To
	if len(got) != 1 || got[0] != "alice@example.com" {
		t.Errorf("To = %v, want [alice@example.com]", got)
	}
}

func TestToOverride_Array_MultipleRecipients(t *testing.T) {
	rt := &recordingTransport{}
	h, _ := gateway.New(apiModeConfig("valid-key"), rt)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequest(`{"message":"hi","to_override":["alice@example.com","bob@example.com"]}`, "valid-key"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := rt.sent[0].To
	if len(got) != 2 || got[0] != "alice@example.com" || got[1] != "bob@example.com" {
		t.Errorf("To = %v, want [alice@example.com bob@example.com]", got)
	}
}

// TestToOverride_Absent_UsesConfigTo asserts FR45 backwards compat — when
// to_override is missing from the JSON body, cfg.To applies.
func TestToOverride_Absent_UsesConfigTo(t *testing.T) {
	rt := &recordingTransport{}
	cfg := apiModeConfig("valid-key")
	cfg.To = []string{"config-recipient@example.com"}
	h, _ := gateway.New(cfg, rt)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequest(`{"message":"hi"}`, "valid-key"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := rt.sent[0].To
	if len(got) != 1 || got[0] != "config-recipient@example.com" {
		t.Errorf("To = %v, want [config-recipient@example.com] (cfg.To fallback)", got)
	}
}

func TestToOverride_EmptyArray_422(t *testing.T) {
	h, _ := gateway.New(apiModeConfig("valid-key"), &recordingTransport{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequest(`{"message":"hi","to_override":[]}`, "valid-key"))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 (empty to_override array)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "to_override") {
		t.Errorf("error body should name to_override: %s", rec.Body.String())
	}
}

func TestToOverride_InvalidEmail_422(t *testing.T) {
	h, _ := gateway.New(apiModeConfig("valid-key"), &recordingTransport{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequest(`{"message":"hi","to_override":"not-an-email"}`, "valid-key"))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "to_override") {
		t.Errorf("error body should name to_override: %s", rec.Body.String())
	}
}

// TestToOverride_OneInvalidInArray_422 asserts that one bad address among
// good ones fails the whole request — partial-success semantics are
// confusing for transactional sends.
func TestToOverride_OneInvalidInArray_422(t *testing.T) {
	h, _ := gateway.New(apiModeConfig("valid-key"), &recordingTransport{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequest(`{"message":"hi","to_override":["alice@example.com","not-an-email","bob@example.com"]}`, "valid-key"))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}
}

// TestToOverride_HeaderInjection asserts that NFR1/NFR2's no-header-
// injection guarantee carries over to to_override-supplied recipients.
// validate.Email already rejects CRLF (it doesn't pass the syntax check),
// so the 422 path catches this. The Postmark transport's struct-based
// JSON encoding is the structural backstop.
func TestToOverride_HeaderInjection_RejectedByValidation(t *testing.T) {
	h, _ := gateway.New(apiModeConfig("valid-key"), &recordingTransport{})
	for _, payload := range []string{
		"alice@example.com\\r\\nBcc: victim@target.com",
		"alice@example.com\nBcc: victim@target.com",
	} {
		t.Run(payload, func(t *testing.T) {
			rec := httptest.NewRecorder()
			body := fmt.Sprintf(`{"message":"hi","to_override":%q}`, payload)
			h.ServeHTTP(rec, apiRequest(body, "valid-key"))
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("status = %d, want 422 (injection payload should fail validation)", rec.Code)
			}
		})
	}
}

// TestToOverride_NotInCustomFieldsBlock asserts that to_override is
// reserved — it must not appear in the rendered body's "Additional
// fields:" passthrough block.
func TestToOverride_NotInCustomFieldsBlock(t *testing.T) {
	rt := &recordingTransport{}
	cfg := apiModeConfig("valid-key")
	cfg.Subject = "S"
	cfg.Body = "Body"
	h, _ := gateway.New(cfg, rt)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequest(`{"message":"hi","to_override":"alice@example.com"}`, "valid-key"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rt.sent[0].BodyText
	if strings.Contains(body, "to_override") {
		t.Errorf("to_override should not appear in body: %s", body)
	}
}

// TestToOverride_FormModeIgnoresField asserts that a `to_override` form
// field on a form-mode endpoint does NOT override recipients — form-mode
// endpoints' recipients are operator-controlled by design (ADR-11).
func TestToOverride_FormModeIgnoresField(t *testing.T) {
	rt := &recordingTransport{}
	cfg := config.EndpointConfig{
		Path:    "/test",
		To:      []string{"config-recipient@example.com"},
		From:    "from@example.com",
		Subject: "S",
		Body:    "B",
	}
	h, err := gateway.New(cfg, rt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("message=hi&to_override=evil@attacker.com"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got := rt.sent[0].To
	if len(got) != 1 || got[0] != "config-recipient@example.com" {
		t.Errorf("To = %v, want [config-recipient@example.com] (form mode must NOT honor to_override)", got)
	}
}

// --- v1.0 block C: Dry-run mode (Story 10.2) ---

func TestDryRun_FormMode_ReturnsPreparedMessage(t *testing.T) {
	rt := &recordingTransport{}
	cfg := config.EndpointConfig{
		Path:     "/test",
		To:       []string{"to@example.com"},
		From:     "from@example.com",
		Subject:  "Subject is {{.topic}}",
		Body:     "Hello {{.name}}",
		Required: []string{"name"},
		DryRun:   true,
	}
	h, err := gateway.New(cfg, rt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("name=Alice&topic=Greetings"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(rt.sent) != 0 {
		t.Errorf("transport.Send was called %d times in dry-run; want 0", len(rt.sent))
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, rec.Body.String())
	}
	if body["status"] != "dry_run" {
		t.Errorf("status = %v, want %q", body["status"], "dry_run")
	}
	if _, ok := body["submission_id"].(string); !ok {
		t.Errorf("submission_id missing or wrong type: %v", body["submission_id"])
	}
	prep, ok := body["prepared_message"].(map[string]any)
	if !ok {
		t.Fatalf("prepared_message missing or wrong type: %v", body["prepared_message"])
	}
	if prep["from"] != "from@example.com" {
		t.Errorf("prepared_message.from = %v", prep["from"])
	}
	if prep["subject"] != "Subject is Greetings" {
		t.Errorf("prepared_message.subject = %v (template should be rendered)", prep["subject"])
	}
	if !strings.Contains(prep["body_text"].(string), "Hello Alice") {
		t.Errorf("prepared_message.body_text missing rendered template: %v", prep["body_text"])
	}
}

func TestDryRun_APIMode_HonorsToOverride(t *testing.T) {
	rt := &recordingTransport{}
	cfg := apiModeConfig("valid-key")
	cfg.DryRun = true
	h, _ := gateway.New(cfg, rt)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequest(`{"message":"hi","to_override":"alice@example.com"}`, "valid-key"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(rt.sent) != 0 {
		t.Errorf("transport.Send called %d times in dry-run; want 0", len(rt.sent))
	}

	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	prep, _ := body["prepared_message"].(map[string]any)
	toList, _ := prep["to"].([]any)
	if len(toList) != 1 || toList[0] != "alice@example.com" {
		t.Errorf("prepared_message.to = %v, want [alice@example.com]", prep["to"])
	}
}

// TestDryRun_ValidationStillFires asserts that dry-run doesn't skip the
// validation gate — operators using dry-run want to catch missing-field
// errors before flipping dry_run back to false.
func TestDryRun_ValidationStillFires(t *testing.T) {
	rt := &recordingTransport{}
	cfg := config.EndpointConfig{
		Path:     "/test",
		To:       []string{"to@example.com"},
		From:     "from@example.com",
		Subject:  "S",
		Body:     "B",
		Required: []string{"name"},
		DryRun:   true,
	}
	h, _ := gateway.New(cfg, rt)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("")) // missing required `name`

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 (validation still runs in dry-run)", rec.Code)
	}
}

func TestDryRun_DefaultOff_DeliversNormally(t *testing.T) {
	rt := &recordingTransport{}
	cfg := config.EndpointConfig{
		Path:    "/test",
		To:      []string{"to@example.com"},
		From:    "from@example.com",
		Subject: "S",
		Body:    "B",
		// DryRun omitted — defaults to false
	}
	h, _ := gateway.New(cfg, rt)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("message=hi"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(rt.sent) != 1 {
		t.Errorf("transport.Send count = %d, want 1 (DryRun off should send)", len(rt.sent))
	}
}

// --- v1.0 block C: CSRF (Story 10.3) ---

func csrfConfig(secret string) config.EndpointConfig {
	return config.EndpointConfig{
		Path:       "/test",
		To:         []string{"to@example.com"},
		From:       "from@example.com",
		Subject:    "S",
		Body:       "B",
		CSRFSecret: secret,
	}
}

const csrfTestSecret = "0123456789abcdef0123456789abcdef" // 32 bytes

func TestCSRF_ValidToken_Succeeds(t *testing.T) {
	rt := &recordingTransport{}
	cfg := csrfConfig(csrfTestSecret)
	h, err := gateway.New(cfg, rt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	token := csrf.IssueNow([]byte(csrfTestSecret))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("message=hi&_csrf_token="+token))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(rt.sent) != 1 {
		t.Errorf("transport.Send not called: sent=%d", len(rt.sent))
	}
}

func TestCSRF_MissingToken_403(t *testing.T) {
	h, err := gateway.New(csrfConfig(csrfTestSecret), &recordingTransport{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("message=hi")) // no _csrf_token
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestCSRF_TamperedToken_403(t *testing.T) {
	h, _ := gateway.New(csrfConfig(csrfTestSecret), &recordingTransport{})
	good := csrf.IssueNow([]byte(csrfTestSecret))
	tampered := good[:len(good)-1] + "0"
	if tampered == good {
		tampered = good[:len(good)-1] + "1"
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("message=hi&_csrf_token="+tampered))
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestCSRF_ExpiredToken_403(t *testing.T) {
	cfg := csrfConfig(csrfTestSecret)
	cfg.CSRFTokenTTL = config.Duration(time.Second)
	h, _ := gateway.New(cfg, &recordingTransport{})

	// Issue a token 10 seconds in the past.
	oldToken := csrf.Issue([]byte(csrfTestSecret), time.Now().Add(-10*time.Second))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("message=hi&_csrf_token="+oldToken))
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (expired)", rec.Code)
	}
}

func TestCSRF_WrongSecret_403(t *testing.T) {
	h, _ := gateway.New(csrfConfig(csrfTestSecret), &recordingTransport{})
	wrongSecret := "ffffffffffffffffffffffffffffffff"
	tokenFromWrongHost := csrf.IssueNow([]byte(wrongSecret))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("message=hi&_csrf_token="+tokenFromWrongHost))
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (wrong secret)", rec.Code)
	}
}

// TestCSRF_NotInCustomFieldsBlock pins that the _csrf_token form field
// doesn't show up in the rendered body's "Additional fields:" block —
// it's a structural field, not template content.
func TestCSRF_NotInCustomFieldsBlock(t *testing.T) {
	rt := &recordingTransport{}
	cfg := csrfConfig(csrfTestSecret)
	cfg.Subject = "S"
	cfg.Body = "Body"
	h, _ := gateway.New(cfg, rt)
	token := csrf.IssueNow([]byte(csrfTestSecret))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("message=hi&_csrf_token="+token))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rt.sent[0].BodyText
	if strings.Contains(body, "_csrf_token") {
		t.Errorf("_csrf_token leaked into body: %s", body)
	}
	if strings.Contains(body, token) {
		t.Errorf("CSRF token value leaked into body: %s", body)
	}
}

// TestCSRF_SecretNotInLogs pins NFR3 at the gateway level: even on a
// failed-CSRF path, the csrf_secret string must not appear in captured
// log output.
func TestCSRF_SecretNotInLogs(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	const sentinel = "sentinel-csrf-secret-do-not-leak-to-logs"
	// 32 bytes, contains sentinel
	secret := sentinel + sentinel
	h, err := gateway.New(csrfConfig(secret), &recordingTransport{}, gateway.WithLogger(logger))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Bad token → 403 → log line — sentinel must not appear.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("message=hi&_csrf_token=bogus"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if strings.Contains(logBuf.String(), sentinel) {
		t.Errorf("csrf_secret leaked in log output: %s", logBuf.String())
	}
}

func TestCSRF_FormModeUnconfigured_NoCheck(t *testing.T) {
	rt := &recordingTransport{}
	h := newTestHandler(t, rt) // no CSRFSecret
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, urlencodedRequest("message=hi")) // no token
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (CSRF off)", rec.Code)
	}
}

// --- v1.0 block C: IP stripping (Story 10.5) ---

func TestStripClientIP_OmitsFromRateLimitLog(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	cfg := config.EndpointConfig{
		Path:          "/test",
		To:            []string{"to@example.com"},
		From:          "from@example.com",
		Subject:       "S",
		Body:          "B",
		StripClientIP: true,
		RateLimit:     &config.RateLimitConfig{Count: 1, Interval: config.Duration(time.Minute)},
	}
	h, err := gateway.New(cfg, &recordingTransport{}, gateway.WithLogger(logger))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// First request succeeds and drains the bucket.
	h.ServeHTTP(httptest.NewRecorder(), urlencodedRequest("message=hi"))
	// Second hits the rate limit and emits rate_limited log.
	h.ServeHTTP(httptest.NewRecorder(), urlencodedRequest("message=hi"))

	out := logBuf.String()
	if !strings.Contains(out, "rate_limited") {
		t.Fatalf("rate_limited log line missing: %s", out)
	}
	if strings.Contains(out, "client_ip") {
		t.Errorf("client_ip field present despite strip_client_ip=true: %s", out)
	}
}

func TestStripClientIP_DefaultOff_LogsClientIP(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	cfg := config.EndpointConfig{
		Path:      "/test",
		To:        []string{"to@example.com"},
		From:      "from@example.com",
		Subject:   "S",
		Body:      "B",
		RateLimit: &config.RateLimitConfig{Count: 1, Interval: config.Duration(time.Minute)},
		// StripClientIP omitted → defaults to false
	}
	h, _ := gateway.New(cfg, &recordingTransport{}, gateway.WithLogger(logger))

	h.ServeHTTP(httptest.NewRecorder(), urlencodedRequest("message=hi"))
	h.ServeHTTP(httptest.NewRecorder(), urlencodedRequest("message=hi"))

	if !strings.Contains(logBuf.String(), "client_ip") {
		t.Errorf("client_ip field missing despite strip_client_ip=false (default): %s", logBuf.String())
	}
}
