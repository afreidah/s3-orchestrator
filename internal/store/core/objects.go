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
func RecordObject(ctx context.Context, runner Runner, key, backend string, size int64, enc *EncryptionMeta) ([]DeletedCopy, error) {
	return WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) ([]DeletedCopy, error) {
		return recordObjectTx(ctx, tx, key, backend, size, enc, "")
	})
}

// RecordObjectAndClearPending performs the same atomic commit as
// RecordObject and additionally deletes the matching pending_objects
// intent inside the same transaction. The write path uses this on a
// successful PUT so the intent never outlives a committed location.
func RecordObjectAndClearPending(ctx context.Context, runner Runner, key, backend string, size int64, enc *EncryptionMeta, intentID string) ([]DeletedCopy, error) {
	return WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) ([]DeletedCopy, error) {
		return recordObjectTx(ctx, tx, key, backend, size, enc, intentID)
	})
}

// recordObjectTx is the shared transactional body. When intentID is
// non-empty the same transaction also deletes the corresponding
// pending row.
func recordObjectTx(ctx context.Context, tx TxAdapter, key, backend string, size int64, enc *EncryptionMeta, intentID string) ([]DeletedCopy, error) {
	if err := tx.AcquireKeyLock(ctx, key); err != nil {
		return nil, err
	}
	existing, err := tx.GetExistingCopiesForUpdate(ctx, key)
	if err != nil {
		return nil, err
	}
	displaced, err := clearExistingCopies(ctx, tx, key, backend, existing)
	if err != nil {
		return nil, err
	}
	if err := tx.InsertObjectLocation(ctx, objectFromEnc(key, backend, size, enc)); err != nil {
		return nil, fmt.Errorf("insert object location: %w", err)
	}
	if err := tx.IncrementBackendQuota(ctx, backend, size); err != nil {
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
// otherwise returns the deleted copies for cleanup.
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
		for i, ec := range existing {
			copies[i] = DeletedCopy{BackendName: ec.BackendName, SizeBytes: ec.SizeBytes}
			if err := tx.DecrementBackendQuota(ctx, ec.BackendName, ec.SizeBytes); err != nil {
				return nil, err
			}
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
		// per displaced copy.
		copies := make(map[string][]DeletedCopy, len(keys))
		backendTotals := make(map[string]int64)
		for _, r := range rows {
			copies[r.ObjectKey] = append(copies[r.ObjectKey], DeletedCopy{
				BackendName: r.BackendName,
				SizeBytes:   r.SizeBytes,
			})
			backendTotals[r.BackendName] += r.SizeBytes
		}
		for backend, total := range backendTotals {
			if err := tx.DecrementBackendQuota(ctx, backend, total); err != nil {
				return nil, err
			}
		}
		return copies, nil
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
		dest := &ObjectLocation{
			ObjectKey:     key,
			BackendName:   toBackend,
			SizeBytes:     src.SizeBytes,
			Encrypted:     src.Encrypted,
			EncryptionKey: src.EncryptionKey,
			KeyID:         src.KeyID,
			PlaintextSize: src.PlaintextSize,
			ContentHash:   src.ContentHash,
		}
		if err := tx.InsertObjectLocation(ctx, dest); err != nil {
			return 0, err
		}
		if err := tx.DecrementBackendQuota(ctx, fromBackend, src.SizeBytes); err != nil {
			return 0, err
		}
		if err := tx.IncrementBackendQuota(ctx, toBackend, src.SizeBytes); err != nil {
			return 0, err
		}
		return src.SizeBytes, nil
	})
}

// -------------------------------------------------------------------------
// IMPORT OBJECT
// -------------------------------------------------------------------------

// ImportObject records a pre-existing object in the database without
// overwriting. Returns true if the object was newly imported, false if
// it already existed for this backend. Used by the sync subcommand to
// bring existing bucket objects under proxy management.
func ImportObject(ctx context.Context, runner Runner, key, backend string, size int64) (bool, error) {
	return WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) (bool, error) {
		loc := &ObjectLocation{
			ObjectKey:   key,
			BackendName: backend,
			SizeBytes:   size,
		}
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
