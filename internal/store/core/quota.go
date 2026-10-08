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
// CHARGE
// -------------------------------------------------------------------------

// chargeStripes records a mutation's byte movements on the key's stripe inside
// the same transaction as the object_locations change, so the counter cannot
// drift from the ledger. Backends are written in sorted order so concurrent
// transactions queue rather than deadlock.
func chargeStripes(ctx context.Context, tx TxAdapter, key string, deltas QuotaDeltas) error {
	if len(deltas) == 0 {
		return nil
	}
	stripe := StripeFor(key)
	for _, name := range slices.Sorted(maps.Keys(deltas)) {
		if deltas[name] == 0 {
			continue
		}
		if err := tx.AdjustQuotaStripe(ctx, name, stripe, deltas[name]); err != nil {
			return fmt.Errorf("charge quota stripe for %s: %w", name, err)
		}
	}
	return nil
}

// chargeStripesByKey is the batch form: each key's copies are charged to that
// key's own stripe, so a batch spreads across rows the way the individual
// writes it replaces would have.
func chargeStripesByKey(ctx context.Context, tx TxAdapter, perKey map[string]QuotaDeltas) error {
	for _, key := range slices.Sorted(maps.Keys(perKey)) {
		if err := chargeStripes(ctx, tx, key, perKey[key]); err != nil {
			return err
		}
	}
	return nil
}
