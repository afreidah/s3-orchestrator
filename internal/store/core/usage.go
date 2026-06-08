// -------------------------------------------------------------------------------
// Core Usage Reconciliation
//
// Author: Alex Freidah
//
// Recomputes backend_quotas.bytes_used from the authoritative object ledger
// (SUM(object_locations.size_bytes) per backend). bytes_used is otherwise an
// incrementally maintained counter that drifts permanently if any write,
// replication, delete, or cleanup path misses an adjustment - a degraded
// backend is the classic trigger. Engine-agnostic so both engines share one
// implementation; the per-engine TxAdapter supplies the read/write primitives.
// -------------------------------------------------------------------------------

package core

import "context"

// ReconcileUsage rewrites every backend's bytes_used counter to match the
// summed size of its object_locations rows, inside a single transaction.
// Returns the per-backend delta that was applied (truth - previous), with
// only drifted backends present; an empty map means everything already
// agreed. A backend whose ledger is empty is set back to zero.
func ReconcileUsage(ctx context.Context, runner Runner) (map[string]int64, error) {
	return WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) (map[string]int64, error) {
		current, err := tx.AllBackendBytesUsed(ctx)
		if err != nil {
			return nil, err
		}
		truth, err := tx.SumObjectSizesByBackend(ctx)
		if err != nil {
			return nil, err
		}

		adjustments := make(map[string]int64)
		for backend, used := range current {
			actual := truth[backend]
			if actual == used {
				continue
			}
			if err := tx.SetBackendBytesUsed(ctx, backend, actual); err != nil {
				return nil, err
			}
			adjustments[backend] = actual - used
		}
		return adjustments, nil
	})
}
