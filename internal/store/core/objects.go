// -------------------------------------------------------------------------------
// Core Object Location Orchestration
//
// Author: Alex Freidah
//
// Engine-agnostic transactional logic for object_locations: recording new
// objects, removing old ones, atomic moves between backends, and import of
// pre-existing data. Each operation is a sequence of TxAdapter calls
// composed inside a single transaction so the Postgres and SQLite paths
// share one implementation.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"fmt"
)

// -------------------------------------------------------------------------
// RECORD OBJECT
// -------------------------------------------------------------------------

// RecordObject records an object's location and updates the backend
// quota. On overwrite, all existing copies (including replicas) are
// removed and their quotas decremented before inserting the new
// primary copy. Returns the displaced copies for cleanup.
func RecordObject(ctx context.Context, runner Runner, key, backend string, size int64, form *StoredForm) ([]DeletedCopy, error) {
	return WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) ([]DeletedCopy, error) {
		return recordObjectTx(ctx, tx, key, backend, size, form, "")
	})
}

// RecordObjectAndClearPending performs the same atomic commit as
// RecordObject and additionally deletes the matching pending_objects
// intent inside the same transaction. The write path uses this on a
// successful PUT so the intent never outlives a committed location.
func RecordObjectAndClearPending(ctx context.Context, runner Runner, key, backend string, size int64, form *StoredForm, intentID string) ([]DeletedCopy, error) {
	return WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) ([]DeletedCopy, error) {
		return recordObjectTx(ctx, tx, key, backend, size, form, intentID)
	})
}

// recordObjectTx is the shared transactional body. When intentID is
// non-empty the same transaction also deletes the corresponding
// pending row. All per-backend quota deltas are aggregated and applied
// in stable backend_name order via applyQuotaDeltas so
// concurrent overwrites can never deadlock on backend_quotas row locks.
func recordObjectTx(ctx context.Context, tx TxAdapter, key, backend string, size int64, form *StoredForm, intentID string) ([]DeletedCopy, error) {
	if err := tx.AcquireKeyLock(ctx, key); err != nil {
		return nil, err
	}
	existing, err := tx.GetExistingCopiesForUpdate(ctx, key)
	if err != nil {
		return nil, err
	}
	deltas := make(map[string]int64, len(existing)+1)
	displaced, err := clearExistingCopies(ctx, tx, key, backend, existing, deltas)
	if err != nil {
		return nil, err
	}
	if err := tx.InsertObjectLocation(ctx, objectFromStoredForm(key, backend, size, form)); err != nil {
		return nil, fmt.Errorf("insert object location: %w", err)
	}
	deltas[backend] += size
	if err := applyQuotaDeltas(ctx, tx, deltas); err != nil {
		return nil, err
	}
	if intentID != "" {
		if err := tx.DeletePending(ctx, intentID); err != nil {
			return nil, fmt.Errorf("clear pending intent: %w", err)
		}
	}
	return displaced, nil
}

// -------------------------------------------------------------------------
// DELETE OBJECT
// -------------------------------------------------------------------------

// DeleteObject removes all copies of an object and decrements their
// quotas. Returns ErrObjectNotFound if the object doesn't exist;
// otherwise returns the deleted copies for cleanup. Quota deltas apply
// in stable backend_name order.
func DeleteObject(ctx context.Context, runner Runner, key string) ([]DeletedCopy, error) {
	return WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) ([]DeletedCopy, error) {
		existing, err := tx.GetExistingCopiesForUpdate(ctx, key)
		if err != nil {
			return nil, err
		}
		if len(existing) == 0 {
			return nil, ErrObjectNotFound
		}
		if err := tx.DeleteObjectCopies(ctx, key); err != nil {
			return nil, fmt.Errorf("delete object copies: %w", err)
		}
		copies := make([]DeletedCopy, len(existing))
		deltas := make(map[string]int64, len(existing))
		for i, ec := range existing {
			copies[i] = DeletedCopy{BackendName: ec.BackendName, SizeBytes: ec.SizeBytes}
			deltas[ec.BackendName] -= ec.SizeBytes
		}
		if err := applyQuotaDeltas(ctx, tx, deltas); err != nil {
			return nil, err
		}
		return copies, nil
	})
}

// -------------------------------------------------------------------------
// DELETE OBJECTS BATCH
// -------------------------------------------------------------------------

// DeleteObjectsBatch removes every supplied key (and all its replicas)
// in a single transaction, decrementing each affected backend's quota
// once by the sum of removed bytes. Returns a map from key to its
// displaced copies so the caller can fan out to the backend cleanup
// path. Keys with no copies on disk are absent from the returned map
// (treated as success-with-nothing-to-clean-up). Empty input yields an
// empty map without opening a transaction.
func DeleteObjectsBatch(ctx context.Context, runner Runner, keys []string) (map[string][]DeletedCopy, error) {
	if len(keys) == 0 {
		return map[string][]DeletedCopy{}, nil
	}
	return WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) (map[string][]DeletedCopy, error) {
		rows, err := tx.GetCopiesForKeysForUpdate(ctx, keys)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return map[string][]DeletedCopy{}, nil
		}
		if err := tx.DeleteObjectsByKeys(ctx, keys); err != nil {
			return nil, fmt.Errorf("delete object copies by keys: %w", err)
		}
		// Per-key copies for the caller, plus per-backend totals so we
		// decrement each backend's quota exactly once instead of once
		// per displaced copy. Deltas apply in stable backend_name
		// order via applyQuotaDeltas; the previous
		// map-iteration order was non-deterministic and let two
		// concurrent batch deletes deadlock on backend_quotas locks.
		copies := make(map[string][]DeletedCopy, len(keys))
		deltas := make(map[string]int64)
		for _, r := range rows {
			copies[r.ObjectKey] = append(copies[r.ObjectKey], DeletedCopy{
				BackendName: r.BackendName,
				SizeBytes:   r.SizeBytes,
			})
			deltas[r.BackendName] -= r.SizeBytes
		}
		if err := applyQuotaDeltas(ctx, tx, deltas); err != nil {
			return nil, err
		}
		return copies, nil
	})
}

// -------------------------------------------------------------------------
// DELETE OBJECT LOCATION
// -------------------------------------------------------------------------

// DeleteObjectLocation removes a single (key, backend) copy from the
// object ledger and debits the backend's bytes_used by that copy's size
// in the same transaction, keeping bytes_used in agreement with
// SUM(object_locations.size_bytes). Its callers are the paths that drop a
// row because the backend no longer holds the object: reconcile's
// stale-entry deleter, the replicator's stale-source prune, and drain's
// replica-source removal and purge. A row that is already gone is a
// benign no-op that leaves the quota untouched.
//
// The size comes from the same FOR-UPDATE re-read that guards the delete,
// so a concurrent overwrite cannot make the debit disagree with the row
// that was actually removed.
func DeleteObjectLocation(ctx context.Context, runner Runner, key, backendName string) error {
	return runner.WithTx(ctx, func(ctx context.Context, tx TxAdapter) error {
		existing, err := tx.GetExistingCopiesForUpdate(ctx, key)
		if err != nil {
			return err
		}
		size, found := copySizeForBackend(existing, backendName)
		if !found {
			return nil
		}
		if err := tx.DeleteObjectFromBackend(ctx, key, backendName); err != nil {
			return err
		}
		return tx.DecrementBackendQuota(ctx, backendName, size)
	})
}

// -------------------------------------------------------------------------
// MOVE OBJECT LOCATION
// -------------------------------------------------------------------------

// MoveObjectLocation atomically moves a copy of an object from one
// backend to another. Uses row-level locks to prevent races. Returns
// (0, nil) if the source copy is gone or the target already has a
// copy.
func MoveObjectLocation(ctx context.Context, runner Runner, key, fromBackend, toBackend string) (int64, error) {
	return WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) (int64, error) {
		targetHasCopy, err := tx.CheckObjectExistsOnBackend(ctx, key, toBackend)
		if err != nil {
			return 0, err
		}
		if targetHasCopy {
			return 0, nil
		}
		src, ok, err := tx.LockObjectOnBackend(ctx, key, fromBackend)
		if err != nil || !ok {
			return 0, err
		}
		if err := tx.DeleteObjectFromBackend(ctx, key, fromBackend); err != nil {
			return 0, err
		}
		// The description of the bytes is carried through the same conversion
		// every other path that moves them verbatim uses, rather than a
		// hand-listed subset of the source row's fields. A field omitted here is
		// a column describing bytes the moved copy then contradicts, which is
		// how this path came to drop the compression columns.
		dest := objectFromStoredForm(key, toBackend, src.SizeBytes, StoredFormFromLocation(src))
		if err := tx.InsertObjectLocation(ctx, dest); err != nil {
			return 0, err
		}
		if err := carryCompressionProbe(ctx, tx, src, key, toBackend); err != nil {
			return 0, err
		}
		// Apply both quota deltas in stable order: a concurrent
		// move in the opposite direction (b1->b2 vs b2->b1) used to
		// lock the two rows in opposite sequences and deadlock.
		if err := applyQuotaDeltas(ctx, tx, map[string]int64{
			fromBackend: -src.SizeBytes,
			toBackend:   src.SizeBytes,
		}); err != nil {
			return 0, err
		}
		return src.SizeBytes, nil
	})
}

// carryCompressionProbe copies a source copy's compression measurement onto the
// destination row of a move.
//
// A measurement of what the encoder produced for these bytes is not a
// description of them, so it rides here rather than through StoredForm. It
// still has to ride: the move is verbatim, so what was measured on the source
// holds on the destination, and dropping it has the next compression pass
// download the copy to learn it again.
func carryCompressionProbe(ctx context.Context, tx TxAdapter, src *ObjectLocation, key, toBackend string) error {
	if src.CompressionProbeSize <= 0 {
		return nil
	}
	return tx.RecordCompressionProbe(ctx, &CompressionProbe{
		ObjectKey:   key,
		BackendName: toBackend,
		Size:        src.CompressionProbeSize,
		Level:       src.CompressionProbeLevel,
	})
}

// -------------------------------------------------------------------------
// IMPORT OBJECT
// -------------------------------------------------------------------------

// ImportObject records a pre-existing object in the database without
// overwriting. Returns true if the object was newly imported, false if
// it already existed for this backend. Used by reconcile and the sync
// subcommand to bring existing bucket objects under proxy management.
//
// unmanaged marks an object that lives outside every configured virtual
// bucket prefix. It is still recorded, because it occupies backend quota
// that placement decisions have to account for, but the workers leave it
// alone.
//
// form carries what the caller established about the bytes on the backend.
// Passing nil records the object as plaintext, so callers that import bytes
// they have not inspected will publish ciphertext as if it were the object.
func ImportObject(ctx context.Context, runner Runner, key, backend string, size int64, unmanaged bool, form *StoredForm) (bool, error) {
	return WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) (bool, error) {
		loc := objectFromStoredForm(key, backend, size, form)
		loc.Unmanaged = unmanaged
		inserted, err := tx.InsertObjectLocationIfNotExists(ctx, loc)
		if err != nil {
			return false, err
		}
		if !inserted {
			return false, nil
		}
		if err := tx.IncrementBackendQuota(ctx, backend, size); err != nil {
			return false, err
		}
		return true, nil
	})
}
