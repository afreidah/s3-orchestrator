// -------------------------------------------------------------------------------
// S3 API Test Signing Helpers
//
// Author: Alex Freidah
//
// One place for the credential the fixture registries in this package are
// built from and the SigV4 signing every test request goes through. Tests
// construct a request, hand it here, and send it; the signature is the only
// way in.
// -------------------------------------------------------------------------------

package s3api

import (
	"net/http"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/testutil/sigv4test"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// testAccessKeyID is the access key every fixture registry in this package
// grants, and the one signRequest signs with.
const testAccessKeyID = "AKIAS3APITESTKEY"

// testSecretKey is the secret half of that credential.
const testSecretKey = "s3api-test-secret-key" //nolint:gosec // G101: test credential

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
	sigv4test.Sign(tb, req, testAccessKeyID, testSecretKey)
}
