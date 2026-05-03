// -------------------------------------------------------------------------------
// Core Replication Orchestration
//
// Author: Alex Freidah
//
// Engine-agnostic transactional logic for replica management. RecordReplica
// inserts a new replica copy iff the source copy still exists; RemoveExcessCopy
// deletes a single copy and decrements the backend's quota.
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

// RecordReplica inserts a replica copy of an object, but only if the
// source copy still exists. This prevents stale replicas when an
// object is overwritten or deleted during the (potentially slow)
// replication copy. Returns the size that was actually written into
// object_locations.size_bytes (read from the source row inside
// InsertReplicaConditional) and inserted=true on success, or
// (0, false, nil) when the source copy is gone or the target already
// holds a copy.
//
// IncrementBackendQuota is called with the same size the row was
// inserted with, so object_locations.size_bytes and
// backend_quotas.bytes_used always agree - even if the in-memory copy
// size the caller observed before this call differs (concurrent
// overwrite mid-replication).
func RecordReplica(ctx context.Context, runner Runner, key, targetBackend, sourceBackend string) (int64, bool, error) {
	res, err := WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) (recordReplicaResult, error) {
		size, inserted, err := tx.InsertReplicaConditional(ctx, key, targetBackend, sourceBackend)
		if err != nil || !inserted {
			return recordReplicaResult{}, err
		}
		if err := tx.IncrementBackendQuota(ctx, targetBackend, size); err != nil {
			return recordReplicaResult{}, err
		}
		return recordReplicaResult{size: size, inserted: true}, nil
	})
	return res.size, res.inserted, err
}

// -------------------------------------------------------------------------
// REMOVE EXCESS COPY
// -------------------------------------------------------------------------

// RemoveExcessCopy deletes one copy of an object from the given
// backend inside a transaction, decrementing the backend quota
// atomically. The caller must have already performed FOR UPDATE
// locking and copy-count validation.
func RemoveExcessCopy(ctx context.Context, runner Runner, key, backendName string, size int64) error {
	return runner.WithTx(ctx, func(ctx context.Context, tx TxAdapter) error {
		if err := tx.DeleteObjectFromBackend(ctx, key, backendName); err != nil {
			return err
		}
		return tx.DecrementBackendQuota(ctx, backendName, size)
	})
}
