package transport

import (
	"bytes"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestDeriveSigningKey_AWSExample verifies the four-step HMAC chain
// against AWS's published example. From:
// https://docs.aws.amazon.com/general/latest/gr/signature-v4-examples.html
//
// Inputs:
//
//	secretKey = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
//	dateStamp = "20150830"
//	region    = "us-east-1"
//	service   = "iam"
//
// Expected kSigning hex:
//
//	c4afb1cc5771d871763a393e44b703571b55cc28424d1a5e86da6ed3c154a4b9
func TestDeriveSigningKey_AWSPublishedExample(t *testing.T) {
	secret := "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	dateStamp := "20150830"
	region := "us-east-1"
	service := "iam"
	wantHex := "c4afb1cc5771d871763a393e44b703571b55cc28424d1a5e86da6ed3c154a4b9"

	got := hex.EncodeToString(deriveSigningKey(secret, dateStamp, region, service))
	if got != wantHex {
		t.Errorf("deriveSigningKey hex = %s\n want = %s", got, wantHex)
	}
}

func TestSHA256Hex_KnownVector(t *testing.T) {
	// SHA256("") = e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
	got := sha256Hex([]byte(""))
	want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got != want {
		t.Errorf("sha256Hex(\"\") = %s, want %s", got, want)
	}
}

func TestAWSURIEncode(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		encodePath bool
		want       string
	}{
		{"unreserved_passthrough", "AZaz09-_.~", true, "AZaz09-_.~"},
		{"space_to_percent20", "a b", true, "a%20b"},
		{"slash_preserved_in_path", "/foo/bar", true, "/foo/bar"},
		{"slash_encoded_in_query", "/foo/bar", false, "%2Ffoo%2Fbar"},
		{"plus_encoded", "a+b", true, "a%2Bb"},
		{"equals_encoded", "a=b", true, "a%3Db"},
		{"ampersand_encoded", "a&b", true, "a%26b"},
		{"uppercase_hex_digits", "\x7f", true, "%7F"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := awsURIEncode(tt.in, tt.encodePath)
			if got != tt.want {
				t.Errorf("awsURIEncode(%q, %v) = %q, want %q", tt.in, tt.encodePath, got, tt.want)
			}
		})
	}
}

// TestSignRequest_AddsExpectedHeaders covers the happy path: SignRequest
// installs X-Amz-Date, X-Amz-Content-Sha256, and Authorization headers
// with the right shape.
func TestSignRequest_AddsExpectedHeaders(t *testing.T) {
	body := []byte(`{"action":"test"}`)
	req, _ := http.NewRequest(http.MethodPost,
		"https://email.us-east-1.amazonaws.com/v2/email/outbound-emails",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	creds := SigV4Credentials{
		AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
	}
	signTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)

	SignRequest(req, body, creds, "us-east-1", "ses", signTime)

	if got := req.Header.Get("X-Amz-Date"); got != "20240115T120000Z" {
		t.Errorf("X-Amz-Date = %q, want %q", got, "20240115T120000Z")
	}
	contentSha := sha256Hex(body)
	if got := req.Header.Get("X-Amz-Content-Sha256"); got != contentSha {
		t.Errorf("X-Amz-Content-Sha256 = %q, want %q", got, contentSha)
	}
	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, sigV4Algorithm+" ") {
		t.Errorf("Authorization missing algorithm prefix: %s", auth)
	}
	if !strings.Contains(auth, "Credential=AKIAIOSFODNN7EXAMPLE/20240115/us-east-1/ses/aws4_request") {
		t.Errorf("Authorization missing expected Credential: %s", auth)
	}
	if !strings.Contains(auth, "SignedHeaders=") {
		t.Errorf("Authorization missing SignedHeaders: %s", auth)
	}
	if !strings.Contains(auth, "Signature=") {
		t.Errorf("Authorization missing Signature: %s", auth)
	}
}

// TestSignRequest_DeterministicForFixedInput pins that the same inputs
// produce the same Authorization header — a regression would imply
// non-determinism somewhere in the signing pipeline (e.g., header order).
func TestSignRequest_DeterministicForFixedInput(t *testing.T) {
	body := []byte(`{"k":"v"}`)
	creds := SigV4Credentials{
		AccessKeyID:     "AKIAEXAMPLE",
		SecretAccessKey: "secret-do-not-leak",
	}
	signTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)

	auth1 := signOnce(t, body, creds, signTime)
	auth2 := signOnce(t, body, creds, signTime)
	if auth1 != auth2 {
		t.Errorf("Authorization differs across runs:\n  %s\n  %s", auth1, auth2)
	}
}

// TestSignRequest_DifferentTimeChangesSignature pins that the timestamp
// is part of the signing material — a stolen signature for one moment
// can't be reused at a different moment.
func TestSignRequest_DifferentTimeChangesSignature(t *testing.T) {
	body := []byte(`{"k":"v"}`)
	creds := SigV4Credentials{
		AccessKeyID:     "AKIAEXAMPLE",
		SecretAccessKey: "secret-do-not-leak",
	}

	auth1 := signOnce(t, body, creds, time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC))
	auth2 := signOnce(t, body, creds, time.Date(2024, 1, 15, 12, 0, 1, 0, time.UTC))
	if auth1 == auth2 {
		t.Errorf("Authorization unchanged across 1s of time:\n  %s", auth1)
	}
}

// TestSignRequest_DifferentBodyChangesSignature pins that the body hash
// is part of the signing material.
func TestSignRequest_DifferentBodyChangesSignature(t *testing.T) {
	creds := SigV4Credentials{
		AccessKeyID:     "AKIAEXAMPLE",
		SecretAccessKey: "secret-do-not-leak",
	}
	signTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)

	auth1 := signOnce(t, []byte(`{"k":"v1"}`), creds, signTime)
	auth2 := signOnce(t, []byte(`{"k":"v2"}`), creds, signTime)
	if auth1 == auth2 {
		t.Errorf("Authorization unchanged across different bodies")
	}
}

// TestSignRequest_SecretNotInHeaders pins NFR3 at the signing-primitive
// level: the secret access key must never appear in any request header
// (only the access key ID and the derived signature do).
func TestSignRequest_SecretNotInHeaders(t *testing.T) {
	const secret = "sentinel-secret-key-do-not-leak-via-headers"
	body := []byte(`{"k":"v"}`)
	req, _ := http.NewRequest(http.MethodPost, "https://email.us-east-1.amazonaws.com/v2/email/outbound-emails", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	creds := SigV4Credentials{AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: secret}
	SignRequest(req, body, creds, "us-east-1", "ses", time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC))

	for name, values := range req.Header {
		for _, v := range values {
			if strings.Contains(v, secret) {
				t.Errorf("secret leaked in header %q: %s", name, v)
			}
		}
	}
}

// signOnce is a helper that signs a fresh request and returns just the
// Authorization header — used in determinism tests.
func signOnce(t *testing.T, body []byte, creds SigV4Credentials, signTime time.Time) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost,
		"https://email.us-east-1.amazonaws.com/v2/email/outbound-emails",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	SignRequest(req, body, creds, "us-east-1", "ses", signTime)
	return req.Header.Get("Authorization")
}

func TestReadAndReplaceBody_RoundTrip(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/", strings.NewReader("hello"))
	got, err := readAndReplaceBody(req)
	if err != nil {
		t.Fatalf("readAndReplaceBody: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("returned bytes = %q, want %q", string(got), "hello")
	}
	// Body should be readable again.
	again, _ := io.ReadAll(req.Body)
	if string(again) != "hello" {
		t.Errorf("body after replace = %q, want %q", string(again), "hello")
	}
}

func TestReadAndReplaceBody_NilBody(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	got, err := readAndReplaceBody(req)
	if err != nil || got != nil {
		t.Errorf("nil-body case: got=%v err=%v, want nil/nil", got, err)
	}
}
