// -------------------------------------------------------------------------------
// Auth - Object Key Authorization
//
// Author: Alex Freidah
//
// The one decision every surface that addresses objects by a bucket-qualified
// key makes before touching them: whether a user's grants carry a permission on
// the bucket the key names. The admin API and the dashboard both ask it here, so
// the two cannot drift into answering the same request differently.
// -------------------------------------------------------------------------------

package auth

import (
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// The reasons AuthorizeKey refuses with, for the caller's log and audit entry.
const (
	ReasonNoBucket   = "resource names no bucket"
	ReasonNoGrant    = "no grant on the bucket"
	ReasonPermission = "grant does not carry the permission"
)

// AuthorizeKey reports whether u may exercise want on the bucket key names, and
// returns that bucket for the caller's refusal record.
//
// The first slash-separated segment of key is the bucket. A key with no slash
// (the empty prefix, or a partial bucket name) spans every bucket it prefixes,
// so only the bucket wildcard can authorize it. A nil user reaches nothing.
func AuthorizeKey(u *User, key string, want core.PermissionSet) (bucket, reason string, ok bool) {
	bucket, _, found := strings.Cut(key, "/")
	if !found || bucket == "" {
		if u.AllBuckets().Has(want) {
			return "", "", true
		}
		return "", ReasonNoBucket, false
	}
	if !u.CanReach(bucket) {
		return bucket, ReasonNoGrant, false
	}
	if !u.Can(bucket, want) {
		return bucket, ReasonPermission, false
	}
	return bucket, "", true
}
