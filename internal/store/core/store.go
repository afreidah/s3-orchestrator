// -------------------------------------------------------------------------------
// Core Public API - Orchestrated Operations
//
// Author: Alex Freidah
//
// The pending-intent claims and resolutions, as TxOps methods each engine
// store promotes: claiming a backend for one intent or several, promoting a
// surviving intent, and committing a copy that finished after the client was
// answered. Trivial single-statement operations stay in the engine packages,
// where they call their queries directly without crossing the TxAdapter seam.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"fmt"
)

// -------------------------------------------------------------------------
// PENDING ORCHESTRATION
// -------------------------------------------------------------------------

// InsertPendingIfFits claims the bytes p will occupy on its backend and records
// the intent, reporting false when the backend has no room or carries a flag
// outside accepts. The room check reads the snapshot its own statement starts
// with, so two claims checked at once would each miss the other's bytes. The
// backend lock taken first, in its own statement, makes the check see every
// claim committed before it on a backend with a ceiling.
func (o TxOps) InsertPendingIfFits(ctx context.Context, p *PendingObject, accepts BackendFlags) (bool, error) {
	return WithTxVal(ctx, o.runner, func(ctx context.Context, tx TxAdapter) (bool, error) {
		if err := tx.AcquireBackendLock(ctx, p.BackendName); err != nil {
			return false, err
		}
		return tx.InsertPendingIfFits(ctx, p, accepts)
	})
}

// CopyClaims is what ClaimPendingCopies claimed: the intents that got a
// backend, in claim order with BackendName set, and the candidates that
// declined.
type CopyClaims struct {
	Claimed  []*PendingObject
	Declined []string
}

// ClaimPendingCopies claims a distinct backend for each intent in one
// transaction, walking ranked in order and recording each intent with the
// conditional insert InsertPendingIfFits uses. Every candidate is locked
// first, in sorted order, so two claims ranking the same backends differently
// cannot deadlock. An error rolls back every claim.
func (o TxOps) ClaimPendingCopies(ctx context.Context, intents []*PendingObject, ranked []string, accepts BackendFlags) (CopyClaims, error) {
	return WithTxVal(ctx, o.runner, func(ctx context.Context, tx TxAdapter) (CopyClaims, error) {
		if err := lockInOrder(ctx, ranked, tx.AcquireBackendLock); err != nil {
			return CopyClaims{}, err
		}
		return ClaimInRankOrder(ctx, intents, ranked, accepts, tx.InsertPendingIfFits)
	})
}

// ClaimInRankOrder walks ranked in order, asking insert whether each candidate
// takes the next unclaimed intent, until every intent has a backend. It is the
// decision ClaimPendingCopies makes, exported so a test that stubs the insert
// claims copies the same way.
func ClaimInRankOrder(ctx context.Context, intents []*PendingObject, ranked []string, accepts BackendFlags,
	insert func(context.Context, *PendingObject, BackendFlags) (bool, error),
) (CopyClaims, error) {
	var out CopyClaims
	for _, name := range ranked {
		if len(out.Claimed) == len(intents) {
			break
		}
		p := intents[len(out.Claimed)]
		p.BackendName = name
		fits, err := insert(ctx, p, accepts)
		if err != nil {
			return out, fmt.Errorf("claim %s: %w", name, err)
		}
		if fits {
			out.Claimed = append(out.Claimed, p)
		} else {
			out.Declined = append(out.Declined, name)
		}
	}
	return out, nil
}

// PromotePending resolves a pending intent transactionally. The pending row is
// locked first so two reapers cannot promote the same intent. A location row
// newer than the intent makes it Superseded, and a row that vanished before
// the lock means another reaper won (AlreadyResolved). Otherwise the intent is
// promoted. Whatever bytes the resolution stopped recording are queued, and the
// rows are returned for the caller to settle.
func (o TxOps) PromotePending(ctx context.Context, p *PendingObject) (PendingPromoteResult, []CleanupItem, error) {
	out, err := WithTxVal(ctx, o.runner, func(ctx context.Context, tx TxAdapter) (promoteOutcome, error) {
		return promotePendingTx(ctx, tx, p)
	})
	if err != nil {
		// Ambiguous is the safe result for a caller that inspects it before err.
		return PendingPromoteAmbiguous, nil, fmt.Errorf("promote pending: %w", err)
	}
	return out.result, out.cleanup, nil
}

// CommitCompanionCopy adds a copy whose upload was still running when the
// client was answered, keeping the copies its siblings committed. It returns
// Untrusted when a newer write took the key mid-upload, with the cleanup row
// queued for the copy's bytes.
func (o TxOps) CommitCompanionCopy(ctx context.Context, p *PendingObject) (CompanionCommitResult, []CleanupItem, error) {
	out, err := WithTxVal(ctx, o.runner, func(ctx context.Context, tx TxAdapter) (companionOutcome, error) {
		return commitCompanionTx(ctx, tx, p)
	})
	if err != nil {
		// The zero value with nothing queued, so a caller that reaches the
		// result before the error deletes no bytes over a database blip. The
		// intent is still there either way, and the reaper resolves it.
		return CompanionCopyCommitted, nil, fmt.Errorf("commit companion copy: %w", err)
	}
	return out.result, out.cleanup, nil
}
