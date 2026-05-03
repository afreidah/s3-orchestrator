// -------------------------------------------------------------------------------
// Core Cleanup Queue Orchestration
//
// Author: Alex Freidah
//
// Engine-agnostic transactional logic for the cleanup_queue and cleanup_dlq
// tables. Most queue operations are single-statement and stay in the engine
// packages; the multi-step flows that need atomicity across rows live here
// so both engines share one implementation: the stale-row sweep that pairs
// row-deletion with orphan-bytes accounting, and the move-to-DLQ flow that
// graduates an exhausted retry to the dead-letter table without touching
// quota state.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"errors"
	"fmt"
)

// -------------------------------------------------------------------------
// SWEEP STALE CLEANUP QUEUE ROWS
// -------------------------------------------------------------------------

// SweepStaleCleanupQueueRows removes every cleanup_queue row matching
// the (objectKey, backend) pair and decrements the backend's
// orphan_bytes counter by the sum of their size_bytes. Used by the
// reconciler when it deletes a stale object_locations row so the
// queue does not retain orphan entries pointing at a key the backend
// no longer holds. Returns the number of rows deleted.
func SweepStaleCleanupQueueRows(ctx context.Context, runner Runner, objectKey, backend string) (int64, error) {
	return WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) (int64, error) {
		rowCount, totalBytes, err := tx.SumAndDeleteCleanupQueueRows(ctx, objectKey, backend)
		if err != nil {
			return 0, err
		}
		if rowCount == 0 {
			return 0, nil
		}
		if totalBytes > 0 {
			if err := tx.DecrementOrphanBytes(ctx, backend, totalBytes); err != nil {
				return 0, fmt.Errorf("decrement orphan bytes: %w", err)
			}
		}
		return rowCount, nil
	})
}

// -------------------------------------------------------------------------
// MOVE CLEANUP TO DLQ
// -------------------------------------------------------------------------

// MoveCleanupToDLQ atomically graduates an exhausted cleanup_queue row
// (one whose retry budget is spent without ever succeeding at the
// physical backend delete) to the cleanup_dlq table. The single
// transaction reads the queue row, inserts a corresponding DLQ row, and
// deletes the queue row.
//
// orphan_bytes is intentionally left untouched: the backend object is
// still on disk, so the bytes really are still occupying the backend's
// quota - decrementing here would lie about reclaimed capacity. The DLQ
// table exists so an operator can see the unrecoverable orphan, decide
// whether to retry it manually or write it off, and reconcile
// orphan_bytes deliberately as part of that workflow.
//
// Returns true if a row was moved, false if no row existed for id (a
// benign concurrent-finaliser race).
func MoveCleanupToDLQ(ctx context.Context, runner Runner, id int64, lastError string) (bool, error) {
	return WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) (bool, error) {
		row, err := tx.GetCleanupQueueRow(ctx, id)
		if err != nil {
			if errors.Is(err, ErrCleanupItemNotFound) {
				return false, nil
			}
			return false, err
		}
		if lastError != "" {
			row.LastError = lastError
		}
		if err := tx.InsertCleanupDLQ(ctx, &row); err != nil {
			return false, err
		}
		if err := tx.DeleteCleanupItem(ctx, id); err != nil {
			return false, err
		}
		return true, nil
	})
}
