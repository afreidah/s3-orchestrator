// -------------------------------------------------------------------------------
// SigV4 Path Encoding Round-Trip Tests
//
// Author: Alex Freidah
//
// Pins the canonical-URI handling against the real AWS Go SDK signer. The
// verifier accepts any signature the SDK produces across the adversarial
// key set: keys containing %2F, %25, spaces, +, =, ?, #, Unicode, and
// mixed-case unreserved characters all round-trip cleanly.
//
// The path-substitution test pins the security guarantee that two distinct
// wire URLs whose decoded paths happen to be identical produce different
// canonical requests, so an upstream proxy or attacker cannot substitute
// one path encoding for another and have the verifier accept it.
// -------------------------------------------------------------------------------

package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// TestSigV4_RoundTripEncodedKey signs a PUT for each adversarial key with
// the real AWS SDK signer and asserts the orchestrator's verifier accepts
// the signature. Each adversarial case fires the lossy-path bug (verifier
// rejects with SignatureDoesNotMatch); the baseline case is sanity that
// the test scaffolding itself is correct.
func TestSigV4_RoundTripEncodedKey(t *testing.T) {
	t.Parallel()

	const (
		bucket    = "my-bucket"
		accessKey = "AKID-test"
		secretKey = "SECRET-test"
		region    = "us-east-1"
		service   = "s3"
	)

	cases := []struct {
		name string
		// rawKey is the wire-form of the key (post-URL-encoding). The SDK
		// signs the request as the URL appears on the wire, so the test
		// builds the raw URL directly to control the encoding.
		rawKey string
	}{
		{"baseline_simple", "simple.txt"},
		{"mixed_case_unreserved", "MixedCASE-key_with.tilde~end-Bytes"},
		{"encoded_slash", "foo%2Fbar.txt"},
		{"literal_percent", "key%25percent"},
		{"space_encoded", "key%20with%20space"},
		{"plus_literal", "key%2Bplus"},
		{"equals_in_path", "key%3Deq"},
		{"question_in_path", "key%3Fq"},
		{"hash_in_path", "key%23h"},
		{"unicode_jp", "%E6%97%A5%E6%9C%AC%E8%AA%9E"},
	}

	// DisableURIPathEscaping=true matches S3 mode (the AWS SDK middleware
	// sets this for every S3 request). With it false the SDK double-
	// encodes the wire path before signing, which is correct for non-S3
	// services but would not exercise the S3 verifier path under test.
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	creds := aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secretKey}
	now := time.Now().UTC()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Build the request directly so r.URL.RawPath carries the
			// adversarial encoding. http.NewRequest('s URL parser
			// preserves RawPath when re-encoding would change the shape.
			rawURL := "http://orch.test/" + bucket + "/" + tc.rawKey
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, rawURL, strings.NewReader("body"))
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			// The SDK signs Content-Length via SignedHeaders. The
			// verifier reads it through r.Header.Get, so the value
			// must live on the header map (req.ContentLength alone
			// does not satisfy that read).
			req.ContentLength = int64(len("body"))
			req.Header.Set("Content-Length", "4")

			payloadHash := hashSHA256Bytes([]byte("body"))
			req.Header.Set("X-Amz-Content-Sha256", payloadHash)

			if err := signer.SignHTTP(context.Background(), creds, req, payloadHash, service, region, now); err != nil {
				t.Fatalf("SignHTTP: %v", err)
			}

			if err := VerifySigV4(req, accessKey, secretKey); err != nil {
				t.Fatalf("VerifySigV4 rejected an SDK-signed request for key %q: %v", tc.rawKey, err)
			}
		})
	}
}

// TestSigV4_PathSubstitution_NotAccepted asserts that a request whose
// signature was computed for one wire path cannot be silently accepted
// after an upstream substitution to a different wire path that happens to
// decode to the same r.URL.Path. Today the verifier collapses both shapes
// to the same canonical request and accepts the substitution; after the
// fix the canonical request reflects the actual wire form so the
// substituted request fails verification.
func TestSigV4_PathSubstitution_NotAccepted(t *testing.T) {
	t.Parallel()

	const (
		bucket    = "my-bucket"
		accessKey = "AKID-test"
		secretKey = "SECRET-test"
		region    = "us-east-1"
		service   = "s3"
	)

	// DisableURIPathEscaping=true matches S3 mode (the AWS SDK middleware
	// sets this for every S3 request). With it false the SDK double-
	// encodes the wire path before signing, which is correct for non-S3
	// services but would not exercise the S3 verifier path under test.
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	creds := aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secretKey}
	now := time.Now().UTC()
	payloadHash := hashSHA256Bytes([]byte("body"))

	// Sign the encoded form: literal %2F as a key character.
	signedURL := "http://orch.test/" + bucket + "/foo%2Fbar"
	signedReq, err := http.NewRequestWithContext(context.Background(), http.MethodPut, signedURL, strings.NewReader("body"))
	if err != nil {
		t.Fatalf("NewRequest signed: %v", err)
	}
	signedReq.ContentLength = int64(len("body"))
	signedReq.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if err := signer.SignHTTP(context.Background(), creds, signedReq, payloadHash, service, region, now); err != nil {
		t.Fatalf("SignHTTP: %v", err)
	}

	// Substitute the path to its decoded form (literal `/`) but copy the
	// signature headers verbatim, simulating an upstream proxy that
	// normalised %2F to / after the client signed.
	substitutedURL := "http://orch.test/" + bucket + "/foo/bar"
	substitutedReq, err := http.NewRequestWithContext(context.Background(), http.MethodPut, substitutedURL, strings.NewReader("body"))
	if err != nil {
		t.Fatalf("NewRequest substituted: %v", err)
	}
	substitutedReq.ContentLength = int64(len("body"))
	for k, vs := range signedReq.Header {
		for _, v := range vs {
			substitutedReq.Header.Set(k, v)
		}
	}

	if err := VerifySigV4(substitutedReq, accessKey, secretKey); err == nil {
		t.Fatal("VerifySigV4 accepted a path-substituted request; signature must reflect the wire form, not the decoded path")
	}
}

// hashSHA256Bytes returns the lowercase-hex SHA-256 digest of b. Local to
// this file so the test does not depend on the package's hashSHA256 (which
// takes a string and is exercised separately).
func hashSHA256Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
