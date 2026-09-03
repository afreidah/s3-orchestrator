// -------------------------------------------------------------------------------
// Internal Key - Bucket / User-Key Namespacing Helpers
//
// Author: Alex Freidah
//
// Storage backends and the metadata DB use a flat key space; user-facing
// buckets are layered on top by prefixing every object key with
// "bucket/userkey". These helpers centralize the convention so adding a new
// namespacing scheme later (e.g. tenant scoping) only touches one package.
// -------------------------------------------------------------------------------

package internalkey

import "strings"

// Separator is the delimiter between the bucket name and the user-facing
// object key inside an internal storage key.
const Separator = "/"

// Make returns the internal storage key for a (bucket, userKey) pair.
func Make(bucket, userKey string) string {
	return bucket + Separator + userKey
}

// Prefix returns the bucket-scoped prefix used for listing and reconcile
// scans (i.e. "bucket/").
func Prefix(bucket string) string {
	return bucket + Separator
}

// Split parses an internal key into its bucket and user-facing key. When the
// key has no separator, bucket holds the entire input and userKey is empty.
func Split(internalKey string) (bucket, userKey string) {
	bucket, userKey, _ = strings.Cut(internalKey, Separator)
	return bucket, userKey
}
