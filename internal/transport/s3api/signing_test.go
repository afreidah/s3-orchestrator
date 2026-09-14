// -------------------------------------------------------------------------------
// S3 API Test Signing Helpers
//
// Author: Alex Freidah
//
// One place for the credential the fixture registries in this package are
// built from and the SigV4 signing every test request goes through. Tests
// construct a request, hand it here, and send it; the signature is the only
// way in, so the helper has to agree with the server on the payload hash.
//
// The payload is declared unsigned so a test can attach a body of any size
// without the helper buffering it to compute a hash.
// -------------------------------------------------------------------------------

package s3api

import (
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// testAccessKeyID is the access key every fixture registry in this package
// grants, and the one signRequest signs with.
const testAccessKeyID = "AKIAS3APITESTKEY"

// testSecretKey is the secret half of that credential.
const testSecretKey = "s3api-test-secret-key" //nolint:gosec // G101: test credential

// signingRegion is the scope test signatures are built under. It only has to
// match between the signer and the verifier, both of which are in-process.
const signingRegion = "us-east-1"

// contentSHAHeader carries the payload hash the signature was built over.
const contentSHAHeader = "X-Amz-Content-Sha256"

// unsignedPayload is the SigV4 sentinel for a body left out of the signature.
const unsignedPayload = "UNSIGNED-PAYLOAD"

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// testCredential is the keypair fixture registries are built from.
func testCredential() config.CredentialConfig {
	return config.CredentialConfig{AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretKey}
}

// signRequest signs req with the shared test credential. Call it once the URL
// and query string are final, because both are covered by the signature.
func signRequest(tb testing.TB, req *http.Request) {
	tb.Helper()
	signRequestAs(tb, req, testAccessKeyID, testSecretKey)
}

// signRequestAs signs req with an arbitrary keypair, for the tests that prove
// one credential cannot reach another credential's bucket.
func signRequestAs(tb testing.TB, req *http.Request, accessKeyID, secretKey string) {
	tb.Helper()
	req.Header.Set(contentSHAHeader, unsignedPayload)
	creds := aws.Credentials{AccessKeyID: accessKeyID, SecretAccessKey: secretKey}
	if err := v4.NewSigner().SignHTTP(
		req.Context(), creds, req, unsignedPayload, "s3", signingRegion, time.Now().UTC(),
	); err != nil {
		tb.Fatalf("sign request: %v", err)
	}
}
