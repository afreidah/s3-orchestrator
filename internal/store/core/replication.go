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

// RemovedCopy is the copy a removal dropped, taken from the locked re-read so
// the caller deletes the right path. Removed is carried separately because a
// zero-byte copy can still have been removed.
type RemovedCopy struct {
	StorageKey string
	SizeBytes  int64
	Removed    bool
}

// ReplicaInsert is one replica the replicator is recording. StorageKey is the
// path the caller uploaded to, not the source's, so cleanup of either copy
// reaches only that copy's bytes.
type ReplicaInsert struct {
	ObjectKey     string
	TargetBackend string
	SourceBackend string
	StorageKey    string
}

// RecordReplica inserts a replica row only if the source copy still exists, so
// an overwrite or delete mid-copy leaves no stale replica. It returns the size
// read from the source row inside the transaction, which the caller credits,
// or (0, false, nil) when the source is gone or the target already has a copy.
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

// RemoveExcessCopy deletes the copy on backendName only if, under the key lock,
// the copy set still exceeds factor and that backend still holds a copy.
// Removed is false when a concurrent deleter already absorbed the excess. Size
// and storage key come from the locked re-read; the caller debits the
// in-memory counter.
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
		// Never drop the copy that carries the DEK when a sibling does not;
		// copies share one ciphertext, so the sibling is the damaged row.
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
