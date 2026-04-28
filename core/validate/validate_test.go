package validate_test

import (
	"reflect"
	"testing"

	"github.com/craigmccaskill/posthorn/validate"
)

// --- RequiredFields ---

func TestRequiredFields_AllPresent(t *testing.T) {
	form := map[string][]string{
		"name":    {"craig"},
		"email":   {"craig@example.com"},
		"message": {"hello"},
	}
	got := validate.RequiredFields(form, []string{"name", "email", "message"})
	if len(got) != 0 {
		t.Errorf("missing = %v, want empty", got)
	}
}

func TestRequiredFields_OneMissing(t *testing.T) {
	form := map[string][]string{
		"name":  {"craig"},
		"email": {"craig@example.com"},
	}
	got := validate.RequiredFields(form, []string{"name", "email", "message"})
	want := []string{"message"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("missing = %v, want %v", got, want)
	}
}

func TestRequiredFields_MultipleMissing(t *testing.T) {
	form := map[string][]string{
		"name": {"craig"},
	}
	got := validate.RequiredFields(form, []string{"name", "email", "message"})
	want := []string{"email", "message"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("missing = %v, want %v", got, want)
	}
}

func TestRequiredFields_PreservesConfigOrder(t *testing.T) {
	// Operators read the JSON 422 response top-to-bottom; reporting fields
	// in their configured order makes "fix one, retry, fix next" easier.
	form := map[string][]string{}
	got := validate.RequiredFields(form, []string{"zeta", "alpha", "mike", "bravo"})
	want := []string{"zeta", "alpha", "mike", "bravo"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("missing = %v, want %v (must preserve order)", got, want)
	}
}

func TestRequiredFields_EmptyValueCountsAsMissing(t *testing.T) {
	form := map[string][]string{
		"name":  {""},
		"email": {"craig@example.com"},
	}
	got := validate.RequiredFields(form, []string{"name", "email"})
	want := []string{"name"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("missing = %v, want %v", got, want)
	}
}

func TestRequiredFields_WhitespaceOnlyCountsAsMissing(t *testing.T) {
	form := map[string][]string{
		"name":  {"   "},
		"email": {"\t\n  "},
	}
	got := validate.RequiredFields(form, []string{"name", "email"})
	want := []string{"name", "email"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("missing = %v, want %v", got, want)
	}
}

func TestRequiredFields_MultiValueAtLeastOneNonEmpty(t *testing.T) {
	// HTTP form parsers may produce multi-valued fields (checkbox arrays,
	// repeated keys). At least one non-empty value should satisfy the rule.
	form := map[string][]string{
		"name": {"", "  ", "craig"}, // third value is non-empty
	}
	got := validate.RequiredFields(form, []string{"name"})
	if len(got) != 0 {
		t.Errorf("missing = %v, want empty (at least one non-empty value present)", got)
	}
}

func TestRequiredFields_NoRequiredList(t *testing.T) {
	// No required fields configured → nothing to fail.
	form := map[string][]string{}
	if got := validate.RequiredFields(form, nil); got != nil {
		t.Errorf("missing = %v, want nil for empty required list", got)
	}
	if got := validate.RequiredFields(form, []string{}); got != nil {
		t.Errorf("missing = %v, want nil for empty required list", got)
	}
}

func TestRequiredFields_EmptyForm(t *testing.T) {
	got := validate.RequiredFields(nil, []string{"name"})
	want := []string{"name"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("missing = %v, want %v", got, want)
	}
}

// --- Email ---

func TestEmail_Valid(t *testing.T) {
	tests := []string{
		"craig@example.com",
		"a@b.co",
		"plus+tag@example.com",
		"dotted.name@example.co.uk",
		"Craig <craig@example.com>", // RFC 5322 allows display name
	}
	for _, s := range tests {
		t.Run(s, func(t *testing.T) {
			if !validate.Email(s) {
				t.Errorf("Email(%q) = false, want true", s)
			}
		})
	}
}

func TestEmail_Invalid(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"no_at_sign", "craig.example.com"},
		{"no_local_part", "@example.com"},
		{"no_domain", "craig@"},
		{"just_at", "@"},
		{"spaces", "craig @ example.com"},
		{"not_an_email", "not an email"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if validate.Email(tt.input) {
				t.Errorf("Email(%q) = true, want false", tt.input)
			}
		})
	}
}

// --- BareEmail ---

func TestBareEmail_AcceptsBareAddress(t *testing.T) {
	if !validate.BareEmail("craig@example.com") {
		t.Error("BareEmail(\"craig@example.com\") = false, want true")
	}
}

func TestBareEmail_RejectsDisplayName(t *testing.T) {
	// Display-name forms parse fine via mail.ParseAddress but BareEmail
	// rejects them. Operators who want bare addresses-only on their submitter
	// field can opt in to this stricter check.
	if validate.BareEmail("Craig <craig@example.com>") {
		t.Error("BareEmail(display-name form) = true, want false")
	}
}

func TestBareEmail_RejectsInvalid(t *testing.T) {
	tests := []string{
		"",
		"not-an-email",
		"@example.com",
		"craig@",
	}
	for _, s := range tests {
		t.Run(s, func(t *testing.T) {
			if validate.BareEmail(s) {
				t.Errorf("BareEmail(%q) = true, want false", s)
			}
		})
	}
}

func TestBareEmail_RejectsCRLFInjection(t *testing.T) {
	// Even if mail.ParseAddress would tolerate it (it doesn't, but check
	// the contract), CRLF in the input must produce false. This is defense
	// in depth — the transport's NFR1 guard is the authoritative defense,
	// but rejecting at validate layer means we never even reach the
	// transport with smuggled-header input.
	tests := []string{
		"craig\r\nBcc:evil@example.com@example.com",
		"craig@example.com\r\nX-Spoof: yes",
		"craig\nBcc:evil@example.com",
	}
	for _, s := range tests {
		t.Run(s, func(t *testing.T) {
			if validate.BareEmail(s) {
				t.Errorf("BareEmail(%q) = true, want false (must reject CRLF payloads)", s)
			}
		})
	}
}
