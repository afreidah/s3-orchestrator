// -------------------------------------------------------------------------------
// Integration Test Signing Helpers
//
// Author: Alex Freidah
//
// The root credential the integration harness administers with, and the SigV4
// signing every admin request goes through. A signature is the only way into
// the admin API, so a test that wants to reach it signs for it.
// -------------------------------------------------------------------------------

//go:build integration

package integration

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

// The root credential the harness declares and signs admin requests with.
const (
	adminAccessKey = "AKIAINTEGRATIONROOT"
	adminSecretKey = "integration-root-secret" //nolint:gosec // G101: test credential
)

// signingRegion is the scope test signatures are built under. It only has to
// match between the signer and the verifier, both of which are in this process.
const signingRegion = "us-east-1"

// contentSHAHeader carries the payload hash the signature was built over.
const contentSHAHeader = "X-Amz-Content-Sha256"

// unsignedPayload is the SigV4 sentinel for a body left out of the signature.
const unsignedPayload = "UNSIGNED-PAYLOAD"

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// rootAuthConfig is the auth stanza the harness builds its registry from.
func rootAuthConfig() config.AuthConfig {
	return config.AuthConfig{
		Root: config.RootCredential{AccessKeyID: adminAccessKey, SecretAccessKey: adminSecretKey},
	}
}

// signAdmin signs an admin request with the root credential. Call it once the
// URL and query string are final, because both are covered by the signature.
func signAdmin(tb testing.TB, req *http.Request) {
	tb.Helper()
	req.Header.Set(contentSHAHeader, unsignedPayload)
	creds := aws.Credentials{AccessKeyID: adminAccessKey, SecretAccessKey: adminSecretKey}
	if err := v4.NewSigner().SignHTTP(
		req.Context(), creds, req, unsignedPayload, "s3", signingRegion, time.Now().UTC(),
	); err != nil {
		tb.Fatalf("sign admin request: %v", err)
	}
}
