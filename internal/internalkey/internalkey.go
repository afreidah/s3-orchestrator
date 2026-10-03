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
// A storage key is what the orchestrator hands a backend as its object key, so
// the separator has to survive a request path and a listing unescaped. '!' is
// in S3's "safe characters" set, and it sorts below every alphanumeric, which
// keeps a key's per-write objects together in a raw bucket listing.
const WriteSeparator = "!"

// StorageKey returns the path on a backend that one write of objectKey stores
// its bytes at: the object's own key, the separator, and the id of the pending
// intent that write holds for this copy.
//
// Because each write has its own path, a cleanup deletes exactly the bytes its
// write uploaded and never another write's, and an overwrite never modifies a
// path a reader may be reading.
//
// The intent id is 16 random bytes in hex, so the last separator splits a
// storage key back into its parts even when the client's key contains a '!'.
// A row whose storage key equals its object key keeps its bytes at the key;
// nothing here has to treat it specially.
func StorageKey(objectKey, intentID string) string {
	return objectKey + WriteSeparator + intentID
}
