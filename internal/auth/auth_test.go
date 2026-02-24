// -------------------------------------------------------------------------------
// Authentication Tests - SigV4, Token, and BucketRegistry Verification
//
// Author: Alex Freidah
//
// Unit tests for AWS SigV4 field parsing, canonical query construction, signing
// key derivation, and the BucketRegistry credential-to-bucket resolution for
// both SigV4 and legacy token methods.
// -------------------------------------------------------------------------------

package auth

import (
	"encoding/hex"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

func TestParseSigV4Fields(t *testing.T) {
	input := "Credential=AKID/20260215/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=abcdef1234567890"
	fields := parseSigV4Fields(input)

	if fields["Credential"] != "AKID/20260215/us-east-1/s3/aws4_request" {
		t.Errorf("Credential = %q", fields["Credential"])
	}
	if fields["SignedHeaders"] != "host;x-amz-date" {
		t.Errorf("SignedHeaders = %q", fields["SignedHeaders"])
	}
	if fields["Signature"] != "abcdef1234567890" {
		t.Errorf("Signature = %q", fields["Signature"])
	}
}

func TestBuildCanonicalQueryString(t *testing.T) {
	tests := []struct {
		name   string
		values url.Values
		want   string
	}{
		{
			name:   "empty",
			values: url.Values{},
			want:   "",
		},
		{
			name:   "single param",
			values: url.Values{"prefix": {"photos/"}},
			want:   "prefix=photos%2F",
		},
		{
			name:   "multiple params sorted",
			values: url.Values{"prefix": {"a"}, "delimiter": {"/"}, "max-keys": {"100"}},
			want:   "delimiter=%2F&max-keys=100&prefix=a",
		},
		{
			name:   "spaces encoded as %20 not +",
			values: url.Values{"prefix": {"my photos"}},
			want:   "prefix=my%20photos",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildCanonicalQueryString(tt.values)
			if got != tt.want {
				t.Errorf("buildCanonicalQueryString() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDeriveSigningKey(t *testing.T) {
	// AWS test vector from SigV4 documentation
	key := deriveSigningKey("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "20120215", "us-east-1", "iam")
	if len(key) != 32 {
		t.Errorf("signing key length = %d, want 32", len(key))
	}
}

func TestHmacSHA256(t *testing.T) {
	result := hmacSHA256([]byte("key"), []byte("data"))
	if len(result) != 32 {
		t.Errorf("hmacSHA256 result length = %d, want 32", len(result))
	}
}

func TestHashSHA256(t *testing.T) {
	// SHA256 of empty string
	got := hashSHA256([]byte(""))
	want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got != want {
		t.Errorf("hashSHA256('') = %q, want %q", got, want)
	}
}

func TestSigV4Encode(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"hello", "hello"},
		{"hello world", "hello%20world"},
		{"a+b", "a%2Bb"},
		{"a/b", "a%2Fb"},
	}
	for _, tt := range tests {
		got := sigV4Encode(tt.in)
		if got != tt.want {
			t.Errorf("sigV4Encode(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestEncodePath(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", "/"},
		{"/", "/"},
		{"/bucket/key", "/bucket/key"},
		{"/bucket/my file.txt", "/bucket/my%20file.txt"},
		{"/bucket/a+b/c d", "/bucket/a%2Bb/c%20d"},
		{"/bucket/path/to/special chars!@#", "/bucket/path/to/special%20chars%21%40%23"},
	}
	for _, tt := range tests {
		got := encodePath(tt.in)
		if got != tt.want {
			t.Errorf("encodePath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestVerifySigV4_StaleTimestamp(t *testing.T) {
	// A request signed with a timestamp 30 minutes in the past should be rejected
	staleDate := time.Now().UTC().Add(-30 * time.Minute).Format("20060102T150405Z")
	dateStamp := staleDate[:8]
	secret := "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	accessKey := "AKIDEXAMPLE"

	r, _ := http.NewRequest("GET", "/bucket/key", nil)
	r.Header.Set("X-Amz-Date", staleDate)
	r.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	r.Host = "localhost"

	// Build a valid signature so we test the timestamp check, not a sig mismatch
	signedHeaders := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	canonicalRequest := buildCanonicalRequest(r, signedHeaders)
	credentialScope := dateStamp + "/us-east-1/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + staleDate + "\n" + credentialScope + "\n" + hashSHA256([]byte(canonicalRequest))
	signingKey := deriveSigningKey(secret, dateStamp, "us-east-1", "s3")
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	r.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential="+accessKey+"/"+credentialScope+
			", SignedHeaders=host;x-amz-content-sha256;x-amz-date"+
			", Signature="+signature)

	err := VerifySigV4(r, accessKey, secret)
	if err == nil {
		t.Error("stale timestamp (30m) should be rejected")
	}
}

// -------------------------------------------------------------------------
// BUCKET REGISTRY TESTS
// -------------------------------------------------------------------------

// signRequest creates a valid SigV4-signed request for testing.
func signRequest(t *testing.T, method, path, accessKey, secret string) *http.Request {
	t.Helper()

	amzDate := time.Now().UTC().Format("20060102T150405Z")
	dateStamp := amzDate[:8]

	r, _ := http.NewRequest(method, path, nil)
	r.Header.Set("X-Amz-Date", amzDate)
	r.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	r.Host = "localhost"

	signedHeaders := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	canonicalRequest := buildCanonicalRequest(r, signedHeaders)
	credentialScope := dateStamp + "/us-east-1/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + credentialScope + "\n" + hashSHA256([]byte(canonicalRequest))
	signingKey := deriveSigningKey(secret, dateStamp, "us-east-1", "s3")
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	r.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential="+accessKey+"/"+credentialScope+
			", SignedHeaders=host;x-amz-content-sha256;x-amz-date"+
			", Signature="+signature)

	return r
}

func TestBucketRegistry_SigV4ResolvesCorrectBucket(t *testing.T) {
	buckets := []config.BucketConfig{
		{Name: "app1-files", Credentials: []config.CredentialConfig{
			{AccessKeyID: "APP1_KEY", SecretAccessKey: "APP1_SECRET"},
		}},
		{Name: "app2-files", Credentials: []config.CredentialConfig{
			{AccessKeyID: "APP2_KEY", SecretAccessKey: "APP2_SECRET"},
		}},
	}

	br := NewBucketRegistry(buckets)

	// Request signed with app1 credentials should resolve to app1-files
	r := signRequest(t, "GET", "/app1-files/test.txt", "APP1_KEY", "APP1_SECRET")
	bucket, err := br.AuthenticateAndResolveBucket(r)
	if err != nil {
		t.Fatalf("auth should succeed: %v", err)
	}
	if bucket != "app1-files" {
		t.Errorf("bucket = %q, want %q", bucket, "app1-files")
	}

	// Request signed with app2 credentials should resolve to app2-files
	r2 := signRequest(t, "GET", "/app2-files/test.txt", "APP2_KEY", "APP2_SECRET")
	bucket2, err := br.AuthenticateAndResolveBucket(r2)
	if err != nil {
		t.Fatalf("auth should succeed: %v", err)
	}
	if bucket2 != "app2-files" {
		t.Errorf("bucket = %q, want %q", bucket2, "app2-files")
	}
}

func TestBucketRegistry_TokenResolvesCorrectBucket(t *testing.T) {
	buckets := []config.BucketConfig{
		{Name: "legacy-bucket", Credentials: []config.CredentialConfig{
			{Token: "my-secret-token"},
		}},
	}

	br := NewBucketRegistry(buckets)

	r, _ := http.NewRequest("GET", "/legacy-bucket/key", nil)
	r.Header.Set("X-Proxy-Token", "my-secret-token")

	bucket, err := br.AuthenticateAndResolveBucket(r)
	if err != nil {
		t.Fatalf("token auth should succeed: %v", err)
	}
	if bucket != "legacy-bucket" {
		t.Errorf("bucket = %q, want %q", bucket, "legacy-bucket")
	}
}

func TestBucketRegistry_UnknownAccessKeyDenied(t *testing.T) {
	buckets := []config.BucketConfig{
		{Name: "mybucket", Credentials: []config.CredentialConfig{
			{AccessKeyID: "KNOWN_KEY", SecretAccessKey: "secret"},
		}},
	}

	br := NewBucketRegistry(buckets)

	r := signRequest(t, "GET", "/mybucket/key", "UNKNOWN_KEY", "secret")
	_, err := br.AuthenticateAndResolveBucket(r)
	if err == nil {
		t.Error("unknown access key should be denied")
	}
}

func TestBucketRegistry_InvalidTokenDenied(t *testing.T) {
	buckets := []config.BucketConfig{
		{Name: "mybucket", Credentials: []config.CredentialConfig{
			{Token: "correct-token"},
		}},
	}

	br := NewBucketRegistry(buckets)

	r, _ := http.NewRequest("GET", "/mybucket/key", nil)
	r.Header.Set("X-Proxy-Token", "wrong-token")

	_, err := br.AuthenticateAndResolveBucket(r)
	if err == nil {
		t.Error("invalid token should be denied")
	}
}

func TestBucketRegistry_NoCredentialsDenied(t *testing.T) {
	buckets := []config.BucketConfig{
		{Name: "mybucket", Credentials: []config.CredentialConfig{
			{AccessKeyID: "KEY", SecretAccessKey: "secret"},
		}},
	}

	br := NewBucketRegistry(buckets)

	r, _ := http.NewRequest("GET", "/mybucket/key", nil)
	_, err := br.AuthenticateAndResolveBucket(r)
	if err == nil {
		t.Error("request with no credentials should be denied")
	}
}

func TestBucketRegistry_MultipleCredsOnSameBucket(t *testing.T) {
	buckets := []config.BucketConfig{
		{Name: "shared-files", Credentials: []config.CredentialConfig{
			{AccessKeyID: "WRITER_KEY", SecretAccessKey: "WRITER_SECRET"},
			{AccessKeyID: "READER_KEY", SecretAccessKey: "READER_SECRET"},
		}},
	}

	br := NewBucketRegistry(buckets)

	// Both keys should resolve to the same bucket
	r1 := signRequest(t, "GET", "/shared-files/test.txt", "WRITER_KEY", "WRITER_SECRET")
	bucket1, err := br.AuthenticateAndResolveBucket(r1)
	if err != nil {
		t.Fatalf("writer auth should succeed: %v", err)
	}

	r2 := signRequest(t, "GET", "/shared-files/test.txt", "READER_KEY", "READER_SECRET")
	bucket2, err := br.AuthenticateAndResolveBucket(r2)
	if err != nil {
		t.Fatalf("reader auth should succeed: %v", err)
	}

	if bucket1 != "shared-files" || bucket2 != "shared-files" {
		t.Errorf("both creds should resolve to shared-files, got %q and %q", bucket1, bucket2)
	}
}

func TestBucketRegistry_WrongSecretDenied(t *testing.T) {
	buckets := []config.BucketConfig{
		{Name: "mybucket", Credentials: []config.CredentialConfig{
			{AccessKeyID: "KEY", SecretAccessKey: "correct-secret"},
		}},
	}

	br := NewBucketRegistry(buckets)

	// Sign with wrong secret — access key is known but signature won't match
	r := signRequest(t, "GET", "/mybucket/key", "KEY", "wrong-secret")
	_, err := br.AuthenticateAndResolveBucket(r)
	if err == nil {
		t.Error("wrong secret should be denied")
	}
}
