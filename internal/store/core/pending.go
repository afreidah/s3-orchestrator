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
	displaced, err := clearExistingCopies(ctx, tx, p.ObjectKey, []string{p.BackendName}, existing, deltas)
	if err != nil {
		return promoteOutcome{}, err
	}
	loc := objectFromStoredForm(p.ObjectKey, p.BackendName, p.SizeBytes, pendingStoredForm(p), p.Identity)
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

// resolveCompanion settles an intent for one of the further copies a write was
// placing, left behind by a process that died before it could clean up.
//
// It never promotes. The bytes on that backend cannot be told apart from an
// older object at the same path, and there is a copy we can vouch for - the
// client was told the write succeeded, which only happens once a copy commits -
// so rebuilding from that copy is cheaper than being wrong. The replication
// worker sees the shortfall and fills it on its next pass.
//
// The one case that leaves the backend alone is a copy already recorded there,
// because those bytes are that copy rather than the intent's.
func resolveCompanion(ctx context.Context, tx TxAdapter, p *PendingObject, existing []ExistingCopy) (promoteOutcome, error) {
	if err := tx.DeletePending(ctx, p.IntentID); err != nil {
		return promoteOutcome{}, fmt.Errorf("delete companion pending row: %w", err)
	}
	for _, ec := range existing {
		if ec.BackendName == p.BackendName {
			return promoteOutcome{result: PendingPromoteCompanionKept}, nil
		}
	}
	return promoteOutcome{
		result: PendingPromoteCompanionDiscarded,
		displaced: []DeletedCopy{{
			BackendName: p.BackendName,
			SizeBytes:   p.SizeBytes,
			Reason:      CleanupReasonCompanionDiscarded,
		}},
	}, nil
}

// clearSupersededIntents removes every intent for the key and reports the ones
// whose bytes now need deleting off their backend.
//
// Every intent for a key is resolved by a write to it: the ones this write is
// committing are claims it has just honoured, and the rest describe an object it
// has replaced. Clearing them here is what leaves the reaper with only the
// intents of a process that died.
//
// landedOn names the backends this write placed a copy on. An intent naming one
// of them is dropped without touching the backend, because the object sitting
// at that path is this write's copy - the same reason an overwrite does not
// treat the backend it landed on as displaced.
func clearSupersededIntents(ctx context.Context, tx TxAdapter, key string, landedOn []string) ([]DeletedCopy, error) {
	cleared, err := tx.ClearPendingForKey(ctx, key, nil)
	if err != nil {
		return nil, fmt.Errorf("clear superseded intents: %w", err)
	}
	var stale []DeletedCopy
	for _, si := range cleared {
		if slices.Contains(landedOn, si.BackendName) {
			continue
		}
		stale = append(stale, DeletedCopy{
			BackendName: si.BackendName,
			SizeBytes:   si.SizeBytes,
			Reason:      CleanupReasonSupersededIntent,
		})
	}
	return stale, nil
}

// clearExistingCopies deletes every prior copy of the key and accumulates
// per-backend negative deltas in the supplied map, which the caller applies to
// the byte counter once the transaction has committed. Copies on backends the
// write is not landing on are returned as DeletedCopy entries so the caller can
// enqueue them for physical orphan cleanup.
//
// newBackends is the whole set the write places, not just one: a write landing
// on two backends that reported only the first would hand its own second copy
// to orphan cleanup.
func clearExistingCopies(ctx context.Context, tx TxAdapter, key string, newBackends []string, existing []ExistingCopy, deltas QuotaDeltas) ([]DeletedCopy, error) {
	if len(existing) == 0 {
		return nil, nil
	}
	if err := tx.DeleteObjectCopies(ctx, key); err != nil {
		return nil, fmt.Errorf("delete existing copies: %w", err)
	}
	for _, ec := range existing {
		deltas.Add(ec.BackendName, -ec.SizeBytes)
	}
	return displacedFromExisting(existing, newBackends), nil
}
