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
// Two cases leave the backend alone: a copy already recorded there, because
// those bytes are that copy rather than the intent's, and another intent still
// live for the path, which discardedCompanionBytes leaves the path to.
func resolveCompanion(ctx context.Context, tx TxAdapter, p *PendingObject, existing []ExistingCopy) (promoteOutcome, error) {
	if err := tx.DeletePending(ctx, p.IntentID); err != nil {
		return promoteOutcome{}, fmt.Errorf("delete companion pending row: %w", err)
	}
	for _, ec := range existing {
		if ec.BackendName == p.BackendName {
			return promoteOutcome{result: PendingPromoteCompanionKept}, nil
		}
	}
	displaced, err := discardedCompanionBytes(ctx, tx, p, p.SizeBytes, CleanupReasonCompanionDiscarded)
	if err != nil {
		return promoteOutcome{}, err
	}
	return promoteOutcome{result: PendingPromoteCompanionDiscarded, displaced: displaced}, nil
}

// discardedCompanionBytes names the bytes a discarded copy leaves on its
// backend for the caller to remove, unless another intent for the same key
// and backend is still live.
//
// A delete goes by key and backend, so it removes whatever is at the path when
// it arrives. While another intent for that path is live, that is the other
// upload's bytes once they land, and its commit would then record a row for a
// copy that is gone. So the path is left to that intent, untouched. Every
// resolution of a key runs under the key lock, so the intents for a path
// resolve one at a time, and the last one to resolve finds no other and
// deletes the path: if the other upload commits, the path holds its copy; if
// it fails or its process dies, the intent is still there for the reaper,
// which deletes the path the same way.
//
// The intent is not cancelled, because it is the only promise that the path
// gets cleaned up when its upload does not commit. Deleting it would leave
// the bytes with no row, no intent and no cleanup entry.
func discardedCompanionBytes(ctx context.Context, tx TxAdapter, p *PendingObject, size int64, reason string) ([]DeletedCopy, error) {
	live, err := tx.CountPendingOnBackend(ctx, p.ObjectKey, p.BackendName)
	if err != nil {
		return nil, fmt.Errorf("count intents live for the path: %w", err)
	}
	if live > 0 {
		return nil, nil
	}
	return []DeletedCopy{{BackendName: p.BackendName, SizeBytes: size, Reason: reason}}, nil
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
		return discardUntrustedCopy(ctx, tx, p)
	}
	loc := objectFromStoredForm(p.ObjectKey, p.BackendName, p.SizeBytes, pendingStoredForm(p), p.Identity)
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

// discardUntrustedCopy resolves an upload whose write has been overtaken.
//
// Its bytes went down at a path a newer write may also have written, in an
// order nothing here can establish, so the object sitting there is either
// version and reads served from it would be silently wrong. A row already
// claiming a copy on that backend describes the same path and is no safer, so
// it goes too: replication rebuilds the copy from one the client was told
// about, which costs a rebuild in the case where these bytes never landed on
// top of anything. The bytes themselves are left to any other intent still
// live for the path, for the reason discardedCompanionBytes gives.
func discardUntrustedCopy(ctx context.Context, tx TxAdapter, p *PendingObject) (companionOutcome, error) {
	orphaned := p.SizeBytes
	deltas := QuotaDeltas{}
	loc, ok, err := tx.LockObjectOnBackend(ctx, p.ObjectKey, p.BackendName)
	if err != nil {
		return companionOutcome{}, err
	}
	if ok {
		if err := tx.DeleteObjectFromBackend(ctx, p.ObjectKey, p.BackendName); err != nil {
			return companionOutcome{}, fmt.Errorf("delete untrusted copy: %w", err)
		}
		orphaned = loc.SizeBytes
		deltas.Add(p.BackendName, -loc.SizeBytes)
		if err := chargeStripes(ctx, tx, p.ObjectKey, deltas); err != nil {
			return companionOutcome{}, err
		}
	}
	displaced, err := discardedCompanionBytes(ctx, tx, p, orphaned, CleanupReasonCompanionUntrusted)
	if err != nil {
		return companionOutcome{}, err
	}
	return companionOutcome{result: CompanionCopyUntrusted, displaced: displaced, deltas: deltas}, nil
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
//
// keep names the intents of this same write still uploading. They are the one
// kind a commit leaves behind, because the write they belong to is the write
// doing the clearing; the row is what their commit later reads as proof that
// nothing newer has touched the key.
func clearSupersededIntents(ctx context.Context, tx TxAdapter, key string, landedOn, keep []string) ([]DeletedCopy, error) {
	cleared, err := tx.ClearPendingForKey(ctx, key, keep)
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
