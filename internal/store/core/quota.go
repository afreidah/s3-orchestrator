// -------------------------------------------------------------------------------
// Quota Flush - Batched byte-counter writes
//
// Author: Alex Freidah
//
// The write side of the byte counter. Every mutation reports what it changed
// and the tracker accumulates those deltas in memory; this is where a flush
// interval's worth of them reaches backend_quotas, one statement per backend
// rather than one per object.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"fmt"
	"maps"
	"slices"
)

// -------------------------------------------------------------------------
// FLUSH
// -------------------------------------------------------------------------

// FlushQuotaDeltas applies accumulated per-backend byte deltas to
// backend_quotas in a single transaction.
//
// Backends are written in sorted order for the reason the per-write path used
// to sort them: two flushes touching the same backends acquire the row locks in
// the same sequence and queue rather than deadlock. The adjustment is
// unconditional - the limit was enforced in memory before the bytes were
// written, and a flush that declined to record them would leave bytes_used
// permanently short of what the backend holds.
func FlushQuotaDeltas(ctx context.Context, runner Runner, deltas QuotaDeltas) error {
	if len(deltas) == 0 {
		return nil
	}
	backends := slices.Sorted(maps.Keys(deltas))
	return runner.WithTx(ctx, func(ctx context.Context, tx TxAdapter) error {
		for _, name := range backends {
			delta := deltas[name]
			if delta == 0 {
				continue
			}
			if err := tx.AdjustBackendBytesUsed(ctx, name, delta); err != nil {
				return fmt.Errorf("flush quota delta for %s: %w", name, err)
			}
		}
		return nil
	})
}
