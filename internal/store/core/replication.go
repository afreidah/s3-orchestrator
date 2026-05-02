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

// RecordReplica inserts a replica copy of an object, but only if the
// source copy still exists. This prevents stale replicas when an
// object is overwritten or deleted during the (potentially slow)
// replication copy. Returns true if the replica was inserted, false
// if skipped.
func RecordReplica(ctx context.Context, runner Runner, key, targetBackend, sourceBackend string, size int64) (bool, error) {
	return WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) (bool, error) {
		inserted, err := tx.InsertReplicaConditional(ctx, key, targetBackend, sourceBackend)
		if err != nil || !inserted {
			return false, err
		}
		if err := tx.IncrementBackendQuota(ctx, targetBackend, size); err != nil {
			return false, err
		}
		return true, nil
	})
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
