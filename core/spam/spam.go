// Package spam holds the v1.0 spam-protection primitives:
//
//   - Honeypot: a hidden form field bots fill in but humans don't.
//   - Origin/Referer check: rejects direct-POST bots that skip the
//     form page.
//   - ParseSize: helper for the max_body_size config value.
//
// The body-size cap itself is enforced by http.MaxBytesReader in the
// gateway handler, not by a function in this package — the cap must be
// applied to the body reader before any read, so it lives at handler
// entry. ParseSize converts the human-friendly config string ("32KB")
// to bytes.
//
// Functions here are pure: no logging, no response writing. The
// gateway handler interprets the [Result] and produces the right HTTP
// response.
package spam

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Result is the outcome of a spam check.
type Result int

const (
	// Pass means the request is not flagged; continue the pipeline.
	Pass Result = iota
	// SilentReject means accept-and-discard with a 200 OK. Used for
	// the honeypot so bots can't distinguish honeypot rejection from
	// success and adapt.
	SilentReject
	// HardReject means deny with an explicit error status (403 for
	// origin failures).
	HardReject
)

// CheckHoneypot returns SilentReject if the configured honeypot field
// has any non-empty value in the form. If fieldName is empty (operator
// hasn't configured one), returns Pass.
//
// Per architecture doc: bots typically fill in every input they see;
// a hidden field they fill in is a high-confidence spam signal. Real
// users never trigger it because the field is hidden via CSS or
// positioned offscreen.
func CheckHoneypot(form map[string][]string, fieldName string) Result {
	if fieldName == "" {
		return Pass
	}
	values, ok := form[fieldName]
	if !ok {
		return Pass
	}
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return SilentReject
		}
	}
	return Pass
}

// CheckOrigin validates the request's Origin and Referer headers
// against an allow-list.
//
// Behavior:
//
//   - If allowed is empty/nil → Pass (operator has not configured
//     origin restrictions; fail-open by absence of config per FR6).
//   - If both Origin and Referer are missing → HardReject (fail-closed
//     per NFR4 when allowed_origins is configured).
//   - If Origin or the origin component of Referer matches any entry
//     in allowed → Pass.
//   - Otherwise → HardReject.
//
// The reason string accompanies HardReject for log fields. Pass
// results return an empty reason.
//
// Allowed entries are matched as URL prefixes against the request's
// origin. Operators specify them as bare origins like
// "https://example.com" — paths are not significant.
func CheckOrigin(originHeader, refererHeader string, allowed []string) (Result, string) {
	if len(allowed) == 0 {
		return Pass, ""
	}

	if originHeader == "" && refererHeader == "" {
		return HardReject, "Origin and Referer both missing"
	}

	// Try Origin first (more specific; browsers send only the origin).
	if originHeader != "" {
		if matchesAllowed(originHeader, allowed) {
			return Pass, ""
		}
	}

	// Fall back to Referer's origin component.
	if refererHeader != "" {
		refOrigin, err := originFromReferer(refererHeader)
		if err == nil && matchesAllowed(refOrigin, allowed) {
			return Pass, ""
		}
	}

	return HardReject, fmt.Sprintf("origin not in allow-list: origin=%q referer=%q", originHeader, refererHeader)
}

// matchesAllowed reports whether candidate exactly matches any entry
// in allowed. Comparison is case-insensitive on scheme and host (per
// RFC 3986); paths are stripped.
func matchesAllowed(candidate string, allowed []string) bool {
	candNorm, err := normalizeOrigin(candidate)
	if err != nil {
		return false
	}
	for _, a := range allowed {
		allowNorm, err := normalizeOrigin(a)
		if err != nil {
			continue
		}
		if candNorm == allowNorm {
			return true
		}
	}
	return false
}

// normalizeOrigin returns the canonical form scheme://host[:port],
// lowercased, without trailing slash or path. Returns an error if the
// input doesn't parse as a URL.
func normalizeOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("not an origin: %q", raw)
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host), nil
}

// originFromReferer extracts the origin component from a full URL.
func originFromReferer(referer string) (string, error) {
	u, err := url.Parse(referer)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("not a URL: %q", referer)
	}
	return u.Scheme + "://" + u.Host, nil
}

// ExtractOriginAndReferer reads the standard headers from a request.
// Centralized helper so handler code doesn't have to remember casing.
func ExtractOriginAndReferer(r *http.Request) (origin, referer string) {
	return r.Header.Get("Origin"), r.Header.Get("Referer")
}

// ParseSize parses a size string with units B, KB, MB, GB.
// Examples: "32KB" → 32768, "1MB" → 1048576, "500" → 500 (bytes).
// Empty input returns 0 with no error so operators can omit the
// directive (handler treats 0 as "no cap" or applies a default).
//
// Multipliers use binary (1024) units, matching common operator
// expectations (Docker Compose mem_limit, k8s resource quotas, etc.).
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}

	upper := strings.ToUpper(s)
	multiplier := int64(1)
	switch {
	case strings.HasSuffix(upper, "GB"):
		multiplier = 1024 * 1024 * 1024
		s = upper[:len(upper)-2]
	case strings.HasSuffix(upper, "MB"):
		multiplier = 1024 * 1024
		s = upper[:len(upper)-2]
	case strings.HasSuffix(upper, "KB"):
		multiplier = 1024
		s = upper[:len(upper)-2]
	case strings.HasSuffix(upper, "B"):
		s = upper[:len(upper)-1]
	default:
		s = upper
	}

	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse size %q: %w", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("size must be non-negative, got %d", n)
	}
	return n * multiplier, nil
}
