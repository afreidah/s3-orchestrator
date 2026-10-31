// -------------------------------------------------------------------------------
// Admin API Test Signing Helpers
//
// Author: Alex Freidah
//
// One place for the credentials the handler tests present and the SigV4 signing
// every admin request now goes through. A signature is the only way in, so a
// test that wants to reach a route signs for it.
//
// The payload is declared unsigned, because the server canonicalises whatever
// the request declares and these tests are about the decision the guard reaches
// rather than about body coverage. The real client's body hashing is covered
// end to end in signed_test.go.
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/testutil/sigv4test"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// The root credential the handler tests administer with.
const (
	rootAccessKey = "AKIAADMINTESTROOT"
	rootSecret    = "admin-test-root-secret" //nolint:gosec // G101: test credential
)

// The credential the grant tests authenticate with, holding only what a test
// grants it.
const (
	grantedAccessKey = "AKIAADMINTESTGRANT"
	grantedSecret    = "admin-test-grant-secret" //nolint:gosec // G101: test credential
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// signRoot signs an already-built request with the root credential. Call it
// once the URL and query string are final, because both are covered.
func signRoot(tb testing.TB, req *http.Request) {
	tb.Helper()
	sigv4test.Sign(tb, req, rootAccessKey, rootSecret)
}

// doSigned builds a request signed with the given keypair.
func doSigned(tb testing.TB, accessKey, secret, method, path, body string) *http.Request {
	tb.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
	sigv4test.Sign(tb, req, accessKey, secret)
	return req
}

// doRoot is doSigned with the root credential.
func doRoot(tb testing.TB, method, path, body string) *http.Request {
	tb.Helper()
	return doSigned(tb, rootAccessKey, rootSecret, method, path, body)
}
