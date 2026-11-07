// -------------------------------------------------------------------------------
// Quota Charge - Byte-Counter Writes
//
// Author: Alex Freidah
//
// The write side of the byte counter. Every mutation charges what it changed to
// its keys' quota stripes inside its own transaction, in one statement, so the
// counter commits or rolls back with the rows.
// -------------------------------------------------------------------------------

package core

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
)

// -------------------------------------------------------------------------
// CHARGE
// -------------------------------------------------------------------------

// stripeRow names one row of a backend's byte counter.
type stripeRow struct {
	backend string
	stripe  int16
}

// StripeCharge is a signed byte change to one row of a backend's byte counter.
type StripeCharge struct {
	Backend string
	Stripe  int16
	Delta   int64
}

// stripeDeltas is the signed byte change a transaction makes to each stripe
// row, gathered so a row is written once however many keys land on it.
type stripeDeltas map[stripeRow]int64

// add accumulates a signed delta for one key on one backend, on the stripe the
// key's name selects.
func (d stripeDeltas) add(key, backend string, delta int64) {
	d[stripeRow{backend, StripeFor(key)}] += delta
}

// charge writes every changed row, ordered by backend and then stripe. Every
// transaction holding more than one row takes them in this order, so concurrent
// transactions queue rather than deadlock.
func (d stripeDeltas) charge(ctx context.Context, tx TxAdapter) error {
	rows := slices.SortedFunc(maps.Keys(d), func(a, b stripeRow) int {
		return cmp.Or(cmp.Compare(a.backend, b.backend), cmp.Compare(a.stripe, b.stripe))
	})
	charges := make([]StripeCharge, 0, len(rows))
	for _, row := range rows {
		if d[row] != 0 {
			charges = append(charges, StripeCharge{Backend: row.backend, Stripe: row.stripe, Delta: d[row]})
		}
	}
	if len(charges) == 0 {
		return nil
	}
	if err := tx.AdjustQuotaStripes(ctx, charges...); err != nil {
		return fmt.Errorf("charge quota stripes: %w", err)
	}
	return nil
}

// chargeStripes records a mutation's byte movements on the key's stripe inside
// the same transaction as the object_locations change, so the counter cannot
// drift from the ledger.
func chargeStripes(ctx context.Context, tx TxAdapter, key string, deltas quotaDeltas) error {
	rows := make(stripeDeltas, len(deltas))
	for name, delta := range deltas {
		rows.add(key, name, delta)
	}
	return rows.charge(ctx, tx)
}
