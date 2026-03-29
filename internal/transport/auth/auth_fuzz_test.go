// -------------------------------------------------------------------------------
// Auth Fuzz Tests - SigV4 Parsing and Canonicalization
//
// Author: Alex Freidah
//
// Fuzz tests for security-critical parsing functions in the auth package.
// Covers SigV4 header field extraction, canonical request construction, and
// presigned URL verification with adversarial inputs.
// -------------------------------------------------------------------------------

package auth

import (
	"net/http"
	"strings"
	"testing"
)

func FuzzParseSigV4Fields(f *testing.F) {
	f.Add("Credential=AKID/20260215/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=abcdef1234567890")
	f.Add("")
	f.Add("no-equals-sign")
	f.Add("Key=Value")
	f.Add("A=1, B=2, C=3")
	f.Add("Dup=first, Dup=second")
	f.Add("=empty-key")
	f.Add("empty-value=")
	f.Add(",,,")
	f.Add("embedded\nnewline=val")

	f.Fuzz(func(t *testing.T, input string) {
		result := parseSigV4Fields(input)
		if result == nil {
			t.Error("parseSigV4Fields returned nil")
		}

		// Every key in the map must be non-empty (parseSigV4Fields uses
		// idx > 0 as the guard, so empty keys should never be inserted).
		for k := range result {
			if k == "" {
				t.Error("parseSigV4Fields returned entry with empty key")
			}
		}

		// Cross-validate: parseSigV4FieldsDirect must agree with the
		// map-based parser for the three SigV4 fields.
		cred, sh, sig := parseSigV4FieldsDirect(input)
		if cred != result["Credential"] {
			t.Errorf("Credential mismatch: direct=%q map=%q", cred, result["Credential"])
		}
		if sh != result["SignedHeaders"] {
			t.Errorf("SignedHeaders mismatch: direct=%q map=%q", sh, result["SignedHeaders"])
		}
		if sig != result["Signature"] {
			t.Errorf("Signature mismatch: direct=%q map=%q", sig, result["Signature"])
		}
	})
}

func FuzzBuildCanonicalRequest(f *testing.F) {
	f.Add("GET", "/bucket/key", "param=value", "host;x-amz-date")
	f.Add("PUT", "/bucket/path/to/obj", "uploadId=abc&partNumber=3", "content-type;host;x-amz-date")
	f.Add("GET", "/", "", "host")
	f.Add("DELETE", "/bucket/key with spaces", "", "host;x-amz-date")
	f.Add("GET", "/bucket/%E4%B8%AD%E6%96%87", "prefix=%2F", "host")
	f.Add("GET", "///", "a=1&a=2", "host")
	f.Add("PUT", "/bucket/key", "x=%25already%25encoded", "host")
	f.Add("GET", "", "", "host")

	f.Fuzz(func(t *testing.T, method, path, rawQuery, signedHeadersStr string) {
		r, err := http.NewRequest(method, "/", nil)
		if err != nil {
			return
		}
		r.URL.Path = path
		r.URL.RawQuery = rawQuery
		r.Host = "localhost"
		r.Header.Set("X-Amz-Date", "20260307T000000Z")
		r.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
		r.Header.Set("Content-Type", "application/octet-stream")

		// Split signed headers, skip empty entries.
		var headers []string
		for h := range strings.SplitSeq(signedHeadersStr, ";") {
			if h != "" {
				headers = append(headers, h)
			}
		}
		if len(headers) == 0 {
			return
		}

		// Must not panic on any combination of method, path, query, and headers.
		result := buildCanonicalRequest(r, headers)

		// SigV4 canonical request structure: method, URI, query, one line
		// per header, blank line, signed headers, payload hash. Total
		// newlines = 3 (method+URI+query) + len(headers) + 1 (blank) + 1 (signed headers).
		expected := 3 + len(headers) + 1 + 1
		if n := strings.Count(result, "\n"); n != expected {
			t.Errorf("canonical request has %d newlines, want %d for %d headers", n, expected, len(headers))
		}
	})
}

// FuzzBuildPresignedCanonicalRequest exercises the presigned canonical request
// builder with adversarial query strings, ensuring X-Amz-Signature is always
// excluded and the function never panics.
func FuzzBuildPresignedCanonicalRequest(f *testing.F) {
	f.Add("GET", "/bucket/key", "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=abc123&other=value", "host")
	f.Add("PUT", "/bucket/obj", "X-Amz-Signature=&X-Amz-Date=20260326T000000Z", "host")
	f.Add("GET", "/", "", "host")
	f.Add("GET", "/bucket/key", "X-Amz-Signature=sig&X-Amz-Signature=sig2", "host")
	f.Add("DELETE", "/bucket/key with spaces", "X-Amz-Signature=abc", "host")

	f.Fuzz(func(t *testing.T, method, path, rawQuery, signedHeadersStr string) {
		r, err := http.NewRequest(method, "/", nil)
		if err != nil {
			return
		}
		r.URL.Path = path
		r.URL.RawQuery = rawQuery
		r.Host = "localhost"

		var headers []string
		for h := range strings.SplitSeq(signedHeadersStr, ";") {
			if h != "" {
				headers = append(headers, h)
			}
		}
		if len(headers) == 0 {
			return
		}

		result := buildPresignedCanonicalRequest(r, headers)

		// X-Amz-Signature must never appear in the canonical request.
		if strings.Contains(result, "X-Amz-Signature") {
			t.Error("presigned canonical request must not contain X-Amz-Signature")
		}
	})
}
