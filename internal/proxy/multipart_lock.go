// -------------------------------------------------------------------------------
// Multipart Manager - Advisory Lock Helpers
//
// Author: Alex Freidah
//
// Per-uploadID advisory lock ID derivation. Used by CompleteMultipartUpload
// so two concurrent Complete calls for the same upload cannot both stream
// parts and PUT the assembled object on top of each other. The namespace
// bit keeps these per-key locks above 2^62 so they cannot collide with the
// small reserved service lock IDs in store/core/locks.go.
// -------------------------------------------------------------------------------

package proxy

import "hash/fnv"

// uploadIDLockNamespace is OR'd into every multipart-upload advisory
// lock ID so per-uploadID locks live above 2^62 and cannot collide
// with the small reserved service lock IDs in core/locks.go.
const uploadIDLockNamespace int64 = 1 << 62

// uploadIDLockID derives a stable advisory-lock ID from a multipart
// upload ID. FNV-64a is fast and uniform; the namespace bit keeps the
// per-key range disjoint from the service lock IDs (1001-1011 today).
func uploadIDLockID(uploadID string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(uploadID))
	return uploadIDLockNamespace | int64(h.Sum64()&((1<<62)-1))
}
