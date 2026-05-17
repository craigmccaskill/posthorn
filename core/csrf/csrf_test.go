package csrf

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef") // 32 bytes

func TestIssueAndVerify_RoundTrip(t *testing.T) {
	issuedAt := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	tok := Issue(testSecret, issuedAt)

	if !strings.Contains(tok, ".") {
		t.Errorf("token missing `.` separator: %q", tok)
	}

	// Verify at issuedAt + 30min (within default 1h TTL).
	now := issuedAt.Add(30 * time.Minute)
	if err := Verify(tok, testSecret, time.Hour, now); err != nil {
		t.Errorf("Verify (within TTL): %v", err)
	}
}

func TestVerify_Missing(t *testing.T) {
	if err := Verify("", testSecret, time.Hour, time.Now()); !errors.Is(err, ErrMissingToken) {
		t.Errorf("got %v, want ErrMissingToken", err)
	}
}

func TestVerify_Malformed(t *testing.T) {
	tests := []struct {
		name  string
		token string
	}{
		{"no_separator", "1700000000abcdef"},
		{"non_numeric_timestamp", "notanumber.deadbeef"},
		{"non_hex_signature", "1700000000.nothex"},
		{"empty_parts", "."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Verify(tt.token, testSecret, time.Hour, time.Now())
			if !errors.Is(err, ErrMalformedToken) {
				t.Errorf("got %v, want ErrMalformedToken", err)
			}
		})
	}
}

func TestVerify_InvalidSignature(t *testing.T) {
	issuedAt := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	tok := Issue(testSecret, issuedAt)
	// Verify with a different secret — signature can't match.
	wrongSecret := []byte("ffffffffffffffffffffffffffffffff")
	err := Verify(tok, wrongSecret, time.Hour, issuedAt.Add(time.Minute))
	if !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("got %v, want ErrInvalidSignature", err)
	}
}

func TestVerify_Tampered(t *testing.T) {
	issuedAt := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	tok := Issue(testSecret, issuedAt)
	// Flip a byte in the signature.
	tampered := tok[:len(tok)-1] + "0"
	if tampered == tok {
		tampered = tok[:len(tok)-1] + "1" // ensure actually different
	}
	err := Verify(tampered, testSecret, time.Hour, issuedAt.Add(time.Minute))
	if !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("got %v, want ErrInvalidSignature", err)
	}
}

func TestVerify_TamperedTimestamp(t *testing.T) {
	issuedAt := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	tok := Issue(testSecret, issuedAt)
	// Replace the timestamp with a different one. Signature now
	// doesn't match the timestamp → invalid.
	idx := strings.IndexByte(tok, '.')
	tampered := "1800000000" + tok[idx:]
	err := Verify(tampered, testSecret, time.Hour, time.Unix(1800000000, 0))
	if !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("got %v, want ErrInvalidSignature (sig doesn't match tampered ts)", err)
	}
}

func TestVerify_Expired(t *testing.T) {
	issuedAt := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	tok := Issue(testSecret, issuedAt)
	// Verify 2h after issuance with a 1h TTL.
	err := Verify(tok, testSecret, time.Hour, issuedAt.Add(2*time.Hour))
	if !errors.Is(err, ErrExpired) {
		t.Errorf("got %v, want ErrExpired", err)
	}
}

// TestVerify_FutureToken pins that we accept "future" tokens (timestamp
// > now). Modest clock skew between the form-render host and the
// Posthorn host is tolerated — the HMAC already prevents forgery
// without the secret.
func TestVerify_FutureToken(t *testing.T) {
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	future := now.Add(30 * time.Second)
	tok := Issue(testSecret, future)
	if err := Verify(tok, testSecret, time.Hour, now); err != nil {
		t.Errorf("modest future-skew rejected: %v", err)
	}
}

func TestValidateSecret(t *testing.T) {
	if err := ValidateSecret([]byte("short")); err == nil {
		t.Error("short secret accepted")
	}
	if err := ValidateSecret(testSecret); err != nil {
		t.Errorf("32-byte secret rejected: %v", err)
	}
}

// TestVerify_SecretNotInError pins NFR3: the error returned from Verify
// must not contain secret material, even on the invalid-signature path
// where the error implies "your token didn't verify with our secret."
func TestVerify_SecretNotInError(t *testing.T) {
	const sentinel = "sentinel-csrf-secret-do-not-leak"
	secret := []byte(sentinel + sentinel) // 64 bytes, contains sentinel
	tok := Issue([]byte("different-secret-different-secret"), time.Now())

	err := Verify(tok, secret, time.Hour, time.Now())
	if err == nil {
		t.Fatal("expected error from wrong-secret verify")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("secret sentinel leaked in error: %v", err)
	}
}
