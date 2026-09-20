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

// RemovedCopy is the same shape for the removal direction: the bytes the
// transaction dropped, where they are, and whether it dropped anything at all.
// Zero bytes and Removed=false are different from zero bytes and Removed=true,
// which is why the flag is carried rather than inferred from the size.
//
// StorageKey comes from the locked re-read rather than the caller's scan, for
// the same reason SizeBytes does and then some: it is what the caller deletes
// off the backend, and a path from a stale scan can name bytes a newer write
// has already replaced.
type RemovedCopy struct {
	StorageKey string
	SizeBytes  int64
	Removed    bool
}

// ReplicaInsert is one replica the replicator is recording: the object, the
// backend it read from, the backend it wrote to, and the path it wrote to
// there.
//
// The path is the caller's because the caller is what performed the upload. A
// replica is a fresh set of bytes on a new backend, so it is named after the
// copy that placed it rather than inheriting the source's name - which would
// put two copies of one object at one path again and leave a cleanup for either
// of them guessing.
type ReplicaInsert struct {
	ObjectKey     string
	TargetBackend string
	SourceBackend string
	StorageKey    string
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
func RecordReplica(ctx context.Context, runner Runner, r *ReplicaInsert) (int64, bool, error) {
	res, err := WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) (recordReplicaResult, error) {
		size, inserted, err := tx.InsertReplicaConditional(ctx, r)
		if err != nil || !inserted {
			return recordReplicaResult{}, err
		}
		if err := chargeStripes(ctx, tx, r.ObjectKey, QuotaDeltas{r.TargetBackend: size}); err != nil {
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
// exceeds factor AND the target backend still holds a copy. Removed is
// true when a copy was removed, false when a concurrent deleter or
// earlier cleaner tick already absorbed the excess (benign no-op).
//
// Pulling the size and the storage key from the locked re-read instead of
// trusting the caller's stale values keeps object_locations.size_bytes and the
// byte counter in agreement even when the object was overwritten between the
// cleaner's scan and the per-copy tx, and keeps the caller's backend delete
// aimed at the bytes this transaction actually dropped. The size is returned
// rather than debited here, because the counter it feeds lives in memory.
func RemoveExcessCopy(ctx context.Context, runner Runner, key, backendName string, factor int) (RemovedCopy, error) {
	return WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) (RemovedCopy, error) {
		if err := tx.AcquireKeyLock(ctx, key); err != nil {
			return RemovedCopy{}, err
		}
		existing, err := tx.GetExistingCopiesForUpdate(ctx, key)
		if err != nil {
			return RemovedCopy{}, err
		}
		if len(existing) <= factor {
			return RemovedCopy{}, nil
		}
		// Never drop the copy that carries the key when a sibling does not.
		// Copies of a key share one ciphertext and one DEK, so a set that
		// disagrees means some row lost its metadata; removing the row that
		// still has the key destroys the only way to read the bytes, while
		// removing the one without it is both safe and self-correcting.
		//
		// They share it from either direction: a replica is made by copying
		// bytes verbatim and inheriting the source row's stored form, and a
		// write placing its own copies encrypts once and hands every upload a
		// reader over that one ciphertext. Neither path can produce a set whose
		// members legitimately differ, which is what makes disagreement a lost
		// row rather than a state to preserve.
		if isLastDecryptableCopy(existing, backendName) {
			return RemovedCopy{}, ErrCopyHoldsOnlyDEK
		}
		victim, found := copyOnBackend(existing, backendName)
		if !found {
			return RemovedCopy{}, nil
		}
		if err := tx.DeleteObjectFromBackend(ctx, key, backendName); err != nil {
			return RemovedCopy{}, err
		}
		if err := chargeStripes(ctx, tx, key, QuotaDeltas{backendName: -victim.SizeBytes}); err != nil {
			return RemovedCopy{}, err
		}
		return RemovedCopy{StorageKey: victim.StorageKey, SizeBytes: victim.SizeBytes, Removed: true}, nil
	})
}
