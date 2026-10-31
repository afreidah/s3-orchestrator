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

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/testutil/sigv4test"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// The root credential the harness declares and signs admin requests with.
const (
	adminAccessKey = "AKIAINTEGRATIONROOT"
	adminSecretKey = "integration-root-secret" //nolint:gosec // G101: test credential
)

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
	sigv4test.Sign(tb, req, adminAccessKey, adminSecretKey)
}
