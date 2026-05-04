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

// Package core holds the engine-agnostic orchestration that both
// store engines share: the TxAdapter seam, the Runner abstraction,
// narrow store role interfaces, and operations that span multiple
// statements within a single transaction.
package core

import (
	"context"
	"fmt"
)

// -------------------------------------------------------------------------
// PENDING ORCHESTRATION
// -------------------------------------------------------------------------

// PromotePending resolves a pending intent transactionally. The pending
// row is locked first so two reaper instances cannot promote the same
// intent concurrently. The destination is then inspected:
//
//   - If no row for (object_key, backend_name) exists in
//     object_locations, the pending row is promoted: any displaced
//     copies on other backends are cleared, the new row is inserted
//     with the pending's metadata, quotas are adjusted, and the
//     pending row is deleted in the same tx. The displaced copies are
//     returned so the caller can enqueue cleanup.
//
//   - If any object_locations row for the key was created after this
//     intent was inserted, the intent is provably stale and the
//     pending row is dropped (Superseded).
//
//   - If the pending row is already gone (another reaper resolved it
//     between GetStalePending and the lock acquire), the call returns
//     PendingPromoteAlreadyResolved.
func PromotePending(ctx context.Context, runner Runner, p *PendingObject) (PendingPromoteResult, []DeletedCopy, error) {
	out, err := WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) (promoteOutcome, error) {
		return promotePendingTx(ctx, tx, p)
	})
	if err != nil {
		// On txn error the result code is meaningless; callers check
		// err first. Returning Ambiguous keeps the legacy contract
		// intact for any caller that ignores err and inspects the
		// result anyway.
		return PendingPromoteAmbiguous, nil, fmt.Errorf("promote pending: %w", err)
	}
	return out.result, out.displaced, nil
}
