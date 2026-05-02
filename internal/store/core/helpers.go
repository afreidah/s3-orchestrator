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

import "time"

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

// pendingEncryptionMeta builds an EncryptionMeta from a PendingObject so
// the promoted object_locations row carries the same encryption + integrity
// metadata as the original PUT recorded. Returns nil when the pending row
// carries no encryption or hash metadata.
func pendingEncryptionMeta(p *PendingObject) *EncryptionMeta {
	if !p.Encrypted && p.ContentHash == "" {
		return nil
	}
	return &EncryptionMeta{
		Encrypted:     p.Encrypted,
		EncryptionKey: p.EncryptionKey,
		KeyID:         p.KeyID,
		PlaintextSize: p.PlaintextSize,
		ContentHash:   p.ContentHash,
	}
}

// objectFromEnc builds an ObjectLocation suitable for InsertObjectLocation
// from a key/backend/size triple plus optional encryption metadata.
func objectFromEnc(key, backend string, size int64, enc *EncryptionMeta) *ObjectLocation {
	loc := &ObjectLocation{
		ObjectKey:   key,
		BackendName: backend,
		SizeBytes:   size,
	}
	if enc == nil {
		return loc
	}
	if enc.Encrypted {
		loc.Encrypted = true
		loc.EncryptionKey = enc.EncryptionKey
		loc.KeyID = enc.KeyID
		loc.PlaintextSize = enc.PlaintextSize
	}
	if enc.ContentHash != "" {
		loc.ContentHash = enc.ContentHash
	}
	return loc
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
