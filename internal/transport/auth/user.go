// -------------------------------------------------------------------------------
// Auth - The Identity Behind a Credential
//
// Author: Alex Freidah
//
// The identity a request authenticates as. A credential proves a caller is one
// user, and the user's bucket set says which buckets that one reaches.
// Credentials the config file declares and credentials the store holds both
// resolve to this shape, so the request path has one answer to give whichever
// source declared them.
// -------------------------------------------------------------------------------

package auth

import (
	"slices"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// User is the identity behind a credential.
//
// ID survives a rename and is what an audit record names, so neither rotating a
// credential nor renaming a user breaks the trail. FromConfig marks a user the
// config file declares, which the provisioning API refuses to modify.
//
// No secret lives here: a user may hold several keypairs, and the secret belongs
// to the one that proved the request.
type User struct {
	ID         string
	Name       string
	FromConfig bool

	grants map[string]core.PermissionSet
}

// NewUser builds a user holding the given grants. A nil or absent entry means
// the user does not reach that bucket at all, which is a different answer from
// reaching it with no permissions.
func NewUser(id, name string, grants map[string]core.PermissionSet) *User {
	u := &User{
		ID:     id,
		Name:   name,
		grants: make(map[string]core.PermissionSet, len(grants)),
	}
	for bucket, perms := range grants {
		u.grants[bucket] = perms
	}
	return u
}

// CanReach reports whether this user holds a grant on the named bucket, of any
// kind. A nil user reaches nothing, so a caller that failed to authenticate is
// refused rather than panicking on the check.
//
// Kept separate from Can because the two refusals mean different things: no
// grant is a bucket the caller cannot see, and a grant missing a permission is
// one it can see and may not act on this way.
func (u *User) CanReach(bucket string) bool {
	if u == nil {
		return false
	}
	_, ok := u.grants[bucket]
	return ok
}

// Can reports whether this user's grant on the named bucket carries every
// permission in want. An empty want is satisfied by any grant, which is what an
// operation needing no permission asks for.
func (u *User) Can(bucket string, want core.PermissionSet) bool {
	if u == nil {
		return false
	}
	held, ok := u.grants[bucket]
	return ok && held.Has(want)
}

// Permissions reports what this user's grant on the named bucket carries, and
// whether it holds one at all.
func (u *User) Permissions(bucket string) (core.PermissionSet, bool) {
	if u == nil {
		return 0, false
	}
	held, ok := u.grants[bucket]
	return held, ok
}

// Buckets lists what this user reaches, sorted, which is what a ListBuckets
// response enumerates.
//
// A bucket the caller may not list the contents of is still named, because
// ListBuckets answers which buckets exist for this caller rather than what it
// may do inside them; PermListBuckets is what gates the entry itself.
func (u *User) Buckets() []string {
	if u == nil {
		return nil
	}
	out := make([]string, 0, len(u.grants))
	for name, perms := range u.grants {
		if perms.Has(core.PermListBuckets) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}
