// -------------------------------------------------------------------------------
// Auth Test Builders
//
// Author: Alex Freidah
//
// Constructors this package's tests use to build users, sign requests and check
// signatures directly, without standing up a registry. Production builds users
// only in NewBucketRegistry and verifies signatures only through Authenticate.
// Requests are signed by the AWS SDK, so the verifier is checked against AWS's
// implementation rather than against its own helpers.
// -------------------------------------------------------------------------------

package auth

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/testutil/sigv4test"
)

// newUser builds a user holding the given bucket grants. A nil or absent entry
// means the user does not reach that bucket at all, which is a different answer
// from reaching it with no permissions.
func newUser(id, name string, grants map[string]core.PermissionSet) *User {
	return &User{ID: id, Name: name, grants: maps.Clone(grants)}
}

// withAllBuckets attaches what the user's bucket wildcard carries, which
// applies to every bucket including ones declared after the registry was built.
func (u *User) withAllBuckets(perms core.PermissionSet) *User {
	u.allBuckets = perms
	return u
}

// withAdmin attaches the control-plane grants the user holds, keyed on the
// resource each names.
func (u *User) withAdmin(admin map[core.Resource]core.PermissionSet) *User {
	u.admin = maps.Clone(admin)
	return u
}

// verifySigV4 checks the signature r carries, in its Authorization header or as
// a presigned URL, against secret.
func verifySigV4(r *http.Request, secret string) error {
	c := presignedClaim(r)
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, sigV4Prefix) {
		c = headerClaim(r, h)
	}
	_, err := c.verify(r, secret)
	return err
}

// signRequest builds a request for path, signed in its Authorization header by
// the AWS SDK.
func signRequest(tb testing.TB, method, path, accessKey, secret string) *http.Request {
	tb.Helper()
	r := httptest.NewRequestWithContext(context.Background(), method, path, http.NoBody)
	r.Host = "localhost"
	sigv4test.Sign(tb, r, accessKey, secret)
	return r
}

// presignRequest builds a GET for path as a presigned URL the AWS SDK signed
// at signedAt, carrying expires as its X-Amz-Expires.
func presignRequest(tb testing.TB, path, accessKey, secret, expires string, signedAt time.Time) *http.Request {
	tb.Helper()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, http.NoBody)
	r.Host = "localhost"
	q := r.URL.Query()
	q.Set("X-Amz-Expires", expires)
	r.URL.RawQuery = q.Encode()
	sigv4test.Presign(tb, r, accessKey, secret, signedAt)
	return r
}
