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

// SweepStaleCleanupQueueRows removes every cleanup_queue row for the
// (storageKey, backend) pair, decrements the backend's orphan_bytes by their
// total size, and returns the number of rows deleted.
//
// Rows are matched on the path, not the object: the reconciler only knows the
// bytes at that path are gone, and the key's other writes may still have bytes
// on the backend with deletions queued.
func SweepStaleCleanupQueueRows(ctx context.Context, runner Runner, storageKey, backend string) (int64, error) {
	return WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) (int64, error) {
		rowCount, totalBytes, err := tx.SumAndDeleteCleanupQueueRows(ctx, storageKey, backend)
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

// MoveCleanupToDLQ atomically moves a cleanup_queue row whose retry budget is
// spent into cleanup_dlq. It returns false when no row exists for id, which
// means a concurrent finaliser already removed it.
//
// orphan_bytes is left untouched because the bytes are still on the backend;
// an operator reconciles them when retrying or writing off the DLQ entry.
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
