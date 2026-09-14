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
	"maps"
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

	grants     map[string]core.PermissionSet
	allBuckets core.PermissionSet
	admin      map[core.Resource]core.PermissionSet
}

// WithAllBuckets attaches what this user's bucket wildcard carries, which
// applies to every bucket including ones declared after the registry was built.
func (u *User) WithAllBuckets(perms core.PermissionSet) *User {
	u.allBuckets = perms
	return u
}

// AllBuckets reports what this user may do on any bucket, named or not. Zero
// means it holds no wildcard and reaches only the buckets it is granted.
//
// This is what authorizes an operation spanning the namespace rather than
// naming one bucket: the empty prefix is every bucket, and no per-bucket grant
// can answer for it.
func (u *User) AllBuckets() core.PermissionSet {
	if u == nil {
		return 0
	}
	return u.allBuckets
}

// NewUser builds a user holding the given bucket grants. A nil or absent entry
// means the user does not reach that bucket at all, which is a different answer
// from reaching it with no permissions.
//
// Control-plane grants are added with WithAdmin, so the S3 path constructs a
// user the same way it always has.
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

// WithAdmin attaches the control-plane grants this user holds, keyed on the
// resource each names.
func (u *User) WithAdmin(admin map[core.Resource]core.PermissionSet) *User {
	u.admin = maps.Clone(admin)
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
	if _, ok := u.grants[bucket]; ok {
		return true
	}
	return u.allBuckets != 0
}

// Can reports whether this user's grant on the named bucket carries every
// permission in want. An empty want is satisfied by any grant, which is what an
// operation needing no permission asks for.
// A named grant answers alone, without the wildcard unioned in, which is what
// lets broad access be carved down on one bucket: the publish-time expansion
// already replaced the wildcard for every bucket a grant names, and this keeps
// that same rule for the ones it does not.
func (u *User) Can(bucket string, want core.PermissionSet) bool {
	if u == nil {
		return false
	}
	if held, ok := u.grants[bucket]; ok {
		return held.Has(want)
	}
	return u.allBuckets != 0 && u.allBuckets.Has(want)
}

// Permissions reports what this user's grant on the named bucket carries, and
// whether it holds one at all.
func (u *User) Permissions(bucket string) (core.PermissionSet, bool) {
	if u == nil {
		return 0, false
	}
	if held, ok := u.grants[bucket]; ok {
		return held, true
	}
	return u.allBuckets, u.allBuckets != 0
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

// CanAdmin reports whether this user may act on the named control-plane
// resource. A nil user reaches nothing.
//
// The named grant is asked first and the kind's wildcard second, so a grant on
// one backend answers for that backend and a wildcard answers for every backend
// the deployment gains later. They union rather than the named one displacing
// the wildcard: a control-plane grant is written to widen, and there is no
// carve-out case the way a read-only bucket under a broad grant is.
//
// Nothing is granted implicitly. A user with no control-plane grant at all is
// refused here, which is what keeps an S3 credential out of the admin surface.
func (u *User) CanAdmin(resource core.Resource, want core.PermissionSet) bool {
	if u == nil {
		return false
	}
	held := u.admin[resource]
	if resource.Kind != core.ResourceInstance && !resource.IsWildcard() {
		held |= u.admin[core.Resource{Kind: resource.Kind, Name: core.ResourceWildcard}]
	}
	return held.Has(want)
}

// AdminPermissions reports what this user holds on the named resource, the
// kind's wildcard included, which is what an operator listing renders.
func (u *User) AdminPermissions(resource core.Resource) core.PermissionSet {
	if u == nil {
		return 0
	}
	held := u.admin[resource]
	if resource.Kind != core.ResourceInstance && !resource.IsWildcard() {
		held |= u.admin[core.Resource{Kind: resource.Kind, Name: core.ResourceWildcard}]
	}
	return held
}
