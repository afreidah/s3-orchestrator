// -------------------------------------------------------------------------------
// Ledger Edit - One Transaction's Changes and What They Cost
//
// Author: Alex Freidah
//
// Every transaction that changes copies, intents or parts makes those changes
// through one edit, and each change records what it costs: a copy recorded is
// charged to its key's quota stripe, a copy removed is debited and its bytes
// queued for deletion, and an intent or upload given up on has its bytes
// queued. The edit writes those at commit, every cleanup row in one statement
// and then every stripe charge in another, so the charge is always the
// transaction's last statement and holds its rows only until the commit.
//
// The cleanup rows are queued already claimed by this instance, which deletes
// them once the transaction commits; a claim it never settles is taken over by
// the cleanup worker after the grace period. A backend's orphan bytes are the
// sum of its rows, read through backend_capacity, so nothing keeps a counter.
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
// LEDGER EDIT
// -------------------------------------------------------------------------

// ledgerEdit gathers what one transaction changes: the signed bytes its copy
// changes move on each stripe row, the bytes it stopped recording, and for the
// replication queue how each key's copies plus live intents moved, or the count
// a key ends with where the transaction knows it outright.
type ledgerEdit struct {
	tx      TxAdapter
	factor  int
	charges stripeDeltas
	queued  []*CleanupRequest
	moved   map[string]int
	counts  map[string]int
}

// newLedgerEdit starts an edit over tx that queues keys off factor. Each
// transaction body gets its own, so nothing one transaction gathers reaches
// another's commit.
func newLedgerEdit(tx TxAdapter, factor int) *ledgerEdit {
	return &ledgerEdit{tx: tx, factor: factor, charges: stripeDeltas{}, moved: map[string]int{}, counts: map[string]int{}}
}

// insertCopy records a copy and charges its bytes to its key's stripe on its
// backend.
func (e *ledgerEdit) insertCopy(ctx context.Context, loc *ObjectLocation) error {
	if err := e.tx.InsertObjectLocation(ctx, loc); err != nil {
		return fmt.Errorf("insert copy on %s: %w", loc.BackendName, err)
	}
	e.charges.add(loc.ObjectKey, loc.BackendName, loc.SizeBytes)
	e.moved[loc.ObjectKey]++
	return nil
}

// insertCopyIfAbsent records a copy unless its backend already holds one of the
// key, and charges its bytes only when it was recorded.
func (e *ledgerEdit) insertCopyIfAbsent(ctx context.Context, loc *ObjectLocation) (bool, error) {
	inserted, err := e.tx.InsertObjectLocationIfNotExists(ctx, loc)
	if err != nil || !inserted {
		return false, err
	}
	e.charges.add(loc.ObjectKey, loc.BackendName, loc.SizeBytes)
	e.moved[loc.ObjectKey]++
	return true, nil
}

// know records the count of copies plus live intents key ends with, for a
// transaction that sets the whole of it, so the commit queues the key without
// counting it again.
func (e *ledgerEdit) know(key string, count int) {
	e.counts[key] = count
}

// removeCopies deletes every copy of keys, given as the locked read returned
// them, debiting what each held and queueing its bytes under reason.
func (e *ledgerEdit) removeCopies(ctx context.Context, reason string, keys []string, copies []ExistingCopy) error {
	if len(copies) == 0 {
		return nil
	}
	if err := e.tx.DeleteObjectCopies(ctx, keys...); err != nil {
		return fmt.Errorf("delete copies: %w", err)
	}
	for i := range copies {
		c := &copies[i]
		e.debit(c)
		e.queue(c.ObjectKey, reason, c.BackendName, c.StorageKey, c.SizeBytes)
	}
	return nil
}

// removeCopy deletes one copy, debiting what it held and queueing its bytes
// under reason. An empty reason queues nothing, for a caller that knows the
// bytes are already gone.
func (e *ledgerEdit) removeCopy(ctx context.Context, reason string, c *ExistingCopy) error {
	if err := e.tx.DeleteObjectFromBackend(ctx, c.ObjectKey, c.BackendName); err != nil {
		return err
	}
	e.debit(c)
	if reason != "" {
		e.queue(c.ObjectKey, reason, c.BackendName, c.StorageKey, c.SizeBytes)
	}
	return nil
}

// dropIntent deletes an intent the transaction claimed and queues the bytes its
// upload may have left under reason.
func (e *ledgerEdit) dropIntent(ctx context.Context, p *PendingObject, reason string) error {
	if err := e.resolveIntent(ctx, p.ObjectKey, p.IntentID); err != nil {
		return err
	}
	e.queueIntent(p, reason)
	return nil
}

// resolveIntent deletes an intent of key the transaction claimed, whose upload
// it has recorded as a copy or is giving up on.
func (e *ledgerEdit) resolveIntent(ctx context.Context, key, intentID string) error {
	if err := e.tx.DeletePending(ctx, intentID); err != nil {
		return fmt.Errorf("delete intent: %w", err)
	}
	e.moved[key]--
	return nil
}

// queueIntent queues the bytes an intent's upload may have left under reason,
// for an intent another transaction already removed.
func (e *ledgerEdit) queueIntent(p *PendingObject, reason string) {
	e.queue(p.ObjectKey, reason, p.BackendName, p.StorageKey, p.SizeBytes)
}

// clearIntents removes every intent of keys except keep and queues the bytes of
// each one not in committing, whose bytes are the copies being recorded. keep
// holds the write's own uploads still in flight: their surviving rows are what
// their later commits read as proof that nothing newer touched the key.
func (e *ledgerEdit) clearIntents(ctx context.Context, keep, committing []string, keys ...string) error {
	cleared, err := e.tx.ClearPendingForKey(ctx, keep, keys...)
	if err != nil {
		return fmt.Errorf("clear superseded intents: %w", err)
	}
	for _, si := range cleared {
		e.moved[si.ObjectKey]--
		if !slices.Contains(committing, si.IntentID) {
			e.queue(si.ObjectKey, CleanupReasonSupersededIntent, si.BackendName, si.StorageKey, si.SizeBytes)
		}
	}
	return nil
}

// queue adds the deletion of the bytes stored for key at path on backend to the
// rows the commit writes. An empty path is the key itself, as for a copy written
// before each write had a path of its own.
func (e *ledgerEdit) queue(key, reason, backend, path string, size int64) {
	e.queued = append(e.queued, &CleanupRequest{
		BackendName: backend, ObjectKey: key, StorageKey: StoragePath(key, path), Reason: reason, SizeBytes: size,
	})
}

// debit takes a removed copy's bytes off its key's stripe on its backend, so a
// later credit for the same key cancels it exactly.
func (e *ledgerEdit) debit(c *ExistingCopy) {
	e.charges.add(c.ObjectKey, c.BackendName, -c.SizeBytes)
	e.moved[c.ObjectKey]--
}

// commit writes every queued cleanup row in one statement, queues the keys left
// off factor, and then writes every changed stripe row in another statement,
// returning the queued rows for the caller to settle once the transaction
// commits.
func (e *ledgerEdit) commit(ctx context.Context) ([]CleanupItem, error) {
	var cleanup []CleanupItem
	if len(e.queued) > 0 {
		items, err := e.tx.InsertCleanup(ctx, e.queued...)
		if err != nil {
			return nil, fmt.Errorf("queue cleanup: %w", err)
		}
		cleanup = items
	}
	if err := e.queueOffFactor(ctx); err != nil {
		return nil, err
	}
	if err := e.charges.charge(ctx, e.tx); err != nil {
		return nil, err
	}
	return cleanup, nil
}

// queueOffFactor queues each key the transaction left off factor. A key whose
// count it knows is decided here; one whose count it changed is counted in the
// database; one it left unchanged is skipped, since a key already off factor
// is already queued. A factor of one or less replicates nothing.
func (e *ledgerEdit) queueOffFactor(ctx context.Context) error {
	if e.factor <= 1 {
		return nil
	}
	known, directions := e.knownOffFactor()
	if len(known) > 0 {
		if err := e.tx.QueueReplication(ctx, known, directions); err != nil {
			return fmt.Errorf("queue replication: %w", err)
		}
	}
	if changed := e.countUnknown(); len(changed) > 0 {
		if err := e.tx.QueueOffFactor(ctx, e.factor, changed...); err != nil {
			return fmt.Errorf("queue replication: %w", err)
		}
	}
	return nil
}

// knownOffFactor returns the keys whose known count is off factor, with the
// side of it each is on. A key left with nothing is gone and needs nothing.
func (e *ledgerEdit) knownOffFactor() ([]string, []ReplicationDirection) {
	var keys []string
	var directions []ReplicationDirection
	for _, key := range slices.Sorted(maps.Keys(e.counts)) {
		if n := e.counts[key]; n > 0 && n != e.factor {
			keys = append(keys, key)
			directions = append(directions, directionOf(n, e.factor))
		}
	}
	return keys, directions
}

// countUnknown returns the keys whose count the transaction changed without
// knowing what it ended at.
func (e *ledgerEdit) countUnknown() []string {
	var keys []string
	for _, key := range slices.Sorted(maps.Keys(e.moved)) {
		if _, known := e.counts[key]; !known && e.moved[key] != 0 {
			keys = append(keys, key)
		}
	}
	return keys
}

// -------------------------------------------------------------------------
// OUTCOMES
// -------------------------------------------------------------------------

// outcome is what a transaction body hands back besides its error: what it
// decided, the cleanup rows its edit queued, and, for a request it refused, why.
// A refusal still commits, since it is what queues the bytes the caller wrote.
type outcome[R any] struct {
	result  R
	cleanup []CleanupItem
	refusal error
}

// finish commits e and returns result and refusal with the rows it queued. A
// failed commit returns only the error, so nothing is reported as queued.
func finish[R any](ctx context.Context, e *ledgerEdit, result R, refusal error) (outcome[R], error) {
	cleanup, err := e.commit(ctx)
	if err != nil {
		return outcome[R]{}, err
	}
	return outcome[R]{result: result, cleanup: cleanup, refusal: refusal}, nil
}

// -------------------------------------------------------------------------
// QUOTA STRIPES
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
