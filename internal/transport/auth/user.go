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

// User is the identity behind a credential. ID survives a rename and is what
// audit records name. FromConfig marks a user the config file declares, which
// the provisioning API refuses to modify. No secret lives here, since a user
// may hold several keypairs.
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

// AllBuckets reports what this user's bucket wildcard allows, which authorizes
// operations spanning the whole namespace. Zero means no wildcard.
func (u *User) AllBuckets() core.PermissionSet {
	if u == nil {
		return 0
	}
	return u.allBuckets
}

// NewUser builds a user holding the given bucket grants. A nil or absent entry
// means the user does not reach that bucket at all, which is a different answer
// from reaching it with no permissions. Control-plane grants are added with
// WithAdmin.
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
// kind. A nil user reaches nothing. It is separate from Can because the two
// refusals differ: no grant at all versus a grant missing a permission.
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
// permission in want. An empty want is satisfied by any grant. A named grant
// answers alone, without the wildcard unioned in, so a bucket-specific grant
// can narrow wildcard access on that bucket.
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
// response enumerates. A bucket is included when its grant carries
// PermListBuckets, even if the caller may not list its contents.
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

// CanAdmin reports whether this user may act on the control-plane resource, as
// the union of its named grant and the kind's wildcard. Nothing is implicit.
func (u *User) CanAdmin(resource core.Resource, want core.PermissionSet) bool {
	if u == nil {
		return false
	}
	held := u.admin[resource]
	if resource.Kind != core.ResourceOrchestrator && !resource.IsWildcard() {
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
	if resource.Kind != core.ResourceOrchestrator && !resource.IsWildcard() {
		held |= u.admin[core.Resource{Kind: resource.Kind, Name: core.ResourceWildcard}]
	}
	return held
}
