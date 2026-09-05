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

// chargeStripes records a mutation's byte movements inside the transaction
// that produced them, on the stripe the object key selects.
//
// Being in the same transaction as the object_locations rows is the whole
// point: the counter commits and rolls back with the ledger it summarizes, so
// it cannot drift from it. Reconciliation stays as an audit for the rows that
// predate this, not as the repair the counter depends on.
//
// Backends are written in sorted order so two transactions touching the same
// pair acquire the rows in the same sequence and queue rather than deadlock.
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
