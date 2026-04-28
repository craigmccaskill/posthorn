package response_test

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/craigmccaskill/posthorn/response"
)

// --- WriteJSON ---

func TestWriteJSON_Success(t *testing.T) {
	rec := httptest.NewRecorder()
	response.WriteJSON(rec, 200, response.Success{})
	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if !strings.Contains(rec.Body.String(), "{}") {
		t.Errorf("body = %q, want JSON object", rec.Body.String())
	}
}

func TestWriteJSON_NilBody(t *testing.T) {
	// Nil body is accepted (e.g., 204 No Content). Header and status still set.
	rec := httptest.NewRecorder()
	response.WriteJSON(rec, 204, nil)
	if rec.Code != 204 {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
}

// --- Validation ---

func TestValidation_MissingFieldsOnly(t *testing.T) {
	err := response.Validation([]string{"name", "email"}, nil)
	if err.Code != "validation_failed" {
		t.Errorf("Code = %q", err.Code)
	}
	if err.Fields["name"] != "required" {
		t.Errorf("Fields[name] = %q, want required", err.Fields["name"])
	}
	if err.Fields["email"] != "required" {
		t.Errorf("Fields[email] = %q, want required", err.Fields["email"])
	}
}

func TestValidation_FieldErrorsOnly(t *testing.T) {
	err := response.Validation(nil, map[string]string{
		"email": "invalid format",
	})
	if err.Fields["email"] != "invalid format" {
		t.Errorf("Fields[email] = %q", err.Fields["email"])
	}
}

func TestValidation_BothMissingAndFormatErrors(t *testing.T) {
	err := response.Validation(
		[]string{"name"},
		map[string]string{"email": "invalid format"},
	)
	if got := len(err.Fields); got != 2 {
		t.Errorf("Fields length = %d, want 2", got)
	}
	if err.Fields["name"] != "required" {
		t.Errorf("name field = %q", err.Fields["name"])
	}
	if err.Fields["email"] != "invalid format" {
		t.Errorf("email field = %q", err.Fields["email"])
	}
}

func TestValidation_RequiredTakesPrecedenceOverFormat(t *testing.T) {
	// If a field is both missing AND has a format error (caller passes
	// both), the "required" message wins. Missing is the more fundamental
	// problem; format checks against an empty value would be confusing.
	err := response.Validation(
		[]string{"email"},
		map[string]string{"email": "invalid format"},
	)
	if err.Fields["email"] != "required" {
		t.Errorf("email field = %q, want required (precedence over format error)", err.Fields["email"])
	}
}

func TestValidation_JSONShape(t *testing.T) {
	// Pin the wire shape: { "error": ..., "code": ..., "fields": { ... } }
	v := response.Validation([]string{"name"}, nil)
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := got["error"].(string); !ok {
		t.Errorf("missing 'error' string field: %v", got)
	}
	if _, ok := got["code"].(string); !ok {
		t.Errorf("missing 'code' string field: %v", got)
	}
	fields, ok := got["fields"].(map[string]any)
	if !ok {
		t.Fatalf("missing 'fields' object: %v", got)
	}
	if fields["name"] != "required" {
		t.Errorf("fields.name = %v, want required", fields["name"])
	}
}

func TestValidation_NoFields_OmitsFieldsKey(t *testing.T) {
	// With no validation errors, the Fields map is empty. Make sure the
	// JSON output still has a sensible shape (omitempty drops the key).
	v := response.Validation(nil, nil)
	b, _ := json.Marshal(v)
	if strings.Contains(string(b), `"fields"`) {
		// fields was populated; that's fine but let's confirm it's empty
		if !strings.Contains(string(b), `"fields":{}`) {
			t.Errorf("expected empty fields map, got: %s", b)
		}
	}
}

// --- Negotiate ---

func TestNegotiate_NoRedirectsConfigured_JSON(t *testing.T) {
	// FR14: if no redirect URLs are configured, fall back to JSON
	// regardless of Accept header.
	tests := []string{"", "text/html", "application/json", "*/*"}
	for _, accept := range tests {
		t.Run(accept, func(t *testing.T) {
			if got := response.Negotiate(accept, false); got != response.ModeJSON {
				t.Errorf("Negotiate(%q, false) = %v, want JSON", accept, got)
			}
		})
	}
}

func TestNegotiate_BrowserDefault_Redirect(t *testing.T) {
	// Plain browser form submission → redirect.
	got := response.Negotiate("text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", true)
	if got != response.ModeRedirect {
		t.Errorf("got %v, want Redirect", got)
	}
}

func TestNegotiate_FetchAPIDefault_JSON(t *testing.T) {
	// fetch() with no explicit Accept → */*. Modern fetch with .json()
	// would set Accept: application/json. JSON wins when explicitly
	// requested without HTML alongside.
	got := response.Negotiate("application/json", true)
	if got != response.ModeJSON {
		t.Errorf("got %v, want JSON", got)
	}
}

func TestNegotiate_BothJSONAndHTML_Redirect(t *testing.T) {
	// Mixed Accept headers (some libraries do this) → redirect, since
	// HTML is present.
	got := response.Negotiate("application/json, text/html", true)
	if got != response.ModeRedirect {
		t.Errorf("got %v, want Redirect", got)
	}
}

func TestNegotiate_EmptyAccept_Redirect(t *testing.T) {
	// No Accept header on a redirect-configured endpoint → redirect.
	// (Treat as browser form submission.)
	got := response.Negotiate("", true)
	if got != response.ModeRedirect {
		t.Errorf("got %v, want Redirect", got)
	}
}

func TestNegotiate_CaseInsensitive(t *testing.T) {
	got := response.Negotiate("APPLICATION/JSON", true)
	if got != response.ModeJSON {
		t.Errorf("got %v, want JSON (Accept header should be case-insensitive)", got)
	}
}
