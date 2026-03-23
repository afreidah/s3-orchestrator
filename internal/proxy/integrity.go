// -------------------------------------------------------------------------------
// Integrity Verification - Content Hashing and Verification
//
// Author: Alex Freidah
//
// SHA-256 content hashing for object integrity verification. HashBody computes
// the digest at write time. VerifyingReader wraps a response body and computes
// the hash as data streams through, allowing verification at EOF without
// buffering the entire object.
// -------------------------------------------------------------------------------

package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
)

// HashBody computes the SHA-256 hex digest of a byte slice.
func HashBody(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// VerifyingReader wraps an io.ReadCloser and computes SHA-256 as data is read.
// After the underlying reader returns EOF, call Verify to check the hash.
type VerifyingReader struct {
	inner      io.ReadCloser
	hasher     hash.Hash
	expected   string                        // expected hex digest (empty = skip)
	onMismatch func(expected, actual string) // called on Close if hash doesn't match
}

// NewVerifyingReader wraps r with a streaming SHA-256 computation.
func NewVerifyingReader(r io.ReadCloser) *VerifyingReader {
	return &VerifyingReader{
		inner:  r,
		hasher: sha256.New(),
	}
}

// Read implements io.Reader. Data passes through to the caller while being
// hashed incrementally.
func (vr *VerifyingReader) Read(p []byte) (int, error) {
	n, err := vr.inner.Read(p)
	if n > 0 {
		_, _ = vr.hasher.Write(p[:n])
	}
	return n, err
}

// Close closes the underlying reader. If an OnMismatch callback is set
// and verification fails, it is called before returning.
func (vr *VerifyingReader) Close() error {
	err := vr.inner.Close()
	if vr.expected != "" && vr.onMismatch != nil {
		actual := hex.EncodeToString(vr.hasher.Sum(nil))
		if actual != vr.expected {
			vr.onMismatch(vr.expected, actual)
		}
	}
	return err
}

// SetVerification configures the reader to check the hash on Close and
// call onMismatch if the digest doesn't match. This allows the caller
// to trigger cleanup of corrupted copies after streaming completes.
func (vr *VerifyingReader) SetVerification(expected string, onMismatch func(expected, actual string)) {
	vr.expected = expected
	vr.onMismatch = onMismatch
}

// Verify checks the computed hash against the expected hex digest.
// Returns nil if they match, or an error describing the mismatch.
// Returns nil if expected is empty (object has no stored hash).
func (vr *VerifyingReader) Verify(expected string) error {
	if expected == "" {
		return nil
	}
	actual := hex.EncodeToString(vr.hasher.Sum(nil))
	if actual != expected {
		return fmt.Errorf("integrity check failed: expected %s, got %s", expected, actual)
	}
	return nil
}
