// -------------------------------------------------------------------------------
// Quota Charge - Byte-Counter Writes
//
// Author: Alex Freidah
//
// The write side of the byte counter. Every mutation charges what it changed to
// the key's quota stripe inside its own transaction, one statement per backend
// it touched, so the counter commits or rolls back with the rows.
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
func chargeStripes(ctx context.Context, tx TxAdapter, key string, deltas quotaDeltas) error {
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
func chargeStripesByKey(ctx context.Context, tx TxAdapter, perKey map[string]quotaDeltas) error {
	for _, key := range slices.Sorted(maps.Keys(perKey)) {
		if err := chargeStripes(ctx, tx, key, perKey[key]); err != nil {
			return err
		}
	}
	return nil
}
