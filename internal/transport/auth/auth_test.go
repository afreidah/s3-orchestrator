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
	"fmt"
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

func TestParseSigV4Fields(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
			var b strings.Builder
			buildCanonicalQueryString(&b, tt.values)
			got := b.String()
			if got != tt.want {
				t.Errorf("buildCanonicalQueryString() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDeriveSigningKey(t *testing.T) {
	t.Parallel()
	// AWS test vector from SigV4 documentation
	key := deriveSigningKey("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "20120215", "us-east-1", "iam")
	if len(key) != 32 {
		t.Errorf("signing key length = %d, want 32", len(key))
	}
}

func TestHmacSHA256(t *testing.T) {
	t.Parallel()
	result := hmacSHA256([]byte("key"), []byte("data"))
	if len(result) != 32 {
		t.Errorf("hmacSHA256 result length = %d, want 32", len(result))
	}
}

func TestHashSHA256(t *testing.T) {
	t.Parallel()
	// SHA256 of empty string
	got := hashSHA256([]byte(""))
	want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got != want {
		t.Errorf("hashSHA256('') = %q, want %q", got, want)
	}
}

func TestSigningKeyCache(t *testing.T) {
	t.Parallel()
	secret := "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY" //nolint:gosec // G101: test credential
	accessKey := "CACHE_TEST_KEY"

	// First call: cache miss — derives and stores
	key1 := getCachedSigningKey(accessKey, secret, "20260312", "us-east-1", "s3", true)
	if len(key1) != 32 {
		t.Fatalf("signing key length = %d, want 32", len(key1))
	}

	// Second call: cache hit — same params
	key2 := getCachedSigningKey(accessKey, secret, "20260312", "us-east-1", "s3", true)
	if hex.EncodeToString(key1) != hex.EncodeToString(key2) {
		t.Error("cache hit should return identical key")
	}

	// Third call: stale cache — different dateStamp forces re-derive
	key3 := getCachedSigningKey(accessKey, secret, "20260313", "us-east-1", "s3", true)
	if hex.EncodeToString(key1) == hex.EncodeToString(key3) {
		t.Error("different dateStamp should produce a different key")
	}
}

func TestSigningKeyCache_UnknownKeysNotCached(t *testing.T) {
	t.Parallel()
	// Unknown keys (used with dummy secret for constant-time auth) must
	// not be cached to prevent memory exhaustion from randomized access
	// key IDs.
	for i := range 100 {
		fakeKey := fmt.Sprintf("UNKNOWN_%d", i)
		getCachedSigningKey(fakeKey, "dummy-secret", "20260328", "us-east-1", "s3", false)
	}

	// Verify none of the unknown keys were cached.
	cached := 0
	signingKeyCache.Range(func(key, _ any) bool {
		k := key.(string)
		for i := range 100 {
			if k == fmt.Sprintf("UNKNOWN_%d", i) {
				cached++
			}
		}
		return true
	})
	if cached > 0 {
		t.Errorf("found %d unknown keys in cache, want 0", cached)
	}
}

func TestSigV4Encode(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
		var b strings.Builder
		encodePath(&b, tt.in)
		got := b.String()
		if got != tt.want {
			t.Errorf("encodePath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestVerifySigV4_StaleTimestamp(t *testing.T) {
	t.Parallel()
	// A request signed with a timestamp 30 minutes in the past should be rejected
	staleDate := time.Now().UTC().Add(-30 * time.Minute).Format("20060102T150405Z")
	dateStamp := staleDate[:8]
	secret := "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY" //nolint:gosec // G101: test credential
	accessKey := "AKIDEXAMPLE"

	r, _ := http.NewRequestWithContext(context.Background(), "GET", "/bucket/key", nil)
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

func TestVerifySigV4_HostHeaderMustBeSigned(t *testing.T) {
	t.Parallel()
	secret := "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY" //nolint:gosec // G101: test credential
	accessKey := "AKIDEXAMPLE"
	amzDate := time.Now().UTC().Format("20060102T150405Z")
	dateStamp := amzDate[:8]

	r, _ := http.NewRequestWithContext(context.Background(), "GET", "/bucket/key", nil)
	r.Header.Set("X-Amz-Date", amzDate)
	r.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	r.Host = "localhost"

	// Sign with only x-amz-date and x-amz-content-sha256, deliberately omitting host
	signedHeaders := []string{"x-amz-content-sha256", "x-amz-date"}
	canonicalRequest := buildCanonicalRequest(r, signedHeaders)
	credentialScope := dateStamp + "/us-east-1/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + credentialScope + "\n" + hashSHA256([]byte(canonicalRequest))
	signingKey := deriveSigningKey(secret, dateStamp, "us-east-1", "s3")
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	r.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential="+accessKey+"/"+credentialScope+
			", SignedHeaders=x-amz-content-sha256;x-amz-date"+
			", Signature="+signature)

	err := VerifySigV4(r, accessKey, secret)
	if err == nil {
		t.Error("request without host in SignedHeaders should be rejected")
	}
}

func TestBucketRegistry_TokenAuthIteratesAllTokens(t *testing.T) {
	t.Parallel()
	// Verify that token auth works correctly with multiple tokens of varying lengths
	buckets := []config.BucketConfig{
		{Name: "bucket-a", Credentials: []config.CredentialConfig{
			{Token: "short"},
		}},
		{Name: "bucket-b", Credentials: []config.CredentialConfig{
			{Token: "medium-token"},
		}},
		{Name: "bucket-c", Credentials: []config.CredentialConfig{
			{Token: "a-very-long-token-value"},
		}},
	}

	br := NewBucketRegistry(buckets)

	// Each token should resolve to the correct bucket
	for _, tt := range []struct {
		token, wantBucket string
	}{
		{"short", "bucket-a"},
		{"medium-token", "bucket-b"},
		{"a-very-long-token-value", "bucket-c"},
	} {
		r, _ := http.NewRequestWithContext(context.Background(), "GET", "/"+tt.wantBucket+"/key", nil)
		r.Header.Set("X-Proxy-Token", tt.token)

		bucket, err := br.AuthenticateAndResolveBucket(r)
		if err != nil {
			t.Errorf("token %q should succeed: %v", tt.token, err)
			continue
		}
		if bucket != tt.wantBucket {
			t.Errorf("token %q: bucket = %q, want %q", tt.token, bucket, tt.wantBucket)
		}
	}

	// Wrong token should fail
	r, _ := http.NewRequestWithContext(context.Background(), "GET", "/bucket-a/key", nil)
	r.Header.Set("X-Proxy-Token", "wrong")
	_, err := br.AuthenticateAndResolveBucket(r)
	if err == nil {
		t.Error("wrong token should be denied")
	}

	// Token with same length as a valid token but different content should fail
	r2, _ := http.NewRequestWithContext(context.Background(), "GET", "/bucket-a/key", nil)
	r2.Header.Set("X-Proxy-Token", "SHORT") // same length as "short"
	_, err = br.AuthenticateAndResolveBucket(r2)
	if err == nil {
		t.Error("wrong token (same length) should be denied")
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

	r, _ := http.NewRequestWithContext(context.Background(), method, path, nil)
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
	t.Parallel()
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
	t.Parallel()
	buckets := []config.BucketConfig{
		{Name: "legacy-bucket", Credentials: []config.CredentialConfig{
			{Token: "my-secret-token"},
		}},
	}

	br := NewBucketRegistry(buckets)

	r, _ := http.NewRequestWithContext(context.Background(), "GET", "/legacy-bucket/key", nil)
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
	t.Parallel()
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
	t.Parallel()
	buckets := []config.BucketConfig{
		{Name: "mybucket", Credentials: []config.CredentialConfig{
			{Token: "correct-token"},
		}},
	}

	br := NewBucketRegistry(buckets)

	r, _ := http.NewRequestWithContext(context.Background(), "GET", "/mybucket/key", nil)
	r.Header.Set("X-Proxy-Token", "wrong-token")

	_, err := br.AuthenticateAndResolveBucket(r)
	if err == nil {
		t.Error("invalid token should be denied")
	}
}

func TestBucketRegistry_NoCredentialsDenied(t *testing.T) {
	t.Parallel()
	buckets := []config.BucketConfig{
		{Name: "mybucket", Credentials: []config.CredentialConfig{
			{AccessKeyID: "KEY", SecretAccessKey: "secret"},
		}},
	}

	br := NewBucketRegistry(buckets)

	r, _ := http.NewRequestWithContext(context.Background(), "GET", "/mybucket/key", nil)
	_, err := br.AuthenticateAndResolveBucket(r)
	if err == nil {
		t.Error("request with no credentials should be denied")
	}
}

func TestBucketRegistry_MultipleCredsOnSameBucket(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

func TestBucketRegistry_MaxMultipartUploads(t *testing.T) {
	t.Parallel()
	buckets := []config.BucketConfig{
		{Name: "limited", MaxMultipartUploads: 50, Credentials: []config.CredentialConfig{
			{AccessKeyID: "K1", SecretAccessKey: "S1"},
		}},
		{Name: "unlimited", Credentials: []config.CredentialConfig{
			{AccessKeyID: "K2", SecretAccessKey: "S2"},
		}},
	}

	br := NewBucketRegistry(buckets)

	if limit := br.MaxMultipartUploads("limited"); limit != 50 {
		t.Errorf("limited bucket limit = %d, want 50", limit)
	}
	if limit := br.MaxMultipartUploads("unlimited"); limit != 0 {
		t.Errorf("unlimited bucket limit = %d, want 0", limit)
	}
	if limit := br.MaxMultipartUploads("nonexistent"); limit != 0 {
		t.Errorf("nonexistent bucket limit = %d, want 0", limit)
	}
}

// -------------------------------------------------------------------------
// PRESIGNED URL TESTS
// -------------------------------------------------------------------------

// presignRequest creates a valid presigned URL request for testing. Auth
// credentials are placed in query parameters, not the Authorization header.
func presignRequest(t *testing.T, method, path, accessKey, secret string, expireSeconds int) *http.Request {
	t.Helper()

	amzDate := time.Now().UTC().Format("20060102T150405Z")
	dateStamp := amzDate[:8]
	credentialScope := dateStamp + "/us-east-1/s3/aws4_request"
	signedHeadersStr := "host"

	r, _ := http.NewRequestWithContext(context.Background(), method, path, nil)
	r.Host = "localhost"

	// Set the presigned query parameters (except Signature, computed below)
	q := r.URL.Query()
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", accessKey+"/"+credentialScope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", strconv.Itoa(expireSeconds))
	q.Set("X-Amz-SignedHeaders", signedHeadersStr)
	r.URL.RawQuery = q.Encode()

	// Build canonical request with all query params EXCEPT Signature
	signedHeaders := []string{"host"}
	canonicalRequest := buildPresignedCanonicalRequest(r, signedHeaders)
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + credentialScope + "\n" + hashSHA256([]byte(canonicalRequest))
	signingKey := deriveSigningKey(secret, dateStamp, "us-east-1", "s3")
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	// Now add the signature to the query string
	q.Set("X-Amz-Signature", signature)
	r.URL.RawQuery = q.Encode()

	return r
}

// TestBucketRegistry_PresignedResolvesCorrectBucket verifies that a valid
// presigned URL resolves to the correct virtual bucket.
func TestBucketRegistry_PresignedResolvesCorrectBucket(t *testing.T) {
	t.Parallel()
	buckets := []config.BucketConfig{
		{Name: "app1-files", Credentials: []config.CredentialConfig{
			{AccessKeyID: "APP1_KEY", SecretAccessKey: "APP1_SECRET"},
		}},
		{Name: "app2-files", Credentials: []config.CredentialConfig{
			{AccessKeyID: "APP2_KEY", SecretAccessKey: "APP2_SECRET"},
		}},
	}

	br := NewBucketRegistry(buckets)
	r := presignRequest(t, "GET", "/app1-files/test.txt", "APP1_KEY", "APP1_SECRET", 300)
	bucket, err := br.AuthenticateAndResolveBucket(r)
	if err != nil {
		t.Fatalf("presigned auth should succeed: %v", err)
	}
	if bucket != "app1-files" {
		t.Errorf("bucket = %q, want %q", bucket, "app1-files")
	}

	r2 := presignRequest(t, "GET", "/app2-files/other.txt", "APP2_KEY", "APP2_SECRET", 300)
	bucket2, err := br.AuthenticateAndResolveBucket(r2)
	if err != nil {
		t.Fatalf("presigned auth should succeed: %v", err)
	}
	if bucket2 != "app2-files" {
		t.Errorf("bucket = %q, want %q", bucket2, "app2-files")
	}
}

// TestPresigned_ExpiredURL verifies that a presigned URL whose date + expires
// window has passed is rejected.
func TestPresigned_ExpiredURL(t *testing.T) {
	t.Parallel()
	accessKey := "AKID"
	secret := "SECRET"
	dateStamp := time.Now().UTC().Add(-2 * time.Hour).Format("20060102T150405Z")
	ds := dateStamp[:8]
	credentialScope := ds + "/us-east-1/s3/aws4_request"

	r, _ := http.NewRequestWithContext(context.Background(), "GET", "/bucket/key", nil)
	r.Host = "localhost"

	q := r.URL.Query()
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", accessKey+"/"+credentialScope)
	q.Set("X-Amz-Date", dateStamp)
	q.Set("X-Amz-Expires", "3600") // 1 hour — expired 1 hour ago
	q.Set("X-Amz-SignedHeaders", "host")
	r.URL.RawQuery = q.Encode()

	canonicalRequest := buildPresignedCanonicalRequest(r, []string{"host"})
	stringToSign := "AWS4-HMAC-SHA256\n" + dateStamp + "\n" + credentialScope + "\n" + hashSHA256([]byte(canonicalRequest))
	signingKey := deriveSigningKey(secret, ds, "us-east-1", "s3")
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))
	q.Set("X-Amz-Signature", signature)
	r.URL.RawQuery = q.Encode()

	err := verifyPresignedSigV4(r, accessKey, secret, accessKey+"/"+credentialScope, "host", signature, dateStamp, "3600", true)
	if err == nil {
		t.Error("expired presigned URL should be rejected")
	}
}

// TestPresigned_ExcessiveExpiry verifies that X-Amz-Expires values exceeding
// the 7-day maximum are rejected.
func TestPresigned_ExcessiveExpiry(t *testing.T) {
	t.Parallel()
	r := presignRequest(t, "GET", "/bucket/key", "AKID", "SECRET", 604800+1)

	buckets := []config.BucketConfig{
		{Name: "bucket", Credentials: []config.CredentialConfig{
			{AccessKeyID: "AKID", SecretAccessKey: "SECRET"},
		}},
	}
	br := NewBucketRegistry(buckets)
	_, err := br.AuthenticateAndResolveBucket(r)
	if err == nil {
		t.Error("presigned URL with > 7 day expiry should be rejected")
	}
}

// TestPresigned_InvalidExpiry verifies that non-integer, zero, and negative
// X-Amz-Expires values are rejected.
func TestPresigned_InvalidExpiry(t *testing.T) {
	t.Parallel()
	for _, expires := range []string{"abc", "0", "-100", ""} {
		t.Run(expires, func(t *testing.T) {
			err := verifyPresignedSigV4(
				&http.Request{URL: &url.URL{}, Host: "localhost"},
				"AKID", "SECRET",
				"AKID/20260326/us-east-1/s3/aws4_request",
				"host",
				"fakesig",
				time.Now().UTC().Format("20060102T150405Z"),
				expires,
				true,
			)
			if err == nil {
				t.Errorf("expires=%q should be rejected", expires)
			}
		})
	}
}

// TestPresigned_TamperedSignature verifies that a presigned URL with a
// modified signature is rejected.
func TestPresigned_TamperedSignature(t *testing.T) {
	t.Parallel()
	r := presignRequest(t, "GET", "/bucket/key", "AKID", "SECRET", 300)

	// Tamper with the signature — replace entirely with zeros
	q := r.URL.Query()
	sig := q.Get("X-Amz-Signature")
	q.Set("X-Amz-Signature", strings.Repeat("0", len(sig)))
	r.URL.RawQuery = q.Encode()

	buckets := []config.BucketConfig{
		{Name: "bucket", Credentials: []config.CredentialConfig{
			{AccessKeyID: "AKID", SecretAccessKey: "SECRET"},
		}},
	}
	br := NewBucketRegistry(buckets)
	_, err := br.AuthenticateAndResolveBucket(r)
	if err == nil {
		t.Error("tampered presigned signature should be rejected")
	}
}

// TestPresigned_UnknownAccessKeyDenied verifies that a presigned URL with an
// unknown access key is rejected (constant-time path).
func TestPresigned_UnknownAccessKeyDenied(t *testing.T) {
	t.Parallel()
	r := presignRequest(t, "GET", "/bucket/key", "UNKNOWN_KEY", "SOME_SECRET", 300)

	buckets := []config.BucketConfig{
		{Name: "bucket", Credentials: []config.CredentialConfig{
			{AccessKeyID: "REAL_KEY", SecretAccessKey: "REAL_SECRET"},
		}},
	}
	br := NewBucketRegistry(buckets)
	_, err := br.AuthenticateAndResolveBucket(r)
	if err == nil {
		t.Error("unknown access key in presigned URL should be denied")
	}
}

// TestPresigned_WrongSecretDenied verifies that a presigned URL signed with
// the wrong secret is rejected.
func TestPresigned_WrongSecretDenied(t *testing.T) {
	t.Parallel()
	// Sign with "WRONG_SECRET" but register "REAL_SECRET"
	r := presignRequest(t, "GET", "/bucket/key", "AKID", "WRONG_SECRET", 300)

	buckets := []config.BucketConfig{
		{Name: "bucket", Credentials: []config.CredentialConfig{
			{AccessKeyID: "AKID", SecretAccessKey: "REAL_SECRET"},
		}},
	}
	br := NewBucketRegistry(buckets)
	_, err := br.AuthenticateAndResolveBucket(r)
	if err == nil {
		t.Error("presigned URL signed with wrong secret should be denied")
	}
}

// TestPresigned_HostHeaderMustBeSigned verifies that presigned URLs that do
// not include "host" in X-Amz-SignedHeaders are rejected.
func TestPresigned_HostHeaderMustBeSigned(t *testing.T) {
	t.Parallel()
	err := verifyPresignedSigV4(
		&http.Request{URL: &url.URL{}, Host: "localhost"},
		"AKID", "SECRET",
		"AKID/20260326/us-east-1/s3/aws4_request",
		"x-amz-date", // missing "host"
		"fakesig",
		time.Now().UTC().Format("20060102T150405Z"),
		"300",
		true,
	)
	if err == nil {
		t.Error("presigned URL without host in signed headers should be rejected")
	}
}

// TestPresigned_SignatureExcludedFromCanonicalQuery verifies that
// X-Amz-Signature is excluded from the canonical query string but other
// X-Amz-* parameters are included.
func TestPresigned_SignatureExcludedFromCanonicalQuery(t *testing.T) {
	t.Parallel()
	r, _ := http.NewRequestWithContext(context.Background(), "GET", "/bucket/key?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=abc123&other=value", nil)
	r.Host = "localhost"

	canonical := buildPresignedCanonicalRequest(r, []string{"host"})
	if strings.Contains(canonical, "X-Amz-Signature") {
		t.Error("canonical request should NOT contain X-Amz-Signature")
	}
	if !strings.Contains(canonical, "X-Amz-Algorithm") {
		t.Error("canonical request should contain X-Amz-Algorithm")
	}
	if !strings.Contains(canonical, "other=value") {
		t.Error("canonical request should contain non-auth query params")
	}
}

// TestPresigned_HeaderAndPresignedCoexist verifies that header-based SigV4
// and presigned URL auth both work correctly in the same BucketRegistry.
func TestPresigned_HeaderAndPresignedCoexist(t *testing.T) {
	t.Parallel()
	buckets := []config.BucketConfig{
		{Name: "bucket-a", Credentials: []config.CredentialConfig{
			{AccessKeyID: "KEY_A", SecretAccessKey: "SECRET_A"},
		}},
		{Name: "bucket-b", Credentials: []config.CredentialConfig{
			{AccessKeyID: "KEY_B", SecretAccessKey: "SECRET_B"},
		}},
	}
	br := NewBucketRegistry(buckets)

	// Header-based auth
	rHeader := signRequest(t, "GET", "/bucket-a/file.txt", "KEY_A", "SECRET_A")
	bucket, err := br.AuthenticateAndResolveBucket(rHeader)
	if err != nil {
		t.Fatalf("header auth should succeed: %v", err)
	}
	if bucket != "bucket-a" {
		t.Errorf("header auth bucket = %q, want %q", bucket, "bucket-a")
	}

	// Presigned URL auth
	rPresigned := presignRequest(t, "GET", "/bucket-b/file.txt", "KEY_B", "SECRET_B", 300)
	bucket, err = br.AuthenticateAndResolveBucket(rPresigned)
	if err != nil {
		t.Fatalf("presigned auth should succeed: %v", err)
	}
	if bucket != "bucket-b" {
		t.Errorf("presigned auth bucket = %q, want %q", bucket, "bucket-b")
	}
}

// TestPresigned_OverflowExpiry verifies that an expiry value large enough to
// overflow time.Duration is rejected before the multiplication.
func TestPresigned_OverflowExpiry(t *testing.T) {
	t.Parallel()
	err := verifyPresignedSigV4(
		&http.Request{URL: &url.URL{}, Host: "localhost"},
		"AKID", "SECRET",
		"AKID/20260329/us-east-1/s3/aws4_request",
		"host",
		"fakesig",
		time.Now().UTC().Format("20060102T150405Z"),
		"9999999999999", // would overflow time.Duration
		true,
	)
	if err == nil {
		t.Error("huge expiry should be rejected")
	}
}

// TestSigV4_CredentialDateMismatch verifies that a request where the
// credential scope date differs from X-Amz-Date is rejected.
func TestSigV4_CredentialDateMismatch(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	wrongDate := now.AddDate(0, 0, -1).Format("20060102") // yesterday

	err := verifySigV4Parsed(
		&http.Request{
			Method: "GET",
			URL:    &url.URL{Path: "/"},
			Host:   "localhost",
			Header: http.Header{
				"X-Amz-Date": {amzDate},
				"Host":       {"localhost"},
			},
		},
		"AKID", "SECRET",
		"AKID/"+wrongDate+"/us-east-1/s3/aws4_request",
		"host",
		"fakesig",
		true,
	)
	if err == nil {
		t.Error("credential date mismatch should be rejected")
	}
	if !strings.Contains(err.Error(), "credential date does not match") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestPresigned_CredentialDateMismatch verifies the same check for presigned URLs.
func TestPresigned_CredentialDateMismatch(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	wrongDate := now.AddDate(0, 0, -1).Format("20060102")

	err := verifyPresignedSigV4(
		&http.Request{URL: &url.URL{}, Host: "localhost"},
		"AKID", "SECRET",
		"AKID/"+wrongDate+"/us-east-1/s3/aws4_request",
		"host",
		"fakesig",
		amzDate,
		"300",
		true,
	)
	if err == nil {
		t.Error("presigned credential date mismatch should be rejected")
	}
	if !strings.Contains(err.Error(), "credential date does not match") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCollapseWhitespace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{"no change", "no change"},
		{"  leading", " leading"},
		{"trailing  ", "trailing "},
		{"a  b", "a b"},
		{"a\tb", "a b"},
		{"a\nb", "a b"},
		{"a\r\nb", "a b"},
		{"a \t \n b", "a b"},
		{"", ""},
		{"\n", " "},
		{"\t\t\t", " "},
	}
	for _, tt := range tests {
		if got := collapseWhitespace(tt.in); got != tt.want {
			t.Errorf("collapseWhitespace(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestStripWhitespace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{"host", "host"},
		{"x-amz-date", "x-amz-date"},
		{"0\n0", "00"},
		{"\n", ""},
		{"a b", "ab"},
		{"a\tb\nc\rd", "abcd"},
		{"", ""},
		{" host ", "host"},
	}
	for _, tt := range tests {
		if got := stripWhitespace(tt.in); got != tt.want {
			t.Errorf("stripWhitespace(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
