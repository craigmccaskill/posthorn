// Package csrf implements the HMAC-signed timestamp tokens used for
// form-mode CSRF protection (FR57, ADR-16).
//
// Token format: `<unix-seconds>.<hex-encoded HMAC-SHA256>`
//
//	hmac = HMAC-SHA256(csrf_secret, unix-seconds-as-bytes)
//
// The operator issues tokens server-side at form-render time using
// Issue. Posthorn verifies tokens on submit using Verify. The secret
// never crosses to the client; only the timestamp + hmac do.
//
// NFR3 enforcement: secrets reach this package via Issue/Verify
// parameters, never via package-level state. The Verify return value
// is a structured error type that callers can map to HTTP 403; the
// error string does not contain secret material.
package csrf

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Sentinel errors returned by Verify. All map to HTTP 403 at the
// handler level; the difference is operator log shape, not response
// detail (we never tell the client which way the token was bad).
var (
	// ErrMissingToken means the request didn't include a token at all.
	ErrMissingToken = errors.New("csrf: token missing")
	// ErrMalformedToken means the token didn't have the right shape
	// (wrong number of dots, non-numeric timestamp, etc.).
	ErrMalformedToken = errors.New("csrf: token malformed")
	// ErrInvalidSignature means the HMAC didn't verify. Either the
	// token was forged, the secret was rotated, or the token was
	// issued for a different endpoint/secret.
	ErrInvalidSignature = errors.New("csrf: signature invalid")
	// ErrExpired means the token's timestamp is older than the
	// configured TTL.
	ErrExpired = errors.New("csrf: token expired")
)

// Issue computes a token for the given moment using secret. Operators
// call this at form-render time and embed the result as a hidden
// `_csrf_token` form field. The token is opaque to the client.
//
// secret may be any byte sequence; longer secrets give better security
// margin. The recommended shape is a random 32-byte key loaded from
// ${env.CSRF_SECRET}.
func Issue(secret []byte, issuedAt time.Time) string {
	ts := strconv.FormatInt(issuedAt.Unix(), 10)
	return ts + "." + hex.EncodeToString(sign([]byte(ts), secret))
}

// Verify checks that token is a valid HMAC-signed timestamp within ttl
// of now. Returns one of the sentinel errors above on failure; nil on
// success.
//
// The HMAC comparison uses subtle.ConstantTimeCompare-equivalent
// behavior via hmac.Equal so timing attacks can't probe the secret.
func Verify(token string, secret []byte, ttl time.Duration, now time.Time) error {
	if token == "" {
		return ErrMissingToken
	}
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return ErrMalformedToken
	}
	tsStr, sigHex := parts[0], parts[1]

	tsUnix, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return ErrMalformedToken
	}

	gotSig, err := hex.DecodeString(sigHex)
	if err != nil {
		return ErrMalformedToken
	}
	wantSig := sign([]byte(tsStr), secret)
	if !hmac.Equal(gotSig, wantSig) {
		return ErrInvalidSignature
	}

	// Reject tokens older than ttl. We don't reject "future" tokens
	// (timestamps > now); HMAC verification already guarantees the
	// timestamp wasn't forged without the secret. Modest clock skew
	// from the operator's form-render host is tolerated.
	issuedAt := time.Unix(tsUnix, 0)
	if now.Sub(issuedAt) > ttl {
		return ErrExpired
	}

	return nil
}

// sign returns HMAC-SHA256(secret, message) as raw bytes.
func sign(message, secret []byte) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write(message)
	return h.Sum(nil)
}

// IssueNow is a convenience wrapper for Issue(secret, time.Now()). Used
// by examples and operator-side token rendering code that doesn't need
// time injection for testing.
func IssueNow(secret []byte) string {
	return Issue(secret, time.Now())
}

// TokenField is the canonical form-field name where Posthorn looks for
// the CSRF token. Operators embed the token under this exact name.
const TokenField = "_csrf_token"

// ValidateSecret enforces the minimum operator-facing secret hygiene at
// config-parse time: secrets must be at least 16 bytes (otherwise a
// brute-force pre-image attack against HMAC-SHA256 becomes feasible).
// Returns nil on success or a clear error suitable for parse-time
// rejection.
func ValidateSecret(secret []byte) error {
	const minBytes = 16
	if len(secret) < minBytes {
		return fmt.Errorf("csrf_secret must be at least %d bytes; got %d", minBytes, len(secret))
	}
	return nil
}
