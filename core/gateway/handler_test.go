package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
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

func (s *scriptedTransport) Send(_ context.Context, _ transport.Message) error {
	defer func() { s.calls++ }()
	if s.calls < len(s.results) {
		return s.results[s.calls]
	}
	return nil
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
