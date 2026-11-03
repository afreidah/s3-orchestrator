// -------------------------------------------------------------------------------
// Auth - Object Key Authorization
//
// Author: Alex Freidah
//
// The one decision every surface that addresses objects by a bucket-qualified
// key makes before touching them: whether a user's grants carry a permission on
// the bucket the key names. The S3 API, the admin API and the dashboard all ask
// it here, so they cannot drift into answering the same request differently.
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
	reason, ok = AuthorizeBucket(u, bucket, want)
	return bucket, reason, ok
}

// AuthorizeBucket reports whether u may exercise want on bucket, and why not.
// No grant at all and a grant missing a permission are refused separately, so
// an operator reading the refusal knows which one to fix.
func AuthorizeBucket(u *User, bucket string, want core.PermissionSet) (reason string, ok bool) {
	held, reached := u.Permissions(bucket)
	if !reached {
		return ReasonNoGrant, false
	}
	if !held.Has(want) {
		return ReasonPermission, false
	}
	return "", true
}
