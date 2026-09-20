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

// WriteSeparator divides an object's internal key from the id of the write that
// produced the bytes stored under it. See StorageKey.
//
// '!' is in S3's own "safe characters" set, so it survives a key in a request
// path with no percent-encoding and needs no XML escaping in a listing - it has
// to, because a storage key is what the orchestrator hands a backend as its
// object key. It also sorts below every alphanumeric, which puts a key's
// per-write objects immediately after it in a raw bucket listing rather than
// scattered through the namespace, and that is what an operator reading a
// provider's console needs to see.
const WriteSeparator = "!"

// StorageKey returns the path on a backend that one write of objectKey stores
// its bytes at: the object's own key, the separator, and the id of the pending
// intent that write holds for this copy.
//
// Every write of a key used to land at the key itself, which is the whole of
// issue #1527: two overwrites racing towards one backend wrote the same path,
// so the cleanup that followed the loser could not tell its own bytes from the
// winner's and deleted whichever was there. A path that names the write makes
// every deletion unambiguous - a discard removes what it wrote and nothing else
// - and an overwrite stops being an in-place mutation, so a read during one
// sees either the old object or the new one rather than a half-written path.
//
// The intent id is 16 random bytes in hex and a storage key ends with one, so
// the last separator splits a storage key back into its parts even when the
// client's own key contains a '!'. Two writes therefore collide only by
// colliding on that id.
//
// Rows written before storage keys existed hold storage_key = object_key, which
// is exactly where their bytes are; nothing built here has to recognise them.
func StorageKey(objectKey, intentID string) string {
	return objectKey + WriteSeparator + intentID
}
