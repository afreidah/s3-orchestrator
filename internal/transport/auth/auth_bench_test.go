// -------------------------------------------------------------------------------
// Auth Benchmarks - SigV4 Verification Hot Path
//
// Author: Alex Freidah
//
// Pins the per-request cost of SigV4 verification: full VerifySigV4 with
// and without query params, the per-call deriveSigningKey HMAC chain, and
// the parser that splits the Authorization header. AuthenticateAndResolveBucket
// covers the full bucket-lookup path so credential-cache regressions
// surface here. Critical to keep stable because every request pays this
// cost.
// -------------------------------------------------------------------------------

package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// BenchmarkVerifySigV4 measures the verify sig v4 behaviour described by the test name.
func BenchmarkVerifySigV4(b *testing.B) {
	accessKey := "AKIDEXAMPLE"
	secret := "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY" //nolint:gosec // G101: benchmark credential
	r := signRequest(b, http.MethodGet, "/bucket/key", accessKey, secret)

	b.ResetTimer()
	for b.Loop() {
		_ = verifySigV4(r, secret)
	}
}

// BenchmarkDeriveSigningKey measures the derive signing key behaviour described by the test name.
func BenchmarkDeriveSigningKey(b *testing.B) {
	for b.Loop() {
		deriveSigningKey("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "20260215", "us-east-1", "s3")
	}
}

// BenchmarkParseSigV4Fields measures the parse sig v4 fields behaviour across the supplied sub-cases:
// "map", "direct".
// Each sub-case exercises one branch of the code under test.
func BenchmarkParseSigV4Fields(b *testing.B) {
	input := "Credential=AKID/20260215/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

	b.Run("map", func(b *testing.B) {
		for b.Loop() {
			parseSigV4Fields(input)
		}
	})

	b.Run("direct", func(b *testing.B) {
		for b.Loop() {
			parseSigV4FieldsDirect(input)
		}
	})
}

// BenchmarkAuthenticateAndResolveBucket measures the full per-request auth
// entry point including access key lookup, signing key cache hit, canonical
// request building, and signature verification.
func BenchmarkAuthenticateAndResolveBucket(b *testing.B) {
	accessKey := "AKIDEXAMPLE"
	secret := "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY" //nolint:gosec // G101: benchmark credential
	buckets := []config.BucketConfig{
		{Name: "bucket", Credentials: []config.CredentialConfig{
			{AccessKeyID: accessKey, SecretAccessKey: secret},
		}},
	}
	br := mustBucketRegistry(b, buckets)
	r := signRequest(b, http.MethodGet, "/bucket/key", accessKey, secret)

	b.ResetTimer()
	for b.Loop() {
		_, _, _ = br.Authenticate(r)
	}
}

// BenchmarkVerifySigV4_WithQueryParams measures the verify sig v4 with query params path by exercising q.Set, fmt.Sprintf, q.Encode.
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
	secret := "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY" //nolint:gosec // G101: benchmark credential

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			q := url.Values{}
			for i := range tc.params {
				q.Set(fmt.Sprintf("param%d", i), fmt.Sprintf("value%d", i))
			}
			r := signRequest(b, http.MethodGet, "/bucket/key?"+q.Encode(), accessKey, secret)

			b.ResetTimer()
			for b.Loop() {
				_ = verifySigV4(r, secret)
			}
		})
	}
}

// BenchmarkBuildCanonicalRequest measures the build canonical request path by exercising http.NewRequestWithContext, context.Background.
func BenchmarkBuildCanonicalRequest(b *testing.B) {
	r, _ := http.NewRequestWithContext(context.Background(), "PUT", "/bucket/path/to/object.bin?uploadId=abc123&partNumber=3", nil)
	r.Header.Set("X-Amz-Date", "20260307T000000Z")
	r.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	r.Header.Set("Content-Type", "application/octet-stream")
	r.Host = "s3.example.com"

	signedHeaders := []string{"content-type", "host", "x-amz-content-sha256", "x-amz-date"}

	for b.Loop() {
		(&sigV4Claim{}).canonicalRequest(r, signedHeaders)
	}
}

// BenchmarkVerifyPresignedSigV4 measures the verify presigned sig v4 path by exercising br.AuthenticateAndResolveBucket.
func BenchmarkVerifyPresignedSigV4(b *testing.B) {
	accessKey := "AKIDEXAMPLE"
	secret := "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY" //nolint:gosec // G101: benchmark credential

	buckets := []config.BucketConfig{
		{Name: "bucket", Credentials: []config.CredentialConfig{
			{AccessKeyID: accessKey, SecretAccessKey: secret},
		}},
	}
	br := mustBucketRegistry(b, buckets)
	r := presignRequest(b, "/bucket/key", accessKey, secret, "300", time.Now())

	b.ResetTimer()
	for b.Loop() {
		_, _, _ = br.Authenticate(r)
	}
}

// BenchmarkSecretAuth measures resolving a keypair presented whole, which is
// the dashboard's login path. The bucket count varies; a result that grows
// with it means the lookup has regressed from a map read to a scan.
func BenchmarkSecretAuth(b *testing.B) {
	for _, tc := range []struct {
		name  string
		count int
	}{
		{"1_bucket", 1},
		{"5_buckets", 5},
		{"20_buckets", 20},
	} {
		b.Run(tc.name, func(b *testing.B) {
			buckets := make([]config.BucketConfig, tc.count)
			for i := range tc.count {
				buckets[i] = config.BucketConfig{
					Name: fmt.Sprintf("bucket-%d", i),
					Credentials: []config.CredentialConfig{{
						AccessKeyID:     fmt.Sprintf("AKIA%028d", i),
						SecretAccessKey: fmt.Sprintf("secret-%032d", i),
					}},
				}
			}
			br := mustBucketRegistry(b, buckets)

			last := buckets[tc.count-1].Credentials[0]
			b.ResetTimer()
			for b.Loop() {
				_, _ = br.AuthenticateSecret(last.AccessKeyID, last.SecretAccessKey)
			}
		})
	}
}
