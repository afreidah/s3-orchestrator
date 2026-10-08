// -------------------------------------------------------------------------------
// Core Object Location Orchestration
//
// Author: Alex Freidah
//
// Engine-agnostic transactional logic for object_locations: recording new
// objects, removing old ones, atomic moves between backends, and import of
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
// are cleared in the same transaction.
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

// DeleteObjectLocation removes the (key, backend) copy and returns the bytes it
// removed, for the caller to debit; a missing row removes nothing. The size
// comes from the locked re-read, so a concurrent overwrite cannot make the
// debit disagree with the removed row.
func DeleteObjectLocation(ctx context.Context, runner Runner, key, backendName string) (int64, error) {
	return WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) (int64, error) {
		if err := tx.AcquireKeyLock(ctx, key); err != nil {
			return 0, err
		}
		existing, err := tx.GetExistingCopiesForUpdate(ctx, key)
		if err != nil {
			return 0, err
		}
		held, found := copyOnBackend(existing, backendName)
		if !found {
			return 0, nil
		}
		if err := tx.DeleteObjectFromBackend(ctx, key, backendName); err != nil {
			return 0, err
		}
		// Only removing the last copy drops the object's tags.
		if len(existing) == 1 {
			if err := clearTagsForKey(ctx, tx, key); err != nil {
				return 0, err
			}
		}
		if err := chargeStripes(ctx, tx, key, QuotaDeltas{backendName: -held.SizeBytes}); err != nil {
			return 0, err
		}
		return held.SizeBytes, nil
	})
}

// -------------------------------------------------------------------------
// MOVE OBJECT LOCATION
// -------------------------------------------------------------------------

// MoveLocation is one src -> dest repointing of a copy. StorageKey is the
// destination path the caller wrote, not the source's, so orphan cleanup for a
// move that loses its race deletes exactly what that move uploaded.
type MoveLocation struct {
	ObjectKey   string
	FromBackend string
	ToBackend   string
	StorageKey  string
}

// MoveObjectLocation atomically moves a copy of an object from one backend to
// another and returns the bytes moved, for the caller to apply to the in-memory
// counter. It returns (0, nil) if the source copy is gone or the target
// already has a copy.
func MoveObjectLocation(ctx context.Context, runner Runner, m *MoveLocation) (int64, error) {
	key, fromBackend, toBackend := m.ObjectKey, m.FromBackend, m.ToBackend
	return WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) (int64, error) {
		targetHasCopy, err := tx.CheckObjectExistsOnBackend(ctx, key, toBackend)
		if err != nil {
			return 0, err
		}
		if targetHasCopy {
			return 0, nil
		}
		src, ok, err := tx.LockObjectOnBackend(ctx, key, fromBackend)
		if err != nil || !ok {
			return 0, err
		}
		if err := tx.DeleteObjectFromBackend(ctx, key, fromBackend); err != nil {
			return 0, err
		}
		// Carry the full stored form; hand-listing source fields drops columns
		// and leaves a row that contradicts its bytes.
		dest := ObjectFromStoredForm(key, toBackend, m.StorageKey, src.SizeBytes, StoredFormFromLocation(src), src.Identity)
		if err := tx.InsertObjectLocation(ctx, dest); err != nil {
			return 0, err
		}
		if err := carryCompressionProbe(ctx, tx, src, key, toBackend); err != nil {
			return 0, err
		}
		if err := chargeStripes(ctx, tx, key, QuotaDeltas{
			fromBackend: -src.SizeBytes,
			toBackend:   src.SizeBytes,
		}); err != nil {
			return 0, err
		}
		return src.SizeBytes, nil
	})
}

// carryCompressionProbe copies a source copy's compression measurement onto the
// destination row of a move. The probe is not part of StoredForm, and dropping
// it would make the next compression pass download the copy to measure again.
func carryCompressionProbe(ctx context.Context, tx TxAdapter, src *ObjectLocation, key, toBackend string) error {
	if src.CompressionProbeSize <= 0 {
		return nil
	}
	return tx.RecordCompressionProbe(ctx, &CompressionProbe{
		ObjectKey:   key,
		BackendName: toBackend,
		Size:        src.CompressionProbeSize,
		Level:       src.CompressionProbeLevel,
	})
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
		pending, err := tx.HasPendingCleanup(ctx, req.Key, req.Backend)
		if err != nil {
			return ImportSkippedExisting, err
		}
		if pending {
			return ImportSkippedPendingCleanup, nil
		}

		// Bytes at a recorded path belong to a known copy under its real key;
		// without this check a sync would import every per-write path as an
		// object named after its path.
		recorded, err := tx.CopyExistsAtPath(ctx, req.Backend, req.Key)
		if err != nil {
			return ImportSkippedExisting, err
		}
		if recorded {
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
