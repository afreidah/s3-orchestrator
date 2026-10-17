// -------------------------------------------------------------------------------
// Core Object Location Orchestration
//
// Author: Alex Freidah
//
// Engine-agnostic transactional logic for object_locations: recording new
// objects, removing old ones, placing copies on other backends, and import of
// pre-existing data. Each operation is a sequence of TxAdapter calls
// composed inside a single transaction so the Postgres and SQLite paths
// share one implementation.
// -------------------------------------------------------------------------------

package core

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"
)

// -------------------------------------------------------------------------
// RECORD OBJECT
// -------------------------------------------------------------------------

// ObjectCopy names one backend a write landed on, the pending intent it
// resolves there, and the path its bytes occupy. The path is per copy because
// each copy is stored under its own intent's id; the description of the bytes
// lives on the request, since every copy holds the same bytes.
type ObjectCopy struct {
	Backend    string
	IntentID   string
	StorageKey string
}

// RecordObjectRequest is one committed write: where the object landed, how its
// bytes are stored, the tag set it carries, and the pending intents it resolves.
//
// Copies commit together or not at all, so a partly recorded write never looks
// like an overwrite that displaced its own copies. Tags are kept out of Form
// because Form rides through replication and moves, while an object has one
// tag set however many copies exist.
//
// Placing names this write's copies whose uploads are still running. Their
// intents are the only ones a commit leaves alone; every other intent for the
// key describes an object this write replaced.
type RecordObjectRequest struct {
	Key      string
	Size     int64
	Form     *StoredForm
	Identity *ObjectIdentity
	Tags     []Tag
	Copies   []ObjectCopy
	Placing  []ObjectCopy
}

// keepIntents names the intents the commit must not clear: the ones belonging
// to this write's own uploads still in flight.
func (r *RecordObjectRequest) keepIntents() []string {
	ids := make([]string, 0, len(r.Placing))
	for i := range r.Placing {
		ids = append(ids, r.Placing[i].IntentID)
	}
	return ids
}

// committedIntents names the intents this write is honouring: one per copy it
// is recording. They are cleared with the rest, having served their purpose,
// but their bytes are the object and must not be handed to orphan cleanup.
func (r *RecordObjectRequest) committedIntents() []string {
	ids := make([]string, 0, len(r.Copies))
	for i := range r.Copies {
		ids = append(ids, r.Copies[i].IntentID)
	}
	return ids
}

// mutationResult is what a mutation's transactional body hands back: the copies
// that need physical cleanup and the byte deltas the caller must apply. Paired
// in one value because WithTxVal carries a single result, and separating them
// would mean two transactions to learn one outcome.
type mutationResult struct {
	displaced []DeletedCopy
	deltas    QuotaDeltas
}

// batchDeleteResult is the batch form of mutationResult: cleanup is owed per
// key, while the deltas are already folded per backend.
type batchDeleteResult struct {
	copies map[string][]DeletedCopy
	deltas QuotaDeltas
}

// RecordObject records an object's copies, replacing every existing copy of the
// key, and returns the displaced copies for cleanup plus the backend byte
// deltas. The caller applies the deltas to the in-memory counter; nothing here
// touches backend_quotas. The key's pending intents, except those in Placing,
// are cleared in the same transaction. A write whose own intent a newer write
// already cleared records nothing and returns only its own copies for cleanup.
func RecordObject(ctx context.Context, runner Runner, req *RecordObjectRequest) ([]DeletedCopy, QuotaDeltas, error) {
	// A request with no copies would clear the key and put nothing back.
	if len(req.Copies) == 0 {
		return nil, nil, ErrNoCopiesToRecord
	}
	if err := ValidateTags(req.Tags); err != nil {
		return nil, nil, err
	}
	res, err := WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) (mutationResult, error) {
		return recordObjectTx(ctx, tx, req)
	})
	return res.displaced, res.deltas, err
}

// recordObjectTx is the shared transactional body. Per-backend byte deltas are
// aggregated and handed back rather than written here, so the transaction holds
// no backend_quotas lock and concurrent writes to one backend do not queue
// behind each other.
func recordObjectTx(ctx context.Context, tx TxAdapter, req *RecordObjectRequest) (mutationResult, error) {
	if err := tx.AcquireKeyLock(ctx, req.Key); err != nil {
		return mutationResult{}, err
	}
	held, err := intentsHeld(ctx, tx, req.committedIntents())
	if err != nil {
		return mutationResult{}, err
	}
	if !held {
		return discardOvertakenWrite(ctx, tx, req)
	}
	existing, err := tx.GetExistingCopiesForUpdate(ctx, req.Key)
	if err != nil {
		return mutationResult{}, err
	}
	deltas := make(QuotaDeltas, len(existing)+len(req.Copies))
	displaced, err := clearExistingCopies(ctx, tx, req.Key, existing, deltas)
	if err != nil {
		return mutationResult{}, err
	}
	// A PUT replaces the tag set too, even when the key had no copies, so
	// leftover tag rows are swept. Tags commit in the same transaction as the
	// object.
	if err := replaceObjectTagsTx(ctx, tx, req.Key, req.Tags); err != nil {
		return mutationResult{}, err
	}
	for _, c := range req.Copies {
		if err := tx.InsertObjectLocation(ctx, ObjectFromStoredForm(req.Key, c.Backend, c.StorageKey, req.Size, req.Form, req.Identity)); err != nil {
			return mutationResult{}, fmt.Errorf("insert object location on %s: %w", c.Backend, err)
		}
		deltas.Add(c.Backend, req.Size)
	}
	if err := chargeStripes(ctx, tx, req.Key, deltas); err != nil {
		return mutationResult{}, err
	}
	superseded, err := clearSupersededIntents(ctx, tx, req.Key, req.keepIntents(), req.committedIntents())
	if err != nil {
		return mutationResult{}, err
	}
	return mutationResult{displaced: append(displaced, superseded...), deltas: deltas}, nil
}

// intentsHeld reports whether every intent the write is committing still
// exists. A newer write clears the key's other intents when it commits, so a
// missing one means this write was overtaken while it uploaded.
func intentsHeld(ctx context.Context, tx TxAdapter, intentIDs []string) (bool, error) {
	for _, id := range intentIDs {
		if id == "" {
			continue
		}
		claimed, err := tx.ClaimPending(ctx, id)
		if err != nil || !claimed {
			return false, err
		}
	}
	return true, nil
}

// discardOvertakenWrite resolves a write a newer one overtook: nothing is
// recorded, its remaining intents are dropped, and its own bytes are handed
// back for cleanup. The newer write already queued those bytes when it
// cleared the intent, but the upload may have landed after that delete ran.
func discardOvertakenWrite(ctx context.Context, tx TxAdapter, req *RecordObjectRequest) (mutationResult, error) {
	displaced := make([]DeletedCopy, 0, len(req.Copies))
	for _, c := range req.Copies {
		if c.IntentID != "" {
			if err := tx.DeletePending(ctx, c.IntentID); err != nil {
				return mutationResult{}, fmt.Errorf("delete overtaken intent: %w", err)
			}
		}
		displaced = append(displaced, DeletedCopy{
			BackendName: c.Backend,
			StorageKey:  c.StorageKey,
			SizeBytes:   req.Size,
			Reason:      CleanupReasonSupersededIntent,
		})
	}
	return mutationResult{displaced: displaced}, nil
}

// -------------------------------------------------------------------------
// DELETE OBJECT
// -------------------------------------------------------------------------

// DeleteObject removes all copies of an object and reports the byte deltas
// their removal made. Returns ErrObjectNotFound if the object doesn't exist;
// otherwise returns the deleted copies for cleanup.
func DeleteObject(ctx context.Context, runner Runner, key string) ([]DeletedCopy, QuotaDeltas, error) {
	res, err := WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) (mutationResult, error) {
		return deleteObjectTx(ctx, tx, key)
	})
	return res.displaced, res.deltas, err
}

// deleteObjectTx is the transactional body of DeleteObject: clear the key's
// copies, its tags and its intents, then debit what those copies held.
func deleteObjectTx(ctx context.Context, tx TxAdapter, key string) (mutationResult, error) {
	// The key lock comes before the row read on every path, because tagging
	// calls touch only object_tags and are excluded by the key lock alone.
	// Taking it in the same order everywhere prevents deadlock.
	if err := tx.AcquireKeyLock(ctx, key); err != nil {
		return mutationResult{}, err
	}
	existing, err := tx.GetExistingCopiesForUpdate(ctx, key)
	if err != nil {
		return mutationResult{}, err
	}
	if len(existing) == 0 {
		return mutationResult{}, ErrObjectNotFound
	}
	if err := tx.DeleteObjectCopies(ctx, key); err != nil {
		return mutationResult{}, fmt.Errorf("delete object copies: %w", err)
	}
	if err := clearTagsForKey(ctx, tx, key); err != nil {
		return mutationResult{}, err
	}
	// A delete keeps no intents: an upload still running is placing a copy of
	// an object that no longer exists.
	superseded, err := clearSupersededIntents(ctx, tx, key, nil, nil)
	if err != nil {
		return mutationResult{}, err
	}
	copies, deltas := debitExistingCopies(existing)
	if err := chargeStripes(ctx, tx, key, deltas); err != nil {
		return mutationResult{}, err
	}
	return mutationResult{displaced: append(copies, superseded...), deltas: deltas}, nil
}

// debitExistingCopies turns a locked copy set into the cleanup list and the
// negative byte deltas its removal owes each backend.
func debitExistingCopies(existing []ExistingCopy) ([]DeletedCopy, QuotaDeltas) {
	copies := make([]DeletedCopy, len(existing))
	deltas := make(QuotaDeltas, len(existing))
	for i, ec := range existing {
		copies[i] = DeletedCopy{BackendName: ec.BackendName, StorageKey: ec.StorageKey, SizeBytes: ec.SizeBytes}
		deltas.Add(ec.BackendName, -ec.SizeBytes)
	}
	return copies, deltas
}

// -------------------------------------------------------------------------
// DELETE OBJECTS BATCH
// -------------------------------------------------------------------------

// DeleteObjectsBatch removes every copy of the supplied keys in one transaction
// and returns each key's removed copies for cleanup plus the per-backend byte
// deltas. Keys with no copies are absent from the map.
func DeleteObjectsBatch(ctx context.Context, runner Runner, keys []string) (map[string][]DeletedCopy, QuotaDeltas, error) {
	if len(keys) == 0 {
		return map[string][]DeletedCopy{}, nil, nil
	}
	res, err := WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) (batchDeleteResult, error) {
		if err := lockKeysInOrder(ctx, tx, keys); err != nil {
			return batchDeleteResult{}, err
		}
		rows, err := tx.GetCopiesForKeysForUpdate(ctx, keys)
		if err != nil {
			return batchDeleteResult{}, err
		}
		if len(rows) == 0 {
			return batchDeleteResult{copies: map[string][]DeletedCopy{}}, nil
		}
		if err := tx.DeleteObjectsByKeys(ctx, keys); err != nil {
			return batchDeleteResult{}, fmt.Errorf("delete object copies by keys: %w", err)
		}
		if err := clearTagsForKeys(ctx, tx, keys); err != nil {
			return batchDeleteResult{}, err
		}
		copies, deltas, perKey := splitRemovedCopies(rows, len(keys))
		if err := chargeStripesByKey(ctx, tx, perKey); err != nil {
			return batchDeleteResult{}, err
		}
		return batchDeleteResult{copies: copies, deltas: deltas}, nil
	})
	return res.copies, res.deltas, err
}

// splitRemovedCopies folds the removed rows into the three views the batch
// needs: the copies each key owes cleanup for, the per-backend totals the
// caller reports, and the per-key totals the stripe charge uses, since each
// key's bytes belong on the stripe its own name selects.
func splitRemovedCopies(rows []KeyedExistingCopy, keyCount int) (map[string][]DeletedCopy, QuotaDeltas, map[string]QuotaDeltas) {
	copies := make(map[string][]DeletedCopy, keyCount)
	deltas := make(QuotaDeltas)
	perKey := make(map[string]QuotaDeltas, keyCount)
	for _, r := range rows {
		copies[r.ObjectKey] = append(copies[r.ObjectKey], DeletedCopy{
			BackendName: r.BackendName,
			StorageKey:  r.StorageKey,
			SizeBytes:   r.SizeBytes,
		})
		deltas.Add(r.BackendName, -r.SizeBytes)
		if perKey[r.ObjectKey] == nil {
			perKey[r.ObjectKey] = make(QuotaDeltas)
		}
		perKey[r.ObjectKey].Add(r.BackendName, -r.SizeBytes)
	}
	return copies, deltas, perKey
}

// lockKeysInOrder takes the per-key lock for every distinct key in sorted
// order, so concurrent batches sharing keys cannot deadlock. The caller's slice
// is not modified.
func lockKeysInOrder(ctx context.Context, tx TxAdapter, keys []string) error {
	ordered := slices.Clone(keys)
	slices.Sort(ordered)
	for _, k := range slices.Compact(ordered) {
		if err := tx.AcquireKeyLock(ctx, k); err != nil {
			return err
		}
	}
	return nil
}

// -------------------------------------------------------------------------
// DELETE OBJECT LOCATION
// -------------------------------------------------------------------------

// DeleteObjectLocation removes the copy loc names and returns it, for the
// caller to delete its bytes. The copy must still be the one the caller read:
// a copy gone from loc's backend, or replaced there by a newer write, is
// ErrCopyChanged, so a late caller never removes a newer write's row.
//
// keepAtLeast > 0 removes the copy only while the object holds more copies
// than that, returning ErrCopyNotExcess otherwise, and refuses with
// ErrCopyHoldsOnlyDEK the copy that would leave the object unreadable.
func DeleteObjectLocation(ctx context.Context, runner Runner, loc *ObjectLocation, keepAtLeast int) (DeletedCopy, error) {
	return WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) (DeletedCopy, error) {
		key := loc.ObjectKey
		if err := tx.AcquireKeyLock(ctx, key); err != nil {
			return DeletedCopy{}, err
		}
		existing, err := tx.GetExistingCopiesForUpdate(ctx, key)
		if err != nil {
			return DeletedCopy{}, err
		}
		held, err := removableCopy(existing, loc, keepAtLeast)
		if err != nil {
			return DeletedCopy{}, err
		}
		if err := tx.DeleteObjectFromBackend(ctx, key, loc.BackendName); err != nil {
			return DeletedCopy{}, err
		}
		// Only removing the last copy drops the object's tags.
		if len(existing) == 1 {
			if err := clearTagsForKey(ctx, tx, key); err != nil {
				return DeletedCopy{}, err
			}
		}
		if err := chargeStripes(ctx, tx, key, QuotaDeltas{loc.BackendName: -held.SizeBytes}); err != nil {
			return DeletedCopy{}, err
		}
		return DeletedCopy{BackendName: loc.BackendName, StorageKey: held.StorageKey, SizeBytes: held.SizeBytes}, nil
	})
}

// removableCopy returns the copy in the locked re-read that loc names, or the
// reason DeleteObjectLocation must leave it: ErrCopyChanged when it is gone or
// replaced, and with keepAtLeast set, ErrCopyNotExcess or ErrCopyHoldsOnlyDEK.
func removableCopy(existing []ExistingCopy, loc *ObjectLocation, keepAtLeast int) (ExistingCopy, error) {
	held, found := copyOnBackend(existing, loc.BackendName)
	if !found || held.StorageKey != loc.StorageKey {
		return ExistingCopy{}, ErrCopyChanged
	}
	if keepAtLeast <= 0 {
		return held, nil
	}
	if len(existing) <= keepAtLeast {
		return ExistingCopy{}, ErrCopyNotExcess
	}
	// Never drop the copy that carries the DEK when a sibling does not; copies
	// share one ciphertext, so the sibling is the damaged row.
	if isLastDecryptableCopy(existing, loc.BackendName) {
		return ExistingCopy{}, ErrCopyHoldsOnlyDEK
	}
	return held, nil
}

// -------------------------------------------------------------------------
// PLACE COPY
// -------------------------------------------------------------------------

// Placement records bytes copied verbatim from Source onto Target at
// StorageKey, the path the caller wrote. RemoveSource makes it a move: the
// source row goes in the same transaction. Source must be the copy the caller
// read, since its storage key is what proves the copied bytes are still the
// object.
type Placement struct {
	Source       *ObjectLocation
	Target       string
	StorageKey   string
	RemoveSource bool
}

// PlaceCopy records the copy a placement describes and returns the bytes it
// placed, read from the locked source row. A source gone or replaced since the
// caller read it is ErrCopyChanged; a target that already holds a copy or does
// not admit the bytes is ErrTargetRefused. Either way nothing is recorded and
// the caller owns the bytes it wrote.
func PlaceCopy(ctx context.Context, runner Runner, p *Placement) (int64, error) {
	return WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) (int64, error) {
		return placeCopyTx(ctx, tx, p)
	})
}

// placeCopyTx is the transactional body of PlaceCopy: lock the key, confirm
// the source and the target against the locked copy set, admit, insert, and
// for a move remove the source.
func placeCopyTx(ctx context.Context, tx TxAdapter, p *Placement) (int64, error) {
	key := p.Source.ObjectKey
	if err := tx.AcquireKeyLock(ctx, key); err != nil {
		return 0, err
	}
	existing, err := tx.GetExistingCopiesForUpdate(ctx, key)
	if err != nil {
		return 0, err
	}
	if _, err := removableCopy(existing, p.Source, 0); err != nil {
		return 0, err
	}
	if _, onTarget := copyOnBackend(existing, p.Target); onTarget {
		return 0, ErrTargetRefused
	}
	src, ok, err := tx.LockObjectOnBackend(ctx, key, p.Source.BackendName)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, ErrCopyChanged
	}
	admitted, err := tx.BackendAdmits(ctx, p.Target, src.SizeBytes)
	if err != nil {
		return 0, err
	}
	if !admitted {
		return 0, ErrTargetRefused
	}
	if err := tx.InsertObjectLocation(ctx, src.CopyAt(p.Target, p.StorageKey)); err != nil {
		return 0, fmt.Errorf("insert placed copy on %s: %w", p.Target, err)
	}
	deltas := QuotaDeltas{p.Target: src.SizeBytes}
	if p.RemoveSource {
		if err := tx.DeleteObjectFromBackend(ctx, key, src.BackendName); err != nil {
			return 0, err
		}
		deltas.Add(src.BackendName, -src.SizeBytes)
	}
	if err := chargeStripes(ctx, tx, key, deltas); err != nil {
		return 0, err
	}
	return src.SizeBytes, nil
}

// -------------------------------------------------------------------------
// IMPORT OBJECT
// -------------------------------------------------------------------------

// ImportObjectRequest is one object discovered on a backend. WrittenAt is the
// modification time the backend reported; zero stamps the time of discovery.
type ImportObjectRequest struct {
	Key       string
	Backend   string
	Size      int64
	Unmanaged bool
	Form      *StoredForm
	WrittenAt time.Time
}

// PathState is what the ledger says about a path a backend listed. A path
// with a delete outstanding ranks above one with a recorded copy, because the
// pending delete is what decides that import must leave the path alone.
type PathState uint8

// PathUntracked and the other states. An untracked path is absent from the
// map ListedPathStates returns.
const (
	PathUntracked      PathState = iota // nothing in the ledger names the path
	PathRecorded                        // a copy is recorded at the path
	PathInFlight                        // a write's intent names the path; its commit is still to come
	PathPendingCleanup                  // a delete for the path is queued or dead-lettered
)

// ImportOutcome reports what an import did with a discovered key.
// ImportSkippedPendingCleanup means a delete is outstanding and the bytes are
// an orphan, which callers must count apart from ImportSkippedExisting.
type ImportOutcome int

const (
	ImportSkippedExisting ImportOutcome = iota
	ImportInserted
	ImportSkippedPendingCleanup
)

// String renders the outcome for logs.
func (o ImportOutcome) String() string {
	switch o {
	case ImportInserted:
		return "inserted"
	case ImportSkippedPendingCleanup:
		return "skipped_pending_cleanup"
	default:
		return "skipped_existing"
	}
}

// ImportObject records a pre-existing backend object without overwriting an
// existing row. A key with a pending cleanup on that backend is skipped:
// importing it would undo the delete, and the cleanup queue already tracks the
// orphan and its bytes.
func ImportObject(ctx context.Context, runner Runner, req *ImportObjectRequest) (ImportOutcome, error) {
	return WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) (ImportOutcome, error) {
		// Bytes at a recorded path belong to a known copy under its real key;
		// without the recorded check a sync would import every per-write path
		// as an object named after its path.
		states, err := tx.ListedPathStates(ctx, req.Backend, []string{req.Key})
		if err != nil {
			return ImportSkippedExisting, err
		}
		switch states[req.Key] {
		case PathPendingCleanup:
			return ImportSkippedPendingCleanup, nil
		case PathRecorded, PathInFlight:
			return ImportSkippedExisting, nil
		}

		// The row addresses the path the listing found. No identity is
		// recorded because the backend's ETag is unknown here; the first read
		// records it. An envelope no key opens is recorded as unmanaged, so it
		// holds quota without being replicated, listed or served.
		loc := ObjectFromStoredForm(req.Key, req.Backend, req.Key, req.Size, req.Form, nil)
		loc.Unmanaged = req.Unmanaged || req.Form.Unreadable()
		loc.CreatedAt = cmp.Or(req.WrittenAt, time.Now())
		inserted, err := tx.InsertObjectLocationIfNotExists(ctx, loc)
		if err != nil {
			return ImportSkippedExisting, err
		}
		if !inserted {
			return ImportSkippedExisting, nil
		}
		// Charged without a ceiling check because the bytes are already stored.
		if err := tx.AdjustQuotaStripe(ctx, req.Backend, StripeFor(req.Key), req.Size); err != nil {
			return ImportSkippedExisting, err
		}
		return ImportInserted, nil
	})
}
