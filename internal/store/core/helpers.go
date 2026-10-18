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
	"fmt"
	"slices"
	"strconv"
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

// Described returns a copy of the form, or nil when it describes plaintext,
// unencoded, unhashed bytes, which is the "nothing to carry" signal the write
// path uses.
func (f *StoredForm) Described() *StoredForm {
	if !f.Form().Any(StoredCompressed | StoredEncrypted | StoredHashed) {
		return nil
	}
	c := *f
	return &c
}

// ObjectFromStoredForm builds an ObjectLocation for InsertObjectLocation. form
// and id are optional; a nil id leaves the identity columns NULL, as for a
// write that never learned the object's ETag. storageKey names the write that
// placed the bytes and cannot be derived from key.
func ObjectFromStoredForm(key, backend, storageKey string, size int64, form *StoredForm, id *ObjectIdentity) *ObjectLocation {
	loc := &ObjectLocation{
		ObjectKey:   key,
		BackendName: backend,
		StorageKey:  StoragePath(key, storageKey),
		SizeBytes:   size,
		Identity:    id,
	}
	if form != nil {
		loc.StoredForm = *form
	}
	return loc
}

// StoragePath returns storageKey, or objectKey when storageKey is empty, which
// is where backfilled and imported objects keep their bytes. It also keeps an
// empty string out of the column, which the (backend_name, storage_key) unique
// index would reject on the second such copy.
func StoragePath(objectKey, storageKey string) string {
	if storageKey == "" {
		return objectKey
	}
	return storageKey
}

// PartPath returns the path a multipart upload's part is stored at. A part is
// not an object any client can name, so the path is also the object key its
// cleanup rows carry.
func PartPath(uploadID string, partNumber int) string {
	return "__multipart/" + uploadID + "/" + strconv.Itoa(partNumber)
}

// -------------------------------------------------------------------------
// COPY-REMOVAL HELPER
// -------------------------------------------------------------------------

// removedFromExisting returns the bytes a removed copy set leaves to delete.
// Every copy goes, including those on backends an overwrite lands on, because
// the new write uses its own path and the old bytes stay behind.
func removedFromExisting(existing []ExistingCopy) []removedCopy {
	if len(existing) == 0 {
		return nil
	}
	removed := make([]removedCopy, 0, len(existing))
	for _, ec := range existing {
		removed = append(removed, removedCopy{
			BackendName: ec.BackendName,
			StorageKey:  ec.StorageKey,
			SizeBytes:   ec.SizeBytes,
		})
	}
	return removed
}

// LocationOn returns the copy in locs held on backendName, or nil when there is
// none. An object holds at most one copy per backend.
func LocationOn(locs []ObjectLocation, backendName string) *ObjectLocation {
	for i := range locs {
		if locs[i].BackendName == backendName {
			return &locs[i]
		}
	}
	return nil
}

// copyOnBackend returns the copy held on backendName in the locked re-read, or
// false when there is none. Callers must take size and storage key from this
// copy, not their own stale values: a concurrent overwrite would otherwise
// skew bytes_used or delete a newer write's bytes.
func copyOnBackend(existing []ExistingCopy, backendName string) (ExistingCopy, bool) {
	for _, ec := range existing {
		if ec.BackendName == backendName {
			return ec, true
		}
	}
	return ExistingCopy{}, false
}

// isLastDecryptableCopy reports whether the copy on backendName is the only
// one still carrying the object's DEK while some sibling is plaintext or has
// lost its key. Such a set is already damaged, and dropping this row would
// make the object permanently unreadable, so callers refuse it.
func isLastDecryptableCopy(existing []ExistingCopy, backendName string) bool {
	var decryptable, target int
	for i := range existing {
		if existing[i].Form.Has(StoredReadable) {
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

// ClientLocations narrows a copy lookup to the copies a client may see,
// dropping unmanaged rows. A key left with none is reported as
// ErrObjectNotFound, so a client asking for it gets NoSuchKey rather than a
// failed read. Takes the lookup's results directly so a call site can wrap it.
func ClientLocations(locs []ObjectLocation, err error) ([]ObjectLocation, error) {
	if err != nil {
		return nil, err
	}
	visible := slices.DeleteFunc(locs, func(loc ObjectLocation) bool { return loc.Unmanaged })
	if len(visible) == 0 {
		return nil, ErrObjectNotFound
	}
	return visible, nil
}

// ValidateEncryptionMetadata reports whether a location row is self-consistent
// about encryption, so the read path can reject a copy instead of serving the
// wrong bytes or size. A nil location passes. The read path treats a failure
// as a per-copy error and fails over to a sibling copy.
func ValidateEncryptionMetadata(loc *ObjectLocation) error {
	if loc == nil {
		return nil
	}
	form := loc.Form()
	if !form.Has(StoredEncrypted) {
		// A cleared flag next to a surviving key is the signature of a row that
		// lost its encryption metadata: the bytes are almost certainly still an
		// envelope, and serving them as plaintext hands the client ciphertext.
		if form.Has(StoredKeyed) {
			return fmt.Errorf("%w: row is not encrypted but still carries a key", ErrEncryptionFlagMismatch)
		}
		return nil
	}
	if form.Unreadable() {
		return fmt.Errorf("%w: row is encrypted but carries no key", ErrEncryptionFlagMismatch)
	}
	if loc.PlaintextSize < 0 {
		return fmt.Errorf("%w: row is encrypted but carries a negative plaintext size", ErrEncryptionFlagMismatch)
	}
	// An empty object encrypts to a bare header, so a zero plaintext size is
	// only lost when the ciphertext is large enough to hold chunks.
	if loc.PlaintextSize == 0 && loc.SizeBytes > int64(encryption.HeaderSize) {
		return fmt.Errorf("%w: row is encrypted but carries no plaintext size", ErrEncryptionFlagMismatch)
	}
	return nil
}
