// -------------------------------------------------------------------------------
// Auth Test Builders
//
// Author: Alex Freidah
//
// Constructors this package's tests use to build users and check signatures
// directly, without standing up a registry. Production builds users only in
// NewBucketRegistry and verifies signatures only through Authenticate.
// -------------------------------------------------------------------------------

package auth

import (
	"errors"
	"maps"
	"net/http"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
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

// verifySigV4 checks an AWS Signature Version 4 Authorization header against a
// secret, splitting the header the way the request path does. Returns nil if
// the signature is valid.
func verifySigV4(r *http.Request, secretAccessKey string) error {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return errors.New("missing Authorization header")
	}
	if !strings.HasPrefix(authHeader, sigV4Prefix) {
		return errors.New("unsupported auth scheme")
	}
	credential, signedHeadersStr, signature := parseSigV4FieldsDirect(authHeader[len(sigV4Prefix):])
	if credential == "" || signedHeadersStr == "" || signature == "" {
		return errors.New("malformed Authorization header")
	}
	_, err := verifySigV4Parsed(r, secretAccessKey, credential, signedHeadersStr, signature)
	return err
}
