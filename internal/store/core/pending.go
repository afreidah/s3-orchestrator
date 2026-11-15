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

// promotePendingTx is the transactional body of PromotePending: claim the
// intent under the key lock, decide it against the locked copy set, and commit
// the edit.
func promotePendingTx(ctx context.Context, tx TxAdapter, p *PendingObject) (outcome[PendingPromoteResult], error) {
	claimed, err := keyIntentsHeld(ctx, tx, p.ObjectKey, p.IntentID)
	if err != nil {
		return outcome[PendingPromoteResult]{}, err
	}
	if !claimed {
		return outcome[PendingPromoteResult]{result: PendingPromoteAlreadyResolved}, nil
	}
	existing, err := tx.GetExistingCopiesForUpdate(ctx, p.ObjectKey)
	if err != nil {
		return outcome[PendingPromoteResult]{}, err
	}
	e := newLedgerEdit(tx)
	result, err := promote(ctx, e, p, existing)
	if err != nil {
		return outcome[PendingPromoteResult]{}, err
	}
	return finish(ctx, e, result, nil)
}

// promote decides a claimed intent: a companion is kept or discarded, a primary
// a newer copy superseded is dropped, and any other primary becomes the object.
func promote(ctx context.Context, e *ledgerEdit, p *PendingObject, existing []ExistingCopy) (PendingPromoteResult, error) {
	switch {
	case p.IsCompanion():
		return resolveCompanion(ctx, e, p, existing)
	case intentSuperseded(existing, p.CreatedAt):
		// The bytes are on the backend, since the reaper only promotes after its
		// probe found them, and a newer copy means nothing will ever record them.
		return PendingPromoteSuperseded, e.dropIntent(ctx, p, CleanupReasonSupersededIntent)
	default:
		return PendingPromoteCommitted, commitPromotion(ctx, e, p, existing)
	}
}

// commitPromotion makes a non-superseded intent the object: it replaces the
// prior copies with the intent's, deletes the pending row, and replaces the
// key's tags with the set the write carried.
func commitPromotion(ctx context.Context, e *ledgerEdit, p *PendingObject, existing []ExistingCopy) error {
	if err := e.removeCopies(ctx, CleanupReasonOverwritten, []string{p.ObjectKey}, existing); err != nil {
		return err
	}
	if err := recordIntent(ctx, e, p); err != nil {
		return err
	}
	return replaceObjectTagsTx(ctx, e.tx, p.ObjectKey, p.Tags)
}

// resolveCompanion settles a companion-copy intent left by a process that died
// mid-upload. It never promotes, because the bytes may be partial; the
// replication worker rebuilds the copy from a committed one. The bytes are
// kept only when a copy is already recorded at this intent's own path.
func resolveCompanion(ctx context.Context, e *ledgerEdit, p *PendingObject, existing []ExistingCopy) (PendingPromoteResult, error) {
	for _, ec := range existing {
		if ec.BackendName == p.BackendName && ec.StorageKey == p.StorageKey {
			if err := e.tx.DeletePending(ctx, p.IntentID); err != nil {
				return PendingPromoteCompanionKept, fmt.Errorf("delete companion pending row: %w", err)
			}
			return PendingPromoteCompanionKept, nil
		}
	}
	return PendingPromoteCompanionDiscarded, e.dropIntent(ctx, p, CleanupReasonCompanionDiscarded)
}

// -------------------------------------------------------------------------
// COMPANION COMMIT
// -------------------------------------------------------------------------

// commitCompanionTx is the transactional body of CommitCompanionCopy: lock the
// key, claim the intent, and either add the copy or discard it. Any newer write
// clears this intent, so a successful claim proves nothing newer took the key.
func commitCompanionTx(ctx context.Context, tx TxAdapter, p *PendingObject) (outcome[CompanionCommitResult], error) {
	claimed, err := keyIntentsHeld(ctx, tx, p.ObjectKey, p.IntentID)
	if err != nil {
		return outcome[CompanionCommitResult]{}, err
	}
	e := newLedgerEdit(tx)
	if !claimed {
		// The upload was overtaken. Its bytes sit at a path no other write
		// shares, and nothing is debited because they were never recorded.
		e.queueIntent(p, CleanupReasonCompanionUntrusted)
		return finish(ctx, e, CompanionCopyUntrusted, nil)
	}
	if err := recordIntent(ctx, e, p); err != nil {
		return outcome[CompanionCommitResult]{}, err
	}
	return finish(ctx, e, CompanionCopyCommitted, nil)
}

// recordIntent records the claimed intent's upload as a copy of its key and
// deletes the intent row, in the transaction that claimed it.
func recordIntent(ctx context.Context, e *ledgerEdit, p *PendingObject) error {
	loc := ObjectFromStoredForm(p.ObjectKey, p.BackendName, p.StorageKey, p.SizeBytes, &p.StoredForm, p.Identity)
	if err := e.insertCopy(ctx, loc); err != nil {
		return fmt.Errorf("record %s: %w", p.RoleOrDefault(), err)
	}
	if err := e.tx.DeletePending(ctx, p.IntentID); err != nil {
		return fmt.Errorf("delete %s pending row: %w", p.RoleOrDefault(), err)
	}
	return nil
}

// -------------------------------------------------------------------------
// DISCARD PENDING
// -------------------------------------------------------------------------

// DiscardPending drops an intent whose upload will not be recorded and queues
// its bytes under reason, for a write that gave up after uploading. An intent
// already gone was resolved by whoever removed it, which queued its bytes, so
// nothing is returned for it.
func (o TxOps) DiscardPending(ctx context.Context, p *PendingObject, reason string) ([]CleanupItem, error) {
	return WithTxVal(ctx, o.runner, func(ctx context.Context, tx TxAdapter) ([]CleanupItem, error) {
		claimed, err := keyIntentsHeld(ctx, tx, p.ObjectKey, p.IntentID)
		if err != nil || !claimed {
			return nil, err
		}
		e := newLedgerEdit(tx)
		if err := e.dropIntent(ctx, p, reason); err != nil {
			return nil, err
		}
		return e.commit(ctx)
	})
}
