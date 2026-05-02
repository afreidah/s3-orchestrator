// -------------------------------------------------------------------------------
// Core Cleanup Queue Orchestration
//
// Author: Alex Freidah
//
// Engine-agnostic transactional logic for the cleanup_queue table. Most
// queue operations are single-statement and stay in the engine packages;
// the multi-step sweep that pairs row-deletion with orphan-bytes
// accounting lives here so both engines share one implementation.
// -------------------------------------------------------------------------------

package core

import (
	"context"
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
