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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
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

// signRoot signs an already-built request with the root credential.
func signRoot(tb testing.TB, req *http.Request) {
	tb.Helper()
	signAs(tb, req, rootAccessKey, rootSecret)
}

// signAs signs an already-built request with an arbitrary keypair. Call it once
// the URL and query string are final, because both are covered.
//
// Content-Length is written into the header set because the signer covers it
// from the field while the verifier reads it from the header. Over the wire the
// two always agree - Go's server puts the header back - but these requests never
// travel, so the header has to be put there by hand.
func signAs(tb testing.TB, req *http.Request, accessKey, secret string) {
	tb.Helper()
	if req.ContentLength > 0 {
		req.Header.Set("Content-Length", strconv.FormatInt(req.ContentLength, 10))
	}
	req.Header.Set(contentSHAHeader, unsignedPayload)
	creds := aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secret}
	if err := v4.NewSigner().SignHTTP(
		req.Context(), creds, req, unsignedPayload, "s3", signingRegion, time.Now().UTC(),
	); err != nil {
		tb.Fatalf("sign request: %v", err)
	}
}

// doSigned builds a request signed with the given keypair.
func doSigned(tb testing.TB, accessKey, secret, method, path, body string) *http.Request {
	tb.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
	signAs(tb, req, accessKey, secret)
	return req
}

// doRoot is doSigned with the root credential.
func doRoot(tb testing.TB, method, path, body string) *http.Request {
	tb.Helper()
	return doSigned(tb, rootAccessKey, rootSecret, method, path, body)
}
