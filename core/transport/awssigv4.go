package transport

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// AWS Signature Version 4 (SigV4) signing primitive.
//
// This is the auth scheme AWS services (SES, S3, etc.) use. We implement
// it bespoke per ADR-14 to avoid pulling in aws-sdk-go-v2 (large transitive
// dep tree) for the narrow surface of "sign one HTTP request for SES."
//
// The algorithm is well-documented:
// https://docs.aws.amazon.com/general/latest/gr/sigv4_signing.html
//
// NFR3 enforcement: secrets reach this file via parameters to Sign and
// SignRequest, never via package-level state. Tests use sentinel secrets
// to verify nothing crosses to logs.

// SigV4Algorithm is the algorithm identifier in the Authorization header.
const sigV4Algorithm = "AWS4-HMAC-SHA256"

// SigV4Credentials carries the static credentials used to sign requests.
// AccessKeyID is operator-facing identification (e.g., AKIA...); it
// appears in the Authorization header. SecretAccessKey is the signing
// secret — never logged, never sent on the wire.
type SigV4Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
}

// SignRequest computes the SigV4 signature for req and adds the required
// headers (X-Amz-Date, Authorization, X-Amz-Content-Sha256). The caller
// supplies region (e.g., "us-east-1"), service (e.g., "ses"), and the
// current time (injectable for deterministic tests; production passes
// time.Now().UTC()).
//
// req.Body must be a fully-readable stream that can be consumed once for
// hashing and then re-consumed by http.Client when the request is sent.
// In practice this means passing bytes.NewReader of a buffered payload.
//
// SECURITY: secrets are passed in via creds and never written to req
// headers as plaintext or used outside of HMAC inputs. The Authorization
// header carries the AccessKeyID (operator-facing) and the signature
// (computed; reveals nothing about the secret beyond the message being
// signed).
func SignRequest(req *http.Request, body []byte, creds SigV4Credentials, region, service string, now time.Time) {
	now = now.UTC()
	dateStamp := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")

	// Body hash header is part of the canonical request and must appear in
	// the signed-headers list.
	bodyHash := sha256Hex(body)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", bodyHash)
	// Ensure Host header is set on req — required by canonical request.
	// http.NewRequest sets req.URL.Host; we copy to a Header entry so the
	// canonical-headers walk picks it up uniformly.
	if req.Host == "" {
		req.Host = req.URL.Host
	}

	canonicalReq, signedHeaders := canonicalRequest(req, bodyHash)
	scope := credentialScope(dateStamp, region, service)
	stringToSign := strings.Join([]string{
		sigV4Algorithm,
		amzDate,
		scope,
		sha256Hex([]byte(canonicalReq)),
	}, "\n")

	signingKey := deriveSigningKey(creds.SecretAccessKey, dateStamp, region, service)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	authHeader := sigV4Algorithm + " " +
		"Credential=" + creds.AccessKeyID + "/" + scope + ", " +
		"SignedHeaders=" + signedHeaders + ", " +
		"Signature=" + signature
	req.Header.Set("Authorization", authHeader)
}

// canonicalRequest builds the canonical-request string per the SigV4
// spec and returns (canonical-request, signed-headers-list).
func canonicalRequest(req *http.Request, bodyHash string) (string, string) {
	method := req.Method

	// Canonical URI: encoded path. Empty path becomes "/".
	uri := req.URL.EscapedPath()
	if uri == "" {
		uri = "/"
	}

	// Canonical query string: keys sorted, encoded in canonical form.
	q := req.URL.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var qparts []string
	for _, k := range keys {
		values := q[k]
		sort.Strings(values)
		for _, v := range values {
			// Query string: encode '/' as %2F (encodePath=false per SigV4 spec).
			qparts = append(qparts, awsURIEncode(k, false)+"="+awsURIEncode(v, false))
		}
	}
	canonQuery := strings.Join(qparts, "&")

	// Canonical headers: lowercase key, trimmed value, sorted by key.
	// Host always included.
	headerMap := map[string]string{}
	for k, v := range req.Header {
		headerMap[strings.ToLower(k)] = strings.TrimSpace(strings.Join(v, ","))
	}
	headerMap["host"] = req.Host
	hkeys := make([]string, 0, len(headerMap))
	for k := range headerMap {
		hkeys = append(hkeys, k)
	}
	sort.Strings(hkeys)

	var canonHeaders strings.Builder
	for _, k := range hkeys {
		canonHeaders.WriteString(k)
		canonHeaders.WriteByte(':')
		canonHeaders.WriteString(headerMap[k])
		canonHeaders.WriteByte('\n')
	}
	signedHeaders := strings.Join(hkeys, ";")

	canonReq := strings.Join([]string{
		method,
		uri,
		canonQuery,
		canonHeaders.String(),
		signedHeaders,
		bodyHash,
	}, "\n")
	return canonReq, signedHeaders
}

// credentialScope is the date/region/service/aws4_request tuple included
// in both the string-to-sign and the Authorization header.
func credentialScope(dateStamp, region, service string) string {
	return dateStamp + "/" + region + "/" + service + "/aws4_request"
}

// deriveSigningKey performs the four-step HMAC derivation that produces
// the request-specific signing key. The chain mixes the date, region,
// and service into the secret so a stolen signature for one request
// cannot be replayed against a different date/region/service.
func deriveSigningKey(secretKey, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secretKey), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	return kSigning
}

// hmacSHA256 returns the HMAC-SHA256 of msg using key.
func hmacSHA256(key, msg []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(msg)
	return h.Sum(nil)
}

// sha256Hex returns the lowercase hex encoding of SHA256(data). Used for
// both the body hash and the canonical-request hash.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// awsURIEncode encodes a string per the SigV4 URI-encoding rules. When
// encodePath is true, '/' is preserved (used in canonical URI). When
// false (used in query strings), '/' is encoded.
//
// SigV4 differs from RFC 3986 in that the unreserved-character set is
// stricter: only letters, digits, and these four — '-', '_', '.', '~'.
// Everything else is percent-encoded with uppercase hex digits.
func awsURIEncode(s string, encodePath bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'):
			b.WriteByte(c)
		case c == '-' || c == '_' || c == '.' || c == '~':
			b.WriteByte(c)
		case c == '/' && encodePath:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			const hexDigits = "0123456789ABCDEF"
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0F])
		}
	}
	return b.String()
}

// readAndReplaceBody is a helper that drains req.Body, returns the bytes,
// and replaces req.Body with a fresh reader over the same bytes so the
// HTTP client can read it again on send. Use this when you need the body
// for signing but still want to send the request.
//
// Returns nil, nil if req.Body is nil (legitimate GET-style request).
func readAndReplaceBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	buf, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	req.Body = io.NopCloser(strings.NewReader(string(buf)))
	return buf, nil
}
