// -------------------------------------------------------------------------------
// Auth - Users and Assembly Notices
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

import "slices"

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

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

	buckets map[string]struct{}
}

// NewUser builds a user that reaches the named buckets.
func NewUser(id, name string, buckets []string) *User {
	u := &User{
		ID:      id,
		Name:    name,
		buckets: make(map[string]struct{}, len(buckets)),
	}
	for _, b := range buckets {
		u.buckets[b] = struct{}{}
	}
	return u
}

// StoredCredential is one keypair the store holds, alongside the user it proves.
// Several may name the same user, which is what lets a credential be replaced
// while its siblings keep working.
type StoredCredential struct {
	AccessKeyID string
	Secret      string
	User        *User
}

// CanReach reports whether this user holds a grant on the named bucket. A nil
// user reaches nothing, so a caller that failed to authenticate is refused
// rather than panicking on the check.
func (u *User) CanReach(bucket string) bool {
	if u == nil {
		return false
	}
	_, ok := u.buckets[bucket]
	return ok
}

// Buckets lists what this user reaches, sorted, which is what a ListBuckets
// response enumerates.
func (u *User) Buckets() []string {
	if u == nil {
		return nil
	}
	out := make([]string, 0, len(u.buckets))
	for name := range u.buckets {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// grant adds a bucket to what this user reaches.
func (u *User) grant(bucket string) {
	if u.buckets == nil {
		u.buckets = make(map[string]struct{}, 1)
	}
	u.buckets[bucket] = struct{}{}
}

// -------------------------------------------------------------------------
// NOTICES
// -------------------------------------------------------------------------

// NoticeCredentialShadowed and NoticeDanglingGrant are the kinds of thing
// assembly reports without refusing to serve.
const (
	NoticeCredentialShadowed = "credential_shadowed" //nolint:gosec // G101: a notice kind, not a credential
	NoticeDanglingGrant      = "dangling_grant"
)

// Notice is something assembly found that an operator should see and that does
// not stop the instance serving.
//
// A stored credential shadowed by a config one and a grant naming a bucket
// neither source declares are both states a running fleet can reach without
// anyone editing anything - a bucket leaves the config file, or a credential is
// added to it that the store already held - so they are reported rather than
// treated as a failure to start.
type Notice struct {
	Kind   string
	Detail string
}
