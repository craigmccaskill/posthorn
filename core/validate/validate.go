// Package validate provides field-level validation primitives for form
// submissions. Functions here are pure: no logging, no response writing,
// no side effects. The handler combines their returns into HTTP 422
// responses (FR8, FR9).
//
// Two responsibilities in v1.0:
//
//   - RequiredFields reports which configured-required fields are missing
//     or empty in a submission.
//   - Email reports whether a string is a syntactically valid email.
//
// More validators (length caps, regex match, etc.) join in v1.1+ behind
// the same "pure function returning structured result" contract so the
// handler's translation to JSON 422 stays uniform.
package validate

import (
	"net/mail"
	"strings"
)

// RequiredFields returns the names of fields in `required` that are absent
// from `form` or present-but-empty (whitespace-only counts as empty).
// The returned list preserves the order of `required` so the caller's 422
// response matches the operator's configuration order — useful for users
// who eyeball error responses.
//
// A field counts as "present and non-empty" if at least one of its values
// (form keys can have multiple values) is non-empty after trimming
// whitespace. Empty strings, "   ", and missing keys all fail.
func RequiredFields(form map[string][]string, required []string) []string {
	if len(required) == 0 {
		return nil
	}
	missing := make([]string, 0, len(required))
	for _, name := range required {
		values, ok := form[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		if !anyNonEmpty(values) {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return missing
}

// anyNonEmpty returns true if any value in vs is non-empty after
// trimming whitespace.
func anyNonEmpty(vs []string) bool {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

// Email reports whether s is a syntactically valid email address.
//
// Uses [net/mail.ParseAddress] under the hood, which accepts RFC 5322
// addresses including display names ("Craig <craig@example.com>"). For
// submitter-email-field validation that should be a bare address, callers
// should additionally check that the parsed address has no Name field
// (use [BareEmail] for that).
//
// A non-existent or syntactically broken address returns false. This is
// not a deliverability check: foo@nonexistent.example is "valid" by
// syntax even though the domain doesn't resolve. v1.0 deliberately keeps
// validation cheap; deliverability is the transport's problem.
func Email(s string) bool {
	if s == "" {
		return false
	}
	_, err := mail.ParseAddress(s)
	return err == nil
}

// BareEmail reports whether s is a valid email AND has no display name
// component. Use this for form submitters' email fields where you want
// "craig@example.com" but not "Evil <craig@example.com>" (the latter
// could be a header-injection attempt against a less careful pipeline).
//
// Posthorn's transport layer is structured-data-only (NFR1), so the
// display name itself isn't a header-injection risk in our outbound
// path. But operators may still want the simpler "bare address only"
// rule for their submitter form field.
func BareEmail(s string) bool {
	if s == "" {
		return false
	}
	addr, err := mail.ParseAddress(s)
	if err != nil {
		return false
	}
	return addr.Name == "" && addr.Address == s
}
