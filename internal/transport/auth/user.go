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

import "slices"

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
