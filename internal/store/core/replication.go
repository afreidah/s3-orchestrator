// -------------------------------------------------------------------------------
// Core Replication Orchestration
//
// Author: Alex Freidah
//
// Engine-agnostic transactional logic for replica management. RecordReplica
// inserts a new replica copy iff the source copy still exists; RemoveExcessCopy
// re-reads copies under a key-scoped FOR-UPDATE lock and only deletes when the
// live count still exceeds the configured replication factor.
// -------------------------------------------------------------------------------

package core

import "context"

// -------------------------------------------------------------------------
// RECORD REPLICA
// -------------------------------------------------------------------------

// recordReplicaResult bundles the outputs of RecordReplica so the
// transaction wrapper can pass both back without an extra round-trip.
type recordReplicaResult struct {
	size     int64
	inserted bool
}

// removedCopy is the same shape for the removal direction: the bytes the
// transaction dropped, and whether it dropped anything at all. Zero bytes and
// removed=false are different from zero bytes and removed=true, which is why
// the flag is carried rather than inferred from the size.
type removedCopy struct {
	size    int64
	removed bool
}

// RecordReplica inserts a replica copy of an object, but only if the
// source copy still exists. This prevents stale replicas when an
// object is overwritten or deleted during the (potentially slow)
// replication copy. Returns the size that was actually written into
// object_locations.size_bytes (read from the source row inside
// InsertReplicaConditional) and inserted=true on success, or
// (0, false, nil) when the source copy is gone or the target already
// holds a copy.
//
// The size returned is the one the row was inserted with, read inside the
// transaction, so the caller credits the backend by exactly what landed - even
// if the copy size it observed before this call differs (concurrent overwrite
// mid-replication).
func RecordReplica(ctx context.Context, runner Runner, key, targetBackend, sourceBackend string) (int64, bool, error) {
	res, err := WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) (recordReplicaResult, error) {
		size, inserted, err := tx.InsertReplicaConditional(ctx, key, targetBackend, sourceBackend)
		if err != nil || !inserted {
			return recordReplicaResult{}, err
		}
		return recordReplicaResult{size: size, inserted: true}, nil
	})
	return res.size, res.inserted, err
}

// -------------------------------------------------------------------------
// REMOVE EXCESS COPY
// -------------------------------------------------------------------------

// RemoveExcessCopy deletes one copy of an object from the given backend
// inside a transaction. It acquires the key-scoped FOR-UPDATE lock,
// re-reads the copy set, and only proceeds when the live count still
// exceeds factor AND the target backend still holds a copy. Returns
// true when a copy was removed, false when a concurrent deleter or
// earlier cleaner tick already absorbed the excess (benign no-op).
//
// Pulling the size from the locked re-read instead of trusting the
// caller's stale value keeps object_locations.size_bytes and the byte
// counter in agreement even when the object was overwritten between the
// cleaner's scan and the per-copy tx. The size is returned rather than
// debited here, because the counter it feeds lives in memory.
func RemoveExcessCopy(ctx context.Context, runner Runner, key, backendName string, factor int) (int64, bool, error) {
	res, err := WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) (removedCopy, error) {
		if err := tx.AcquireKeyLock(ctx, key); err != nil {
			return removedCopy{}, err
		}
		existing, err := tx.GetExistingCopiesForUpdate(ctx, key)
		if err != nil {
			return removedCopy{}, err
		}
		if len(existing) <= factor {
			return removedCopy{}, nil
		}
		// Never drop the copy that carries the key when a sibling does not.
		// Copies of a key share one ciphertext and one DEK, so a set that
		// disagrees means some row lost its metadata; removing the row that
		// still has the key destroys the only way to read the bytes, while
		// removing the one without it is both safe and self-correcting.
		if isLastDecryptableCopy(existing, backendName) {
			return removedCopy{}, ErrCopyHoldsOnlyDEK
		}
		size, found := copySizeForBackend(existing, backendName)
		if !found {
			return removedCopy{}, nil
		}
		if err := tx.DeleteObjectFromBackend(ctx, key, backendName); err != nil {
			return removedCopy{}, err
		}
		return removedCopy{size: size, removed: true}, nil
	})
	return res.size, res.removed, err
}
