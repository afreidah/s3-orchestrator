// -------------------------------------------------------------------------------
// Core Pending Object Orchestration
//
// Author: Alex Freidah
//
// Engine-agnostic transactional logic for the pending_objects table. The write
// path inserts an intent before the backend PUT and removes it on a successful
// metadata commit. Intents that survive a failed commit are resolved by the
// reaper via PromotePending. The orchestration honours the conservative
// supersession contract: a newer object_locations row for the same key
// supersedes the intent and the reaper drops it without writing metadata.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"fmt"
)

// -------------------------------------------------------------------------
// PROMOTE PENDING
// -------------------------------------------------------------------------

// promoteOutcome carries the resolution result and any displaced copies
// out of the transactional body so the caller can fan out cleanups.
type promoteOutcome struct {
	result    PendingPromoteResult
	displaced []DeletedCopy
}

// promotePendingTx is the transactional body of PromotePending. The
// orchestration reads as five ordered steps: claim, key-lock, supersession
// check, commit, and the same-tx delete of the pending row.
func promotePendingTx(ctx context.Context, tx TxAdapter, p *PendingObject) (promoteOutcome, error) {
	claimed, err := tx.ClaimPending(ctx, p.IntentID)
	if err != nil {
		return promoteOutcome{}, err
	}
	if !claimed {
		return promoteOutcome{result: PendingPromoteAlreadyResolved}, nil
	}

	if err := tx.AcquireKeyLock(ctx, p.ObjectKey); err != nil {
		return promoteOutcome{}, err
	}
	existing, err := tx.GetExistingCopiesForUpdate(ctx, p.ObjectKey)
	if err != nil {
		return promoteOutcome{}, err
	}

	if intentSuperseded(existing, p.CreatedAt) {
		if err := tx.DeletePending(ctx, p.IntentID); err != nil {
			return promoteOutcome{}, fmt.Errorf("delete superseded pending row: %w", err)
		}
		return promoteOutcome{result: PendingPromoteSuperseded}, nil
	}

	return commitPromotion(ctx, tx, p, existing)
}

// commitPromotion finalises a non-superseded intent: clears prior copies,
// inserts the new object_location row, increments the destination quota,
// and deletes the pending row in the same transaction.
func commitPromotion(ctx context.Context, tx TxAdapter, p *PendingObject, existing []ExistingCopy) (promoteOutcome, error) {
	displaced, err := clearExistingCopies(ctx, tx, p.ObjectKey, p.BackendName, existing)
	if err != nil {
		return promoteOutcome{}, err
	}
	loc := objectFromEnc(p.ObjectKey, p.BackendName, p.SizeBytes, pendingEncryptionMeta(p))
	if err := tx.InsertObjectLocation(ctx, loc); err != nil {
		return promoteOutcome{}, fmt.Errorf("insert promoted location: %w", err)
	}
	if err := tx.IncrementBackendQuota(ctx, p.BackendName, p.SizeBytes); err != nil {
		return promoteOutcome{}, err
	}
	if err := tx.DeletePending(ctx, p.IntentID); err != nil {
		return promoteOutcome{}, fmt.Errorf("delete promoted pending row: %w", err)
	}
	return promoteOutcome{result: PendingPromoteCommitted, displaced: displaced}, nil
}

// clearExistingCopies deletes every prior copy of the key and decrements
// each backend's quota. Copies on backends other than newBackend are
// returned as DeletedCopy entries so the caller can enqueue them for
// physical orphan cleanup.
func clearExistingCopies(ctx context.Context, tx TxAdapter, key, newBackend string, existing []ExistingCopy) ([]DeletedCopy, error) {
	if len(existing) == 0 {
		return nil, nil
	}
	if err := tx.DeleteObjectCopies(ctx, key); err != nil {
		return nil, fmt.Errorf("delete existing copies: %w", err)
	}
	for _, ec := range existing {
		if err := tx.DecrementBackendQuota(ctx, ec.BackendName, ec.SizeBytes); err != nil {
			return nil, err
		}
	}
	return displacedFromExisting(existing, newBackend), nil
}
