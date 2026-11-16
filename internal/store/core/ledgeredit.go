// -------------------------------------------------------------------------------
// Ledger Edit - One Transaction's Changes and What They Cost
//
// Author: Alex Freidah
//
// Every transaction that changes copies, intents or parts makes those changes
// through one edit, and each change records what it costs: a copy recorded is
// charged to its key's stripe of the ledger counters, a copy removed is debited
// and its bytes queued for deletion, and an intent or upload given up on has
// its bytes queued. The edit writes those at commit, every cleanup row in one
// statement and then every counter charge in another, so the charge is always
// the transaction's last statement and holds its rows only until the commit.
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

// ledgerEdit gathers what one transaction changes: what its copy changes move
// on each counter row, the bytes it stopped recording, and for the
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

// insertCopy records a copy and charges it to its key's stripe on its backend.
func (e *ledgerEdit) insertCopy(ctx context.Context, loc *ObjectLocation) error {
	if err := e.tx.InsertObjectLocation(ctx, loc); err != nil {
		return fmt.Errorf("insert copy on %s: %w", loc.BackendName, err)
	}
	e.credit(loc)
	return nil
}

// insertCopyIfAbsent records a copy unless its backend already holds one of the
// key, and charges it only when it was recorded.
func (e *ledgerEdit) insertCopyIfAbsent(ctx context.Context, loc *ObjectLocation) (bool, error) {
	inserted, err := e.tx.InsertObjectLocationIfNotExists(ctx, loc)
	if err != nil || !inserted {
		return false, err
	}
	e.credit(loc)
	return true, nil
}

// restate moves a copy whose row a patch changed off the counters it held and
// onto the ones it holds now. A patch that changes neither its form nor its
// scrub state moves nothing.
func (e *ledgerEdit) restate(before, after *ExistingCopy) {
	e.charges.add(before.ObjectKey, before.group(), -1)
	e.charges.add(after.ObjectKey, after.group(), 1)
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
// upload may have left under reason. An empty reason queues nothing, for an
// intent whose upload is known to have left no bytes.
func (e *ledgerEdit) dropIntent(ctx context.Context, p *PendingObject, reason string) error {
	if err := e.resolveIntent(ctx, p.ObjectKey, p.IntentID); err != nil {
		return err
	}
	if reason != "" {
		e.queueIntent(p, reason)
	}
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

// credit charges a recorded copy to its key's stripe on its backend.
func (e *ledgerEdit) credit(loc *ObjectLocation) {
	e.charges.add(loc.ObjectKey, insertedGroup(loc), 1)
	e.moved[loc.ObjectKey]++
}

// debit takes a removed copy off its key's stripe on its backend, so a later
// credit for the same key cancels it exactly.
func (e *ledgerEdit) debit(c *ExistingCopy) {
	e.charges.add(c.ObjectKey, c.group(), -1)
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
// COUNTER STRIPES
// -------------------------------------------------------------------------

// stripeCell names one counter row: the copies of one stored form, managed or
// not, on one stripe of a backend.
type stripeCell struct {
	backend string
	stripe  int16
	form    StoredFlags
	managed bool
}

// StripeCharge is a signed change to one counter row: the group's counts, added
// on the stripe named.
type StripeCharge struct {
	Stripe int16
	LedgerGroup
}

// stripeDeltas is the signed change a transaction makes to each counter row,
// gathered so a row is written once however many keys land on it.
type stripeDeltas map[stripeCell]LedgerCounts

// add accumulates n times the group's counts for one key, on the stripe the
// key's name selects, so n of -1 takes a removed copy off.
func (d stripeDeltas) add(key string, g LedgerGroup, n int64) {
	cell := stripeCell{g.Backend, StripeFor(key), g.Form, g.Managed}
	d[cell] = d[cell].plus(g.LedgerCounts, n)
}

// charge writes every changed row, ordered by backend, stripe, form and then
// managed flag. Every transaction holding more than one row takes them in this
// order, so concurrent transactions queue rather than deadlock.
func (d stripeDeltas) charge(ctx context.Context, tx TxAdapter) error {
	charges := make([]StripeCharge, 0, len(d))
	for _, cell := range slices.SortedFunc(maps.Keys(d), compareCells) {
		if counts := d[cell]; counts != (LedgerCounts{}) {
			g := LedgerGroup{Backend: cell.backend, Form: cell.form, Managed: cell.managed, LedgerCounts: counts}
			charges = append(charges, StripeCharge{Stripe: cell.stripe, LedgerGroup: g})
		}
	}
	if len(charges) == 0 {
		return nil
	}
	if err := tx.AdjustQuotaStripes(ctx, charges...); err != nil {
		return fmt.Errorf("charge ledger counters: %w", err)
	}
	return nil
}

// compareCells orders counter rows the way every counter write takes them,
// with an unmanaged row ahead of a managed one as SQL orders false first.
func compareCells(a, b stripeCell) int {
	return cmp.Or(cmp.Compare(a.backend, b.backend), cmp.Compare(a.stripe, b.stripe),
		cmp.Compare(a.form, b.form), cmp.Compare(boolRank(a.managed), boolRank(b.managed)))
}

// boolRank orders false before true.
func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}
