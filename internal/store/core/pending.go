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
	loc := objectFromStoredForm(p.ObjectKey, p.BackendName, p.StorageKey, p.SizeBytes, pendingStoredForm(p), p.Identity)
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
// It never promotes. The upload was still running when its process died, so
// nothing here knows whether the bytes at that path are whole; there is a copy
// we can vouch for - the client was told the write succeeded, which only
// happens once a copy commits - so rebuilding from that copy is cheaper than
// being wrong. The replication worker sees the shortfall and fills it on its
// next pass.
//
// The one case that leaves the backend alone is a recorded copy at this
// intent's own path, which means a commit got there first and the bytes belong
// to it. A copy recorded on the same backend at a different path belongs to a
// different write, and deleting this intent's bytes does not touch it.
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
// key, claim the intent, and either add the copy or discard it.
//
// The intent is the whole test. A write clears every intent for its key except
// the ones it is itself still uploading, so finding this one still there says
// that nothing newer has taken the key and the bytes at that path are this
// write's own.
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
	loc := objectFromStoredForm(p.ObjectKey, p.BackendName, p.StorageKey, p.SizeBytes, pendingStoredForm(p), p.Identity)
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

// discardUntrustedCopy resolves an upload whose write has been overtaken: the
// intent is gone, so a newer write took the key while these bytes were still
// going up, and they describe an object that is no longer the object.
//
// It removes those bytes and nothing else. They sit at this write's own path,
// which no other write shares, so whatever the key holds on this backend now
// is untouched by deleting them, and the row describing it stays.
//
// Nothing is charged against the counter here. These bytes were never recorded,
// so the backend's total never included them, and the intent that was holding
// them against its headroom is already gone.
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

// clearSupersededIntents removes every intent for the key and reports the bytes
// each one was placing, which now need deleting off their backend.
//
// Every intent for a key is resolved by a write to it: the ones this write is
// committing are claims it has just honoured, and the rest describe an object it
// has replaced. Clearing them here is what leaves the reaper with only the
// intents of a process that died.
//
// committing names the intents this write is honouring here. They are cleared
// like the rest - the copies they describe are recorded now, so the intents have
// served their purpose - but their bytes are the object and must not be reported
// as stale.
//
// Every other cleared intent's bytes are stale, whichever backend they are on.
// An intent naming a backend this write also landed on used to be dropped
// without touching it, on the grounds that the object at that path was this
// write's own copy; each write now has a path of its own, so that intent's bytes
// are somewhere else entirely and leaving them leaks on exactly the backend the
// object is most likely to be on. The upload may still be running, in which case
// the deletion finds nothing and the copy's own commit discards it again.
//
// keep names the intents of this same write still uploading. They are the one
// kind a commit leaves behind, because the write they belong to is the write
// doing the clearing; the row is what their commit later reads as proof that
// nothing newer has touched the key.
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
