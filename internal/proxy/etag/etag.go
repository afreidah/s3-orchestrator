// -------------------------------------------------------------------------------
// ETag - S3 Entity Tags for Objects This Orchestrator Wrote
//
// Author: Alex Freidah
//
// S3 defines the ETag of a single-part upload as the MD5 of the object's bytes
// and the ETag of a multipart upload as the MD5 of the concatenated binary
// part digests, suffixed with the part count. Clients compare both against
// locally computed digests, so the algorithm is the contract and not an
// implementation choice.
//
// The digests are always of the bytes the client sent. What lands on a backend
// may be compressed, encrypted or both, and the backend's own ETag describes
// that stored form instead - which is why an object's ETag is computed here
// and stored, rather than read back from whichever copy answers.
//
// MD5 is used because S3 specifies it. It is not a security control here: the
// integrity scrubber verifies stored bytes with SHA-256.
// -------------------------------------------------------------------------------

package etag

import (
	"crypto/md5" //nolint:gosec // G501: MD5 is the S3 ETag algorithm, not a security control
	"encoding/hex"
	"fmt"
	"hash"
	"strconv"
	"strings"
)

// NewHasher returns the hasher an ETag is accumulated in. Callers tee the
// bytes the client sent into it during the pass that already reads them.
func NewHasher() hash.Hash {
	return md5.New() //nolint:gosec // G401: see the package comment
}

// Hex returns the digest accumulated in h, or empty when h is nil so a caller
// that never hashed does not have to branch.
func Hex(h hash.Hash) string {
	if h == nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Single renders the ETag of a whole-object write. Quoted, because that is the
// form S3 puts on the wire and the form clients compare against.
func Single(digestHex string) string {
	if digestHex == "" {
		return ""
	}
	return `"` + digestHex + `"`
}

// Multipart renders the ETag of a completed multipart upload: the MD5 of the
// concatenated binary part digests, then a dash and the number of parts. The
// digests must be in ascending part order, which is the order the client's
// completion manifest lists them in and the order S3 assembles them in.
//
// Returns an empty string when any part has no digest. That happens for an
// upload whose parts predate per-part digests, and the caller falls back to
// the whole-object MD5 rather than publishing a composite over a subset.
func Multipart(partDigestsHex []string) (string, error) {
	if len(partDigestsHex) == 0 {
		return "", nil
	}
	sum := md5.New() //nolint:gosec // G401: see the package comment
	for _, d := range partDigestsHex {
		if d == "" {
			return "", nil
		}
		raw, err := hex.DecodeString(d)
		if err != nil {
			return "", fmt.Errorf("decode part digest %q: %w", d, err)
		}
		sum.Write(raw)
	}
	return `"` + hex.EncodeToString(sum.Sum(nil)) + "-" + strconv.Itoa(len(partDigestsHex)) + `"`, nil
}

// Normalize strips the quotes and the weak-comparison prefix so a value a
// client quoted matches a stored one that is not (or the reverse).
func Normalize(v string) string {
	return strings.Trim(strings.TrimPrefix(v, "W/"), `"`)
}
