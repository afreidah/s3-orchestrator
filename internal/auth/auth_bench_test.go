package auth

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// benchSignedRequest constructs a valid SigV4-signed request for benchmarking.
func benchSignedRequest(accessKey, secret string) *http.Request {
	amzDate := time.Now().UTC().Format("20060102T150405Z")
	dateStamp := amzDate[:8]

	r, _ := http.NewRequest("GET", "/bucket/key", nil)
	r.Header.Set("X-Amz-Date", amzDate)
	r.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	r.Host = "localhost"

	signedHeaders := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	canonicalRequest := buildCanonicalRequest(r, signedHeaders)
	credentialScope := fmt.Sprintf("%s/us-east-1/s3/aws4_request", dateStamp)
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s", amzDate, credentialScope, hashSHA256([]byte(canonicalRequest)))
	signingKey := deriveSigningKey(secret, dateStamp, "us-east-1", "s3")
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	r.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential="+accessKey+"/"+credentialScope+
			", SignedHeaders=host;x-amz-content-sha256;x-amz-date"+
			", Signature="+signature)

	return r
}

func BenchmarkVerifySigV4(b *testing.B) {
	accessKey := "AKIDEXAMPLE"
	secret := "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	r := benchSignedRequest(accessKey, secret)

	b.ResetTimer()
	for b.Loop() {
		_ = VerifySigV4(r, accessKey, secret)
	}
}

func BenchmarkDeriveSigningKey(b *testing.B) {
	for b.Loop() {
		deriveSigningKey("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "20260215", "us-east-1", "s3")
	}
}

func BenchmarkParseSigV4Fields(b *testing.B) {
	input := "Credential=AKID/20260215/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

	for b.Loop() {
		parseSigV4Fields(input)
	}
}

func BenchmarkVerifySigV4_WithQueryParams(b *testing.B) {
	cases := []struct {
		name   string
		params int
	}{
		{"0_params", 0},
		{"5_params", 5},
		{"20_params", 20},
	}

	accessKey := "AKIDEXAMPLE"
	secret := "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			r := benchSignedRequest(accessKey, secret)
			q := r.URL.Query()
			for i := range tc.params {
				q.Set(fmt.Sprintf("param%d", i), fmt.Sprintf("value%d", i))
			}
			r.URL.RawQuery = q.Encode()

			// Re-sign with the query params included
			amzDate := r.Header.Get("X-Amz-Date")
			dateStamp := amzDate[:8]
			signedHeaders := []string{"host", "x-amz-content-sha256", "x-amz-date"}
			canonicalRequest := buildCanonicalRequest(r, signedHeaders)
			credentialScope := fmt.Sprintf("%s/us-east-1/s3/aws4_request", dateStamp)
			stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s", amzDate, credentialScope, hashSHA256([]byte(canonicalRequest)))
			signingKey := deriveSigningKey(secret, dateStamp, "us-east-1", "s3")
			signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))
			r.Header.Set("Authorization",
				"AWS4-HMAC-SHA256 Credential="+accessKey+"/"+credentialScope+
					", SignedHeaders=host;x-amz-content-sha256;x-amz-date"+
					", Signature="+signature)

			b.ResetTimer()
			for b.Loop() {
				_ = VerifySigV4(r, accessKey, secret)
			}
		})
	}
}

func BenchmarkBuildCanonicalRequest(b *testing.B) {
	r, _ := http.NewRequest("PUT", "/bucket/path/to/object.bin?uploadId=abc123&partNumber=3", nil)
	r.Header.Set("X-Amz-Date", "20260307T000000Z")
	r.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	r.Header.Set("Content-Type", "application/octet-stream")
	r.Host = "s3.example.com"

	signedHeaders := []string{"content-type", "host", "x-amz-content-sha256", "x-amz-date"}

	for b.Loop() {
		buildCanonicalRequest(r, signedHeaders)
	}
}

func BenchmarkTokenAuth(b *testing.B) {
	tokens := []struct {
		name  string
		count int
	}{
		{"1_bucket", 1},
		{"5_buckets", 5},
		{"20_buckets", 20},
	}

	for _, tc := range tokens {
		b.Run(tc.name, func(b *testing.B) {
			buckets := make([]config.BucketConfig, tc.count)
			for i := range tc.count {
				buckets[i] = config.BucketConfig{
					Name: fmt.Sprintf("bucket-%d", i),
					Credentials: []config.CredentialConfig{{
						Token: fmt.Sprintf("token-%032d", i),
					}},
				}
			}
			br := NewBucketRegistry(buckets)

			// Use the last token so the loop iterates all entries
			lastToken := buckets[tc.count-1].Credentials[0].Token
			r, _ := http.NewRequest("GET", "/bucket/key", nil)
			r.Header.Set("X-Proxy-Token", lastToken)

			b.ResetTimer()
			for b.Loop() {
				_, _ = br.AuthenticateAndResolveBucket(r)
			}
		})
	}
}
