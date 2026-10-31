// -------------------------------------------------------------------------------
// SigV4 Test Signing
//
// Author: Alex Freidah
//
// Signs test requests with the AWS SDK's SigV4 signer, so every package that
// verifies a signature is checked against AWS's implementation rather than
// against its own. The payload is declared unsigned, so a test can attach a
// body of any size without the signer buffering it to compute a hash.
// -------------------------------------------------------------------------------

package sigv4test

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// Region is the scope test signatures are built under. It only has to match
// between the signer and the verifier, both of which are in the test process.
const Region = "us-east-1"

// UnsignedPayload is the SigV4 sentinel for a body left out of the signature.
const UnsignedPayload = "UNSIGNED-PAYLOAD"

// Sign signs req in its Authorization header with the given keypair. Call it
// once the URL and query string are final, because both are covered.
func Sign(tb testing.TB, req *http.Request, accessKey, secret string) {
	tb.Helper()
	SignAt(tb, req, accessKey, secret, time.Now())
}

// SignAt is Sign as of at, for a test that needs a stale signature. It sets
// Content-Length by hand: the verifier reads the header, which Go's server adds
// for a real request but an in-process one lacks.
func SignAt(tb testing.TB, req *http.Request, accessKey, secret string, at time.Time) {
	tb.Helper()
	if req.ContentLength > 0 {
		req.Header.Set("Content-Length", strconv.FormatInt(req.ContentLength, 10))
	}
	req.Header.Set("X-Amz-Content-Sha256", UnsignedPayload)
	creds := aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secret}
	if err := v4.NewSigner().SignHTTP(req.Context(), creds, req, UnsignedPayload, "s3", Region, at.UTC()); err != nil {
		tb.Fatalf("sign request: %v", err)
	}
}

// Presign replaces req's URL with a presigned URL signed as of at. Set
// X-Amz-Expires on the query first; the signer covers whatever it holds.
func Presign(tb testing.TB, req *http.Request, accessKey, secret string, at time.Time) {
	tb.Helper()
	creds := aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secret}
	uri, _, err := v4.NewSigner().PresignHTTP(req.Context(), creds, req, UnsignedPayload, "s3", Region, at.UTC())
	if err != nil {
		tb.Fatalf("presign request: %v", err)
	}
	if req.URL, err = url.Parse(uri); err != nil {
		tb.Fatalf("parse presigned URL: %v", err)
	}
}
