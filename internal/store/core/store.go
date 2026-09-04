// -------------------------------------------------------------------------------
// Core Public API - Orchestrated Operations
//
// Author: Alex Freidah
//
// Top-level orchestration entry points for engine-agnostic operations that
// span multiple statements within a transaction. Engine packages call these
// from their own role-interface methods, supplying a Runner that opens an
// engine-specific transaction. Trivial single-statement operations stay in
// the engine packages where they call sqlc directly without crossing the
// TxAdapter seam.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"fmt"
)

// -------------------------------------------------------------------------
// PENDING ORCHESTRATION
// -------------------------------------------------------------------------

// PromotePending resolves a pending intent transactionally. The pending row is
// locked first so two reaper instances cannot promote the same intent
// concurrently, then the destination decides the outcome: an unoccupied
// (object_key, backend_name) promotes the intent and returns the displaced
// copies for the caller to enqueue cleanup on; a location row created after the
// intent makes it provably stale (Superseded); a pending row that vanished
// between GetStalePending and the lock means another reaper won
// (AlreadyResolved).
func PromotePending(ctx context.Context, runner Runner, p *PendingObject) (PendingPromoteResult, []DeletedCopy, QuotaDeltas, error) {
	out, err := WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) (promoteOutcome, error) {
		return promotePendingTx(ctx, tx, p)
	})
	if err != nil {
		// On txn error the result code is meaningless; callers check
		// err first. Returning Ambiguous keeps the legacy contract
		// intact for any caller that ignores err and inspects the
		// result anyway.
		return PendingPromoteAmbiguous, nil, nil, fmt.Errorf("promote pending: %w", err)
	}
	return out.result, out.displaced, out.deltas, nil
}
