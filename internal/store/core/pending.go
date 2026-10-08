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
	"slices"
)

// -------------------------------------------------------------------------
// PROMOTE PENDING
// -------------------------------------------------------------------------

// promoteOutcome carries the resolution result and any displaced copies
// out of the transactional body so the caller can fan out cleanups.
type promoteOutcome struct {
	result    PendingPromoteResult
	displaced []DeletedCopy
	deltas    QuotaDeltas
}

// promotePendingTx is the transactional body of PromotePending. The
// orchestration reads as five ordered steps: claim, key-lock, supersession
// check, commit, and the same-tx delete of the pending row.
func promotePendingTx(ctx context.Context, tx TxAdapter, p *PendingObject) (promoteOutcome, error) {
	// The key lock comes first, ahead of the claim. A write takes it and then
	// deletes this key's intent rows, so claiming the row first would leave the
	// two transactions each holding what the other is waiting for.
	if err := tx.AcquireKeyLock(ctx, p.ObjectKey); err != nil {
		return promoteOutcome{}, err
	}
	claimed, err := tx.ClaimPending(ctx, p.IntentID)
	if err != nil {
		return promoteOutcome{}, err
	}
	if !claimed {
		return promoteOutcome{result: PendingPromoteAlreadyResolved}, nil
	}

	existing, err := tx.GetExistingCopiesForUpdate(ctx, p.ObjectKey)
	if err != nil {
		return promoteOutcome{}, err
	}

	if p.IsCompanion() {
		return resolveCompanion(ctx, tx, p, existing)
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
// inserts the new object_location row, and deletes the pending row in the same
// transaction. The per-backend byte deltas ride out on the outcome for the
// caller to apply.
func commitPromotion(ctx context.Context, tx TxAdapter, p *PendingObject, existing []ExistingCopy) (promoteOutcome, error) {
	deltas := make(QuotaDeltas, len(existing)+1)
	displaced, err := clearExistingCopies(ctx, tx, p.ObjectKey, existing, deltas)
	if err != nil {
		return promoteOutcome{}, err
	}
	loc := ObjectFromStoredForm(p.ObjectKey, p.BackendName, p.StorageKey, p.SizeBytes, pendingStoredForm(p), p.Identity)
	if err := tx.InsertObjectLocation(ctx, loc); err != nil {
		return promoteOutcome{}, fmt.Errorf("insert promoted location: %w", err)
	}
	deltas.Add(p.BackendName, p.SizeBytes)
	if err := chargeStripes(ctx, tx, p.ObjectKey, deltas); err != nil {
		return promoteOutcome{}, err
	}
	if err := tx.DeletePending(ctx, p.IntentID); err != nil {
		return promoteOutcome{}, fmt.Errorf("delete promoted pending row: %w", err)
	}
	return promoteOutcome{result: PendingPromoteCommitted, displaced: displaced, deltas: deltas}, nil
}

// resolveCompanion settles a companion-copy intent left by a process that died
// mid-upload. It never promotes, because the bytes may be partial; the
// replication worker rebuilds the copy from a committed one. The bytes are
// kept only when a copy is already recorded at this intent's own path.
func resolveCompanion(ctx context.Context, tx TxAdapter, p *PendingObject, existing []ExistingCopy) (promoteOutcome, error) {
	if err := tx.DeletePending(ctx, p.IntentID); err != nil {
		return promoteOutcome{}, fmt.Errorf("delete companion pending row: %w", err)
	}
	for _, ec := range existing {
		if ec.BackendName == p.BackendName && ec.StorageKey == p.StorageKey {
			return promoteOutcome{result: PendingPromoteCompanionKept}, nil
		}
	}
	return promoteOutcome{
		result: PendingPromoteCompanionDiscarded,
		displaced: []DeletedCopy{{
			BackendName: p.BackendName,
			StorageKey:  p.StorageKey,
			SizeBytes:   p.SizeBytes,
			Reason:      CleanupReasonCompanionDiscarded,
		}},
	}, nil
}

// -------------------------------------------------------------------------
// COMPANION COMMIT
// -------------------------------------------------------------------------

// companionOutcome carries the resolution of an extra copy out of the
// transactional body, shaped like promoteOutcome so both resolution paths hand
// their caller the same three things.
type companionOutcome struct {
	result    CompanionCommitResult
	displaced []DeletedCopy
	deltas    QuotaDeltas
}

// commitCompanionTx is the transactional body of CommitCompanionCopy: lock the
// key, claim the intent, and either add the copy or discard it. Any newer write
// clears this intent, so a successful claim proves nothing newer took the key.
func commitCompanionTx(ctx context.Context, tx TxAdapter, p *PendingObject) (companionOutcome, error) {
	if err := tx.AcquireKeyLock(ctx, p.ObjectKey); err != nil {
		return companionOutcome{}, err
	}
	claimed, err := tx.ClaimPending(ctx, p.IntentID)
	if err != nil {
		return companionOutcome{}, err
	}
	if !claimed {
		return discardUntrustedCopy(p), nil
	}
	loc := ObjectFromStoredForm(p.ObjectKey, p.BackendName, p.StorageKey, p.SizeBytes, pendingStoredForm(p), p.Identity)
	if err := tx.InsertObjectLocation(ctx, loc); err != nil {
		return companionOutcome{}, fmt.Errorf("insert companion location: %w", err)
	}
	deltas := QuotaDeltas{p.BackendName: p.SizeBytes}
	if err := chargeStripes(ctx, tx, p.ObjectKey, deltas); err != nil {
		return companionOutcome{}, err
	}
	if err := tx.DeletePending(ctx, p.IntentID); err != nil {
		return companionOutcome{}, fmt.Errorf("delete companion pending row: %w", err)
	}
	return companionOutcome{result: CompanionCopyCommitted, deltas: deltas}, nil
}

// discardUntrustedCopy resolves an upload overtaken by a newer write by
// deleting its bytes, which sit at a path no other write shares. Nothing is
// debited because the bytes were never recorded.
func discardUntrustedCopy(p *PendingObject) companionOutcome {
	return companionOutcome{
		result: CompanionCopyUntrusted,
		displaced: []DeletedCopy{{
			BackendName: p.BackendName,
			StorageKey:  p.StorageKey,
			SizeBytes:   p.SizeBytes,
			Reason:      CleanupReasonCompanionUntrusted,
		}},
	}
}

// clearSupersededIntents removes every intent for the key except keep, and
// returns the bytes of each cleared intent not in committing for deletion.
//
// committing intents are cleared but their bytes are the object being
// recorded. Every other cleared intent's bytes are stale, even on a backend
// this write also landed on, since they sit at a different path. keep holds
// this write's own uploads still in flight; their surviving rows are what
// their later commit reads as proof that nothing newer touched the key.
func clearSupersededIntents(ctx context.Context, tx TxAdapter, key string, keep, committing []string) ([]DeletedCopy, error) {
	cleared, err := tx.ClearPendingForKey(ctx, key, keep)
	if err != nil {
		return nil, fmt.Errorf("clear superseded intents: %w", err)
	}
	stale := make([]DeletedCopy, 0, len(cleared))
	for _, si := range cleared {
		if slices.Contains(committing, si.IntentID) {
			continue
		}
		stale = append(stale, DeletedCopy{
			BackendName: si.BackendName,
			StorageKey:  si.StorageKey,
			SizeBytes:   si.SizeBytes,
			Reason:      CleanupReasonSupersededIntent,
		})
	}
	return stale, nil
}

// clearExistingCopies deletes every prior copy of the key and accumulates
// per-backend negative deltas in the supplied map, which the caller applies to
// the byte counter once the transaction has committed. Every copy is returned
// as a DeletedCopy so the caller can enqueue its bytes for physical cleanup,
// each one naming the path it occupies.
func clearExistingCopies(ctx context.Context, tx TxAdapter, key string, existing []ExistingCopy, deltas QuotaDeltas) ([]DeletedCopy, error) {
	if len(existing) == 0 {
		return nil, nil
	}
	if err := tx.DeleteObjectCopies(ctx, key); err != nil {
		return nil, fmt.Errorf("delete existing copies: %w", err)
	}
	for _, ec := range existing {
		deltas.Add(ec.BackendName, -ec.SizeBytes)
	}
	return displacedFromExisting(existing), nil
}
