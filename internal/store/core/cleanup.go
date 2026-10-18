// -------------------------------------------------------------------------------
// Core Cleanup Queue Orchestration
//
// Author: Alex Freidah
//
// Engine-agnostic transactional logic for the cleanup_queue and cleanup_dlq
// tables. Every transaction that stops recording bytes queues their deletion
// before it commits, so no copy, intent or part ever leaves bytes behind that
// nothing names. The rows are queued already claimed by this instance, which
// deletes them once the transaction commits; a claim it never settles is taken
// over by the cleanup worker after the grace period. A backend's orphan bytes
// are the sum of its rows, read through backend_capacity, so nothing here keeps
// a counter.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"errors"
	"fmt"
)

// -------------------------------------------------------------------------
// QUEUE REMOVED BYTES
// -------------------------------------------------------------------------

// enqueueRemoved queues the deletion of every copy the transaction stopped
// recording for objectKey, each labelled with its own reason or reason, and
// returns the queued rows for the caller to settle once the transaction
// commits.
func enqueueRemoved(ctx context.Context, tx TxAdapter, objectKey, reason string, removed []removedCopy) ([]CleanupItem, error) {
	if len(removed) == 0 {
		return nil, nil
	}
	items := make([]CleanupItem, 0, len(removed))
	for _, d := range removed {
		c := d.request(objectKey, reason)
		id, err := tx.InsertCleanup(ctx, c)
		if err != nil {
			return nil, fmt.Errorf("queue cleanup on %s: %w", c.BackendName, err)
		}
		items = append(items, CleanupItem{
			ID:          id,
			BackendName: c.BackendName,
			ObjectKey:   c.ObjectKey,
			StorageKey:  c.StorageKey,
			Reason:      c.Reason,
			SizeBytes:   c.SizeBytes,
		})
	}
	return items, nil
}

// intentBytes is the upload an intent names, for queueing when the intent is
// dropped without its bytes being recorded.
func intentBytes(p *PendingObject, reason string) removedCopy {
	return removedCopy{BackendName: p.BackendName, StorageKey: p.StorageKey, SizeBytes: p.SizeBytes, Reason: reason}
}

// -------------------------------------------------------------------------
// DISCARD PENDING
// -------------------------------------------------------------------------

// DiscardPending drops an intent whose upload will not be recorded and queues
// its bytes under reason, for a write that gave up after uploading. An intent
// already gone was resolved by whoever removed it, which queued its bytes, so
// nothing is returned for it.
func DiscardPending(ctx context.Context, runner Runner, p *PendingObject, reason string) ([]CleanupItem, error) {
	return WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) ([]CleanupItem, error) {
		if err := tx.AcquireKeyLock(ctx, p.ObjectKey); err != nil {
			return nil, err
		}
		claimed, err := tx.ClaimPending(ctx, p.IntentID)
		if err != nil || !claimed {
			return nil, err
		}
		return dropIntent(ctx, tx, p, reason)
	})
}

// dropIntent deletes an intent the transaction has claimed and queues its
// bytes under reason.
func dropIntent(ctx context.Context, tx TxAdapter, p *PendingObject, reason string) ([]CleanupItem, error) {
	if err := tx.DeletePending(ctx, p.IntentID); err != nil {
		return nil, fmt.Errorf("delete discarded intent: %w", err)
	}
	return enqueueRemoved(ctx, tx, p.ObjectKey, reason, []removedCopy{intentBytes(p, "")})
}

// -------------------------------------------------------------------------
// MOVE CLEANUP TO DLQ
// -------------------------------------------------------------------------

// MoveCleanupToDLQ atomically moves a cleanup_queue row whose retry budget is
// spent into cleanup_dlq. It returns false when no row exists for id, which
// means a concurrent finaliser already removed it. The bytes still count as
// the backend's orphans while the row is dead-lettered.
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
