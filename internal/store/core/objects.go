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
	"maps"
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

// RecordObject records an object's copies, replacing every existing copy of the
// key, and returns the cleanup rows it queued for the displaced bytes. The
// key's pending intents, except those in Placing, are cleared in the same
// transaction. A write whose own intent a newer write already cleared records
// nothing and queues only its own bytes.
func (o TxOps) RecordObject(ctx context.Context, req *RecordObjectRequest) ([]CleanupItem, error) {
	// A request with no copies would clear the key and put nothing back.
	if len(req.Copies) == 0 {
		return nil, ErrNoCopiesToRecord
	}
	if err := ValidateTags(req.Tags); err != nil {
		return nil, err
	}
	return WithTxVal(ctx, o.runner, func(ctx context.Context, tx TxAdapter) ([]CleanupItem, error) {
		return recordObjectTx(ctx, tx, req)
	})
}

// recordObjectTx is the shared transactional body. The bytes each copy adds or
// displaces are charged to the key's quota stripe inside the transaction, so
// the counter commits or rolls back with the rows.
func recordObjectTx(ctx context.Context, tx TxAdapter, req *RecordObjectRequest) ([]CleanupItem, error) {
	if err := tx.AcquireKeyLock(ctx, req.Key); err != nil {
		return nil, err
	}
	held, err := intentsHeld(ctx, tx, req.committedIntents())
	if err != nil {
		return nil, err
	}
	if !held {
		return discardOvertakenWrite(ctx, tx, req)
	}
	existing, err := tx.GetExistingCopiesForUpdate(ctx, req.Key)
	if err != nil {
		return nil, err
	}
	deltas := make(quotaDeltas, len(existing)+len(req.Copies))
	displaced, err := clearExistingCopies(ctx, tx, req.Key, existing, deltas)
	if err != nil {
		return nil, err
	}
	// A PUT replaces the tag set too, even when the key had no copies, so
	// leftover tag rows are swept. Tags commit in the same transaction as the
	// object.
	if err := replaceObjectTagsTx(ctx, tx, req.Key, req.Tags); err != nil {
		return nil, err
	}
	for _, c := range req.Copies {
		if err := tx.InsertObjectLocation(ctx, ObjectFromStoredForm(req.Key, c.Backend, c.StorageKey, req.Size, req.Form, req.Identity)); err != nil {
			return nil, fmt.Errorf("insert object location on %s: %w", c.Backend, err)
		}
		deltas.add(c.Backend, req.Size)
	}
	if err := chargeStripes(ctx, tx, req.Key, deltas); err != nil {
		return nil, err
	}
	superseded, err := clearSupersededIntents(ctx, tx, req.Key, req.keepIntents(), req.committedIntents())
	if err != nil {
		return nil, err
	}
	return enqueueRemoved(ctx, tx, req.Key, CleanupReasonOverwritten, append(displaced, superseded...))
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
// recorded, its remaining intents are dropped, and its own bytes are queued.
// The newer write already queued those bytes when it cleared the intent, but
// the upload may have landed after that delete ran.
func discardOvertakenWrite(ctx context.Context, tx TxAdapter, req *RecordObjectRequest) ([]CleanupItem, error) {
	own := make([]removedCopy, 0, len(req.Copies))
	for _, c := range req.Copies {
		if c.IntentID != "" {
			if err := tx.DeletePending(ctx, c.IntentID); err != nil {
				return nil, fmt.Errorf("delete overtaken intent: %w", err)
			}
		}
		own = append(own, removedCopy{BackendName: c.Backend, StorageKey: c.StorageKey, SizeBytes: req.Size})
	}
	return enqueueRemoved(ctx, tx, req.Key, CleanupReasonSupersededIntent, own)
}

// -------------------------------------------------------------------------
// DELETE OBJECT
// -------------------------------------------------------------------------

// DeleteObject removes all copies of an object and returns the cleanup rows it
// queued for their bytes. Returns ErrObjectNotFound if the object doesn't
// exist.
func (o TxOps) DeleteObject(ctx context.Context, key string) ([]CleanupItem, error) {
	return WithTxVal(ctx, o.runner, func(ctx context.Context, tx TxAdapter) ([]CleanupItem, error) {
		return deleteObjectTx(ctx, tx, key)
	})
}

// deleteObjectTx is the transactional body of DeleteObject: clear the key's
// copies, its tags and its intents, debit what those copies held, and queue
// their bytes.
func deleteObjectTx(ctx context.Context, tx TxAdapter, key string) ([]CleanupItem, error) {
	// The key lock comes before the row read on every path, because tagging
	// calls touch only object_tags and are excluded by the key lock alone.
	// Taking it in the same order everywhere prevents deadlock.
	if err := tx.AcquireKeyLock(ctx, key); err != nil {
		return nil, err
	}
	existing, err := tx.GetExistingCopiesForUpdate(ctx, key)
	if err != nil {
		return nil, err
	}
	if len(existing) == 0 {
		return nil, ErrObjectNotFound
	}
	if err := tx.DeleteObjectCopies(ctx, key); err != nil {
		return nil, fmt.Errorf("delete object copies: %w", err)
	}
	if err := clearTagsForKey(ctx, tx, key); err != nil {
		return nil, err
	}
	// A delete keeps no intents: an upload still running is placing a copy of
	// an object that no longer exists.
	superseded, err := clearSupersededIntents(ctx, tx, key, nil, nil)
	if err != nil {
		return nil, err
	}
	deltas := make(quotaDeltas, len(existing))
	for _, ec := range existing {
		deltas.add(ec.BackendName, -ec.SizeBytes)
	}
	if err := chargeStripes(ctx, tx, key, deltas); err != nil {
		return nil, err
	}
	return enqueueRemoved(ctx, tx, key, CleanupReasonDeleted, append(removedFromExisting(existing), superseded...))
}

// -------------------------------------------------------------------------
// DELETE OBJECTS BATCH
// -------------------------------------------------------------------------

// DeleteObjectsBatch removes every copy and intent of the supplied keys in one
// transaction and returns the cleanup rows it queued for their bytes.
func (o TxOps) DeleteObjectsBatch(ctx context.Context, keys []string) ([]CleanupItem, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	return WithTxVal(ctx, o.runner, func(ctx context.Context, tx TxAdapter) ([]CleanupItem, error) {
		return deleteObjectsBatchTx(ctx, tx, keys)
	})
}

// deleteObjectsBatchTx is the transactional body of DeleteObjectsBatch. Each
// removed key's intents are cleared as DeleteObject clears them, so an upload
// still running cannot commit a copy of an object the batch removed.
func deleteObjectsBatchTx(ctx context.Context, tx TxAdapter, keys []string) ([]CleanupItem, error) {
	if err := lockInOrder(ctx, keys, tx.AcquireKeyLock); err != nil {
		return nil, err
	}
	rows, err := tx.GetCopiesForKeysForUpdate(ctx, keys)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	if err := tx.DeleteObjectsByKeys(ctx, keys); err != nil {
		return nil, fmt.Errorf("delete object copies by keys: %w", err)
	}
	if err := clearTagsForKeys(ctx, tx, keys); err != nil {
		return nil, err
	}
	removed, perKey := splitRemovedCopies(rows, len(keys))
	if err := chargeStripesByKey(ctx, tx, perKey); err != nil {
		return nil, err
	}
	var cleanup []CleanupItem
	for _, key := range slices.Sorted(maps.Keys(removed)) {
		superseded, err := clearSupersededIntents(ctx, tx, key, nil, nil)
		if err != nil {
			return nil, err
		}
		queued, err := enqueueRemoved(ctx, tx, key, CleanupReasonDeleted, append(removed[key], superseded...))
		if err != nil {
			return nil, err
		}
		cleanup = append(cleanup, queued...)
	}
	return cleanup, nil
}

// splitRemovedCopies folds the removed rows into the two views the batch needs:
// the bytes each key queues, and the per-key totals the stripe charge uses,
// since each key's bytes belong on the stripe its own name selects.
func splitRemovedCopies(rows []KeyedExistingCopy, keyCount int) (map[string][]removedCopy, map[string]quotaDeltas) {
	removed := make(map[string][]removedCopy, keyCount)
	perKey := make(map[string]quotaDeltas, keyCount)
	for _, r := range rows {
		removed[r.ObjectKey] = append(removed[r.ObjectKey], removedCopy{
			BackendName: r.BackendName,
			StorageKey:  r.StorageKey,
			SizeBytes:   r.SizeBytes,
		})
		if perKey[r.ObjectKey] == nil {
			perKey[r.ObjectKey] = make(quotaDeltas)
		}
		perKey[r.ObjectKey].add(r.BackendName, -r.SizeBytes)
	}
	return removed, perKey
}

// lockInOrder takes lock on every distinct name in sorted order, so concurrent
// transactions locking overlapping sets cannot deadlock. The caller's slice is
// not modified.
func lockInOrder(ctx context.Context, names []string, lock func(context.Context, string) error) error {
	ordered := slices.Clone(names)
	slices.Sort(ordered)
	for _, name := range slices.Compact(ordered) {
		if err := lock(ctx, name); err != nil {
			return err
		}
	}
	return nil
}

// -------------------------------------------------------------------------
// DELETE OBJECT LOCATION
// -------------------------------------------------------------------------

// DeleteObjectLocation removes the copy loc names and queues its bytes under
// reason, returning the queued row for the caller to settle. An empty reason
// removes only the row, for a caller that already knows the bytes are gone.
// The copy must still be the one the caller read: a copy gone from loc's
// backend, or replaced there by a newer write, is ErrCopyChanged, so a late
// caller never removes a newer write's row.
//
// keepAtLeast > 0 removes the copy only while the object holds more copies
// than that, returning ErrCopyNotExcess otherwise, and refuses with
// ErrCopyHoldsOnlyDEK the copy that would leave the object unreadable.
func (o TxOps) DeleteObjectLocation(ctx context.Context, loc *ObjectLocation, keepAtLeast int, reason string) ([]CleanupItem, error) {
	return WithTxVal(ctx, o.runner, func(ctx context.Context, tx TxAdapter) ([]CleanupItem, error) {
		key := loc.ObjectKey
		if err := tx.AcquireKeyLock(ctx, key); err != nil {
			return nil, err
		}
		existing, err := tx.GetExistingCopiesForUpdate(ctx, key)
		if err != nil {
			return nil, err
		}
		held, err := removableCopy(existing, loc, keepAtLeast)
		if err != nil {
			return nil, err
		}
		if err := removeCopyRow(ctx, tx, key, &held, len(existing) == 1); err != nil {
			return nil, err
		}
		if reason == "" {
			return nil, nil
		}
		return enqueueRemoved(ctx, tx, key, reason, removedFromExisting([]ExistingCopy{held}))
	})
}

// removeCopyRow deletes the row of a copy of key the locked re-read holds and
// debits what it held. Removing the last copy also drops the object's tags.
func removeCopyRow(ctx context.Context, tx TxAdapter, key string, held *ExistingCopy, last bool) error {
	if err := tx.DeleteObjectFromBackend(ctx, key, held.BackendName); err != nil {
		return err
	}
	if last {
		if err := clearTagsForKey(ctx, tx, key); err != nil {
			return err
		}
	}
	return chargeStripes(ctx, tx, key, quotaDeltas{held.BackendName: -held.SizeBytes})
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

// Placement records bytes copied from Source to the path Intent claimed. The
// intent was claimed before the copy, which is what admitted the bytes, and it
// names the backend and the path the caller wrote. RemoveSource makes it a
// move: the source row goes in the same transaction. Source must be the copy
// the caller read, since its storage key is what proves the copied bytes are
// still the object.
//
// Reformed means the copy transformed the bytes on the way, such as a
// compression pass's encoding, and the intent describes their form and size;
// otherwise they are the source's bytes verbatim. A move may target the
// source's own backend, which replaces the copy read with the transformed one.
type Placement struct {
	Source       *ObjectLocation
	Intent       *PendingObject
	RemoveSource bool
	Reformed     bool
	Reasons      MoveReasons
}

// MoveReasons are the labels a placement queues bytes under: Orphan for the
// copy it wrote when it is refused, SourceDelete for the source a move
// removes.
type MoveReasons struct {
	Orphan       string
	SourceDelete string
}

// placeOutcome is what a placement transaction hands back: the bytes it
// placed, the cleanup rows it queued, and, for a placement it refused, why.
// A refusal still commits, since it is what queues the copy the caller wrote.
type placeOutcome struct {
	placed  int64
	cleanup []CleanupItem
	refusal error
}

// PlaceCopy records the copy a placement describes and clears its intent,
// returning the bytes it placed and the cleanup rows it queued. A source gone
// or replaced since the caller read it is ErrCopyChanged, and a target that
// already holds another copy is ErrTargetRefused; either way nothing is
// recorded, and the copied bytes are queued for deletion in their place.
func (o TxOps) PlaceCopy(ctx context.Context, p *Placement) (int64, []CleanupItem, error) {
	out, err := WithTxVal(ctx, o.runner, func(ctx context.Context, tx TxAdapter) (placeOutcome, error) {
		return placeCopyTx(ctx, tx, p)
	})
	if err != nil {
		return 0, nil, err
	}
	return out.placed, out.cleanup, out.refusal
}

// placeCopyTx is the transactional body of PlaceCopy: lock the key, confirm the
// intent, the source and the target against the locked copy set, clear the
// intent, for a move remove the source, and insert.
func placeCopyTx(ctx context.Context, tx TxAdapter, p *Placement) (placeOutcome, error) {
	key := p.Source.ObjectKey
	if err := tx.AcquireKeyLock(ctx, key); err != nil {
		return placeOutcome{}, err
	}
	held, err := intentsHeld(ctx, tx, []string{p.Intent.IntentID})
	if err != nil {
		return placeOutcome{}, err
	}
	if !held {
		// A client write took the key mid-copy and queued these bytes when it
		// cleared the intent, but the upload may have landed after that delete.
		cleanup, err := enqueueRemoved(ctx, tx, key, CleanupReasonSupersededIntent, []removedCopy{intentBytes(p.Intent)})
		return placeOutcome{cleanup: cleanup, refusal: ErrCopyChanged}, err
	}
	existing, err := tx.GetExistingCopiesForUpdate(ctx, key)
	if err != nil {
		return placeOutcome{}, err
	}
	if refusal := p.refusal(existing); refusal != nil {
		return p.refuse(ctx, tx, refusal)
	}
	src, ok, err := tx.LockObjectOnBackend(ctx, key, p.Source.BackendName)
	if err != nil {
		return placeOutcome{}, err
	}
	if !ok {
		return p.refuse(ctx, tx, ErrCopyChanged)
	}
	if err := tx.DeletePending(ctx, p.Intent.IntentID); err != nil {
		return placeOutcome{}, fmt.Errorf("delete placed intent: %w", err)
	}
	return p.commit(ctx, tx, src)
}

// commit removes the source of a move and inserts the placed copy. The source
// row goes first, since an in-place rewrite inserts on the same backend and a
// key holds one copy per backend.
func (p *Placement) commit(ctx context.Context, tx TxAdapter, src *ObjectLocation) (placeOutcome, error) {
	key := src.ObjectKey
	placed := p.placedCopy(src)
	deltas := quotaDeltas{}
	var cleanup []CleanupItem
	if p.RemoveSource {
		if err := tx.DeleteObjectFromBackend(ctx, key, src.BackendName); err != nil {
			return placeOutcome{}, err
		}
		deltas.add(src.BackendName, -src.SizeBytes)
		queued, err := enqueueRemoved(ctx, tx, key, p.Reasons.SourceDelete, []removedCopy{{
			BackendName: src.BackendName, StorageKey: src.StorageKey, SizeBytes: src.SizeBytes,
		}})
		if err != nil {
			return placeOutcome{}, err
		}
		cleanup = queued
	}
	if err := tx.InsertObjectLocation(ctx, placed); err != nil {
		return placeOutcome{}, fmt.Errorf("insert placed copy on %s: %w", placed.BackendName, err)
	}
	deltas.add(placed.BackendName, placed.SizeBytes)
	if err := chargeStripes(ctx, tx, key, deltas); err != nil {
		return placeOutcome{}, err
	}
	return placeOutcome{placed: placed.SizeBytes, cleanup: cleanup}, nil
}

// refusal reports why the locked copy set cannot take the placement: the
// source is gone or replaced, the target already holds another copy, or an
// in-place placement would keep the source beside its replacement.
func (p *Placement) refusal(existing []ExistingCopy) error {
	if _, err := removableCopy(existing, p.Source, 0); err != nil {
		return err
	}
	target := p.Intent.BackendName
	if target == p.Source.BackendName {
		if !p.RemoveSource {
			return ErrTargetRefused
		}
		return nil
	}
	if _, onTarget := copyOnBackend(existing, target); onTarget {
		return ErrTargetRefused
	}
	return nil
}

// refuse drops the placement's intent and queues the bytes the caller copied,
// committing the refusal so they are not left unrecorded on the target.
func (p *Placement) refuse(ctx context.Context, tx TxAdapter, refusal error) (placeOutcome, error) {
	cleanup, err := dropIntent(ctx, tx, p.Intent, p.Reasons.Orphan)
	return placeOutcome{cleanup: cleanup, refusal: refusal}, err
}

// placedCopy is the row the placement inserts: the locked source at the
// intent's path, carrying the intent's form and size when the bytes changed.
// The content hash stays the source's, since it covers the bytes the client
// wrote, and a recorded compression decline no longer describes a new form.
func (p *Placement) placedCopy(src *ObjectLocation) *ObjectLocation {
	placed := src.CopyAt(p.Intent.BackendName, p.Intent.StorageKey)
	if !p.Reformed {
		return placed
	}
	placed.StoredForm = p.Intent.StoredForm
	placed.ContentHash = src.ContentHash
	placed.SizeBytes = p.Intent.SizeBytes
	placed.CompressionProbeSize, placed.CompressionProbeLevel = 0, ""
	return placed
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
func (o TxOps) ImportObject(ctx context.Context, req *ImportObjectRequest) (ImportOutcome, error) {
	return WithTxVal(ctx, o.runner, func(ctx context.Context, tx TxAdapter) (ImportOutcome, error) {
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
