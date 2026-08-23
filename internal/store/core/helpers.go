// -------------------------------------------------------------------------------
// Core Orchestration Helpers
//
// Author: Alex Freidah
//
// Engine-agnostic helpers used by the transactional orchestration in this
// package. They operate on canonical core types only - never on engine row
// structs - so the same helper serves Postgres and SQLite paths.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/encryption"
)

// -------------------------------------------------------------------------
// PENDING-INTENT HELPERS
// -------------------------------------------------------------------------

// intentSuperseded reports whether any existing object_locations row was
// created after the pending intent. A newer row means a successful write
// happened later and is authoritative, so the intent is provably stale -
// dropping it avoids head-of-line blocking and prevents the reaper from
// corrupting metadata that a retry already committed.
func intentSuperseded(existing []ExistingCopy, intentCreatedAt time.Time) bool {
	for _, ec := range existing {
		if !ec.CreatedAt.IsZero() && ec.CreatedAt.After(intentCreatedAt) {
			return true
		}
	}
	return false
}

// ApplyStoredForm writes a description of an object's stored bytes onto a
// pending row. The inverse of pendingStoredForm: what an intent records here is
// what its promotion reads back, so a field missing from either side is a
// recovered object whose row contradicts its bytes.
func (p *PendingObject) ApplyStoredForm(form *StoredForm) {
	if form == nil {
		return
	}
	p.Encrypted = form.Encrypted
	p.EncryptionKey = form.EncryptionKey
	p.KeyID = form.KeyID
	p.PlaintextSize = form.PlaintextSize
	p.ContentHash = form.ContentHash
	p.CompressionAlgorithm = form.CompressionAlgorithm
	p.CompressionLevel = form.CompressionLevel
	p.CompressionFormatVersion = form.CompressionFormatVersion
	p.LogicalSize = form.LogicalSize
}

// pendingStoredForm builds a StoredForm from a PendingObject so the promoted
// object_locations row carries the same representation metadata as the
// original PUT recorded. Returns nil when the pending row describes bytes
// stored verbatim with no hash.
func pendingStoredForm(p *PendingObject) *StoredForm {
	if !p.Encrypted && p.ContentHash == "" && p.CompressionAlgorithm == "" {
		return nil
	}
	return &StoredForm{
		Encrypted:                p.Encrypted,
		EncryptionKey:            p.EncryptionKey,
		KeyID:                    p.KeyID,
		PlaintextSize:            p.PlaintextSize,
		ContentHash:              p.ContentHash,
		CompressionAlgorithm:     p.CompressionAlgorithm,
		CompressionLevel:         p.CompressionLevel,
		CompressionFormatVersion: p.CompressionFormatVersion,
		LogicalSize:              p.LogicalSize,
	}
}

// StoredFormFromLocation describes how a recorded copy's bytes are stored, so a
// path that moves those bytes verbatim can repeat that description on the row it
// writes for the destination. Returns nil when the row describes plaintext,
// unencoded, unhashed bytes, which is the same "nothing to carry" signal the
// write path uses.
func StoredFormFromLocation(loc *ObjectLocation) *StoredForm {
	if loc == nil || (!loc.Encrypted && loc.ContentHash == "" && loc.CompressionAlgorithm == "") {
		return nil
	}
	return &StoredForm{
		Encrypted:                loc.Encrypted,
		EncryptionKey:            loc.EncryptionKey,
		KeyID:                    loc.KeyID,
		PlaintextSize:            loc.PlaintextSize,
		ContentHash:              loc.ContentHash,
		CompressionAlgorithm:     loc.CompressionAlgorithm,
		CompressionLevel:         loc.CompressionLevel,
		CompressionFormatVersion: loc.CompressionFormatVersion,
		LogicalSize:              loc.LogicalSize,
	}
}

// objectFromStoredForm builds an ObjectLocation suitable for
// InsertObjectLocation from a key/backend/size triple plus the optional
// description of how the bytes are stored.
func objectFromStoredForm(key, backend string, size int64, form *StoredForm) *ObjectLocation {
	loc := &ObjectLocation{
		ObjectKey:   key,
		BackendName: backend,
		SizeBytes:   size,
	}
	if form == nil {
		return loc
	}
	if form.Encrypted {
		loc.Encrypted = true
		loc.EncryptionKey = form.EncryptionKey
		loc.KeyID = form.KeyID
		loc.PlaintextSize = form.PlaintextSize
	}
	if form.ContentHash != "" {
		loc.ContentHash = form.ContentHash
	}
	if form.CompressionAlgorithm != "" {
		loc.CompressionAlgorithm = form.CompressionAlgorithm
		loc.CompressionLevel = form.CompressionLevel
		loc.CompressionFormatVersion = form.CompressionFormatVersion
		loc.LogicalSize = form.LogicalSize
	}
	return loc
}

// -------------------------------------------------------------------------
// QUOTA DELTA APPLICATION
// -------------------------------------------------------------------------

// applyQuotaDeltas applies signed byte deltas to backend_quotas rows
// in stable backend_name order. The deterministic ordering prevents
// row-lock cycles: concurrent transactions touching the same backend
// set acquire locks in the same sequence and queue rather than
// deadlock. Negative deltas decrement (SQL clamps to zero); positive
// increment; zero is skipped so net-zero same-backend overwrites
// produce no SQL call.
func applyQuotaDeltas(ctx context.Context, tx TxAdapter, deltas map[string]int64) error {
	if len(deltas) == 0 {
		return nil
	}
	backends := make([]string, 0, len(deltas))
	for b := range deltas {
		backends = append(backends, b)
	}
	slices.Sort(backends)
	for _, b := range backends {
		d := deltas[b]
		switch {
		case d > 0:
			if err := tx.IncrementBackendQuota(ctx, b, d); err != nil {
				return err
			}
		case d < 0:
			if err := tx.DecrementBackendQuota(ctx, b, -d); err != nil {
				return err
			}
		}
	}
	return nil
}

// -------------------------------------------------------------------------
// COPY-DISPLACEMENT HELPER
// -------------------------------------------------------------------------

// displacedFromExisting filters an existing-copies slice down to the
// copies that need physical orphan cleanup after an overwrite to
// newBackend. The new PUT overwrites in place on newBackend, so a copy
// on that backend is replaced atomically; copies on every other backend
// become orphans.
func displacedFromExisting(existing []ExistingCopy, newBackend string) []DeletedCopy {
	if len(existing) == 0 {
		return nil
	}
	var displaced []DeletedCopy
	for _, ec := range existing {
		if ec.BackendName != newBackend {
			displaced = append(displaced, DeletedCopy{
				BackendName: ec.BackendName,
				SizeBytes:   ec.SizeBytes,
			})
		}
	}
	return displaced
}

// copySizeForBackend returns the SizeBytes of the copy held on backendName
// and true, or (0, false) when the locked re-read holds no copy there.
// Reading the size from the locked set rather than the caller's stale value
// keeps object_locations.size_bytes and backend_quotas.bytes_used in
// agreement across a concurrent overwrite.
func copySizeForBackend(existing []ExistingCopy, backendName string) (int64, bool) {
	for _, ec := range existing {
		if ec.BackendName == backendName {
			return ec.SizeBytes, true
		}
	}
	return 0, false
}

// isLastDecryptableCopy reports whether the copy on backendName is the only
// one still carrying the key needed to decrypt the object, while at least one
// sibling claims to be plaintext or has lost its key.
//
// Every copy of a key holds the same ciphertext under the same DEK, so a copy
// set that disagrees about encryption is already damaged. Dropping the row
// that still has the key makes the object permanently unreadable; dropping one
// of the others loses nothing. This reports the case worth refusing so the
// caller can skip the key and surface it rather than completing the loss.
func isLastDecryptableCopy(existing []ExistingCopy, backendName string) bool {
	var decryptable, target int
	for i := range existing {
		if existing[i].Encrypted && existing[i].HasDEK {
			decryptable++
			if existing[i].BackendName == backendName {
				target++
			}
		}
	}
	// Only a mixed set is suspicious: when every copy is decryptable (the
	// normal encrypted case) or none are (the normal plaintext case), the
	// caller's choice is arbitrary and safe.
	return decryptable == 1 && target == 1 && decryptable < len(existing)
}

// ValidateEncryptionMetadata reports whether a location row is self-consistent
// about encryption, so the read path can reject a copy it cannot serve
// correctly instead of returning the wrong bytes or the wrong size.
//
// A nil location is fine: callers that have no metadata row are serving
// unmanaged bytes and have nothing to contradict.
//
// The read path treats a failure here as a per-copy error, which fails over to
// a sibling copy; only an object whose every copy is inconsistent surfaces the
// error to the client.
func ValidateEncryptionMetadata(loc *ObjectLocation) error {
	if loc == nil {
		return nil
	}
	if !loc.Encrypted {
		// A cleared flag next to a surviving key is the signature of a row that
		// lost its encryption metadata: the bytes are almost certainly still an
		// envelope, and serving them as plaintext hands the client ciphertext.
		if len(loc.EncryptionKey) > 0 {
			return fmt.Errorf("%w: row is not encrypted but still carries a key", ErrEncryptionFlagMismatch)
		}
		return nil
	}
	if len(loc.EncryptionKey) == 0 {
		return fmt.Errorf("%w: row is encrypted but carries no key", ErrEncryptionFlagMismatch)
	}
	if loc.PlaintextSize < 0 {
		return fmt.Errorf("%w: row is encrypted but carries a negative plaintext size", ErrEncryptionFlagMismatch)
	}
	// Without a plaintext size there is no way to size the response or bound
	// range math, and the ciphertext size would be reported to the client.
	// Zero is a real size, though: an empty object encrypts to a bare header,
	// so only a row claiming no plaintext over a ciphertext large enough to
	// hold chunks has actually lost its size.
	if loc.PlaintextSize == 0 && loc.SizeBytes > int64(encryption.HeaderSize) {
		return fmt.Errorf("%w: row is encrypted but carries no plaintext size", ErrEncryptionFlagMismatch)
	}
	return nil
}
