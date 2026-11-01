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
// produced the bytes stored under it. See StorageKey. '!' is in S3's safe
// character set, so it survives a request path unescaped, and it sorts below
// every alphanumeric, keeping a key's per-write objects together in a listing.
const WriteSeparator = "!"

// StorageKey returns the path on a backend that one write of objectKey stores
// its bytes at: the object's own key, the separator, and the id of the pending
// intent that write holds for this copy. Because each write has its own path,
// a cleanup never deletes another write's bytes and an overwrite never
// modifies a path a reader may be reading. The intent id is hex, so the last
// separator splits a storage key even when the client's key contains a '!'.
func StorageKey(objectKey, intentID string) string {
	return CopyPrefix(objectKey) + intentID
}

// CopyPrefix is what every per-write path of objectKey starts with. A
// different object can share it, such as a client's "photo.jpg!backup" beside
// "photo.jpg", so a match on the prefix alone is not a copy; IsCopyOf is.
func CopyPrefix(objectKey string) string {
	return objectKey + WriteSeparator
}

// writeIDLength is the number of characters after the separator in a per-write
// path: the intent id, 16 random bytes written as 32 lowercase hex characters.
const writeIDLength = 32

// IsCopyOf reports whether path holds bytes of objectKey: the key itself, or
// one of its per-write paths, which is the key, the separator, then exactly 32
// lowercase hex characters.
func IsCopyOf(objectKey, path string) bool {
	if path == objectKey {
		return true
	}
	id, ok := strings.CutPrefix(path, CopyPrefix(objectKey))
	if !ok || len(id) != writeIDLength {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
