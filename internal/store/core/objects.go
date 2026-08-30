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
	"context"
	"fmt"
	"slices"
)

// -------------------------------------------------------------------------
// RECORD OBJECT
// -------------------------------------------------------------------------

// RecordObjectRequest is one committed write: where the object landed, how its
// bytes are stored, the tag set it carries, and the pending intent it resolves.
//
// Tags are separate from Form because Form describes the bytes and rides
// through replication and moves; a tag set carried there would be re-inserted
// on every copy that lands. Tags describe the object, and there is one set of
// them however many copies exist.
type RecordObjectRequest struct {
	Key      string
	Backend  string
	Size     int64
	Form     *StoredForm
	Identity *ObjectIdentity
	Tags     []Tag
	IntentID string
}

// RecordObject records an object's location and updates the backend
// quota. On overwrite, all existing copies (including replicas) are
// removed and their quotas decremented before inserting the new
// primary copy. Returns the displaced copies for cleanup.
//
// A non-empty IntentID additionally deletes the matching pending_objects row
// inside the same transaction, so a successful PUT's intent never outlives the
// location it was covering.
func RecordObject(ctx context.Context, runner Runner, req *RecordObjectRequest) ([]DeletedCopy, error) {
	if err := ValidateTags(req.Tags); err != nil {
		return nil, err
	}
	return WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) ([]DeletedCopy, error) {
		return recordObjectTx(ctx, tx, req)
	})
}

// recordObjectTx is the shared transactional body. All per-backend quota
// deltas are aggregated and applied in stable backend_name order via
// applyQuotaDeltas so concurrent overwrites can never deadlock on
// backend_quotas row locks.
func recordObjectTx(ctx context.Context, tx TxAdapter, req *RecordObjectRequest) ([]DeletedCopy, error) {
	if err := tx.AcquireKeyLock(ctx, req.Key); err != nil {
		return nil, err
	}
	existing, err := tx.GetExistingCopiesForUpdate(ctx, req.Key)
	if err != nil {
		return nil, err
	}
	deltas := make(map[string]int64, len(existing)+1)
	displaced, err := clearExistingCopies(ctx, tx, req.Key, req.Backend, existing, deltas)
	if err != nil {
		return nil, err
	}
	// A PUT is a full replacement, so the object landing here starts from an
	// empty set and takes only the tags this write carried. Unconditional
	// rather than gated on len(existing): a key with no copies but leftover
	// tag rows still starts clean, which also sweeps anything a bug elsewhere
	// orphaned.
	//
	// Written here rather than by the caller afterwards so the object and its
	// tags commit together; two calls would leave the object tagless whenever
	// the second one failed.
	if err := replaceObjectTagsTx(ctx, tx, req.Key, req.Tags); err != nil {
		return nil, err
	}
	if err := tx.InsertObjectLocation(ctx, objectFromStoredForm(req.Key, req.Backend, req.Size, req.Form, req.Identity)); err != nil {
		return nil, fmt.Errorf("insert object location: %w", err)
	}
	deltas[req.Backend] += req.Size
	if err := applyQuotaDeltas(ctx, tx, deltas); err != nil {
		return nil, err
	}
	if req.IntentID != "" {
		if err := tx.DeletePending(ctx, req.IntentID); err != nil {
			return nil, fmt.Errorf("clear pending intent: %w", err)
		}
	}
	return displaced, nil
}

// -------------------------------------------------------------------------
// DELETE OBJECT
// -------------------------------------------------------------------------

// DeleteObject removes all copies of an object and decrements their
// quotas. Returns ErrObjectNotFound if the object doesn't exist;
// otherwise returns the deleted copies for cleanup. Quota deltas apply
// in stable backend_name order.
func DeleteObject(ctx context.Context, runner Runner, key string) ([]DeletedCopy, error) {
	return WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) ([]DeletedCopy, error) {
		// Ahead of the row read, matching recordObjectTx. A tagging call
		// touches object_tags without touching object_locations, so the row
		// locks below do not exclude it; only the key lock does. Taking it in
		// the same order everywhere is what keeps the two paths from
		// deadlocking against each other.
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
		copies := make([]DeletedCopy, len(existing))
		deltas := make(map[string]int64, len(existing))
		for i, ec := range existing {
			copies[i] = DeletedCopy{BackendName: ec.BackendName, SizeBytes: ec.SizeBytes}
			deltas[ec.BackendName] -= ec.SizeBytes
		}
		if err := applyQuotaDeltas(ctx, tx, deltas); err != nil {
			return nil, err
		}
		return copies, nil
	})
}

// -------------------------------------------------------------------------
// DELETE OBJECTS BATCH
// -------------------------------------------------------------------------

// DeleteObjectsBatch removes every supplied key (and all its replicas)
// in a single transaction, decrementing each affected backend's quota
// once by the sum of removed bytes. Returns a map from key to its
// displaced copies so the caller can fan out to the backend cleanup
// path. Keys with no copies on disk are absent from the returned map
// (treated as success-with-nothing-to-clean-up). Empty input yields an
// empty map without opening a transaction.
func DeleteObjectsBatch(ctx context.Context, runner Runner, keys []string) (map[string][]DeletedCopy, error) {
	if len(keys) == 0 {
		return map[string][]DeletedCopy{}, nil
	}
	return WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) (map[string][]DeletedCopy, error) {
		if err := lockKeysInOrder(ctx, tx, keys); err != nil {
			return nil, err
		}
		rows, err := tx.GetCopiesForKeysForUpdate(ctx, keys)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return map[string][]DeletedCopy{}, nil
		}
		if err := tx.DeleteObjectsByKeys(ctx, keys); err != nil {
			return nil, fmt.Errorf("delete object copies by keys: %w", err)
		}
		if err := clearTagsForKeys(ctx, tx, keys); err != nil {
			return nil, err
		}
		// Per-key copies for the caller, plus per-backend totals so we
		// decrement each backend's quota exactly once instead of once
		// per displaced copy. Deltas apply in stable backend_name
		// order via applyQuotaDeltas; the previous
		// map-iteration order was non-deterministic and let two
		// concurrent batch deletes deadlock on backend_quotas locks.
		copies := make(map[string][]DeletedCopy, len(keys))
		deltas := make(map[string]int64)
		for _, r := range rows {
			copies[r.ObjectKey] = append(copies[r.ObjectKey], DeletedCopy{
				BackendName: r.BackendName,
				SizeBytes:   r.SizeBytes,
			})
			deltas[r.BackendName] -= r.SizeBytes
		}
		if err := applyQuotaDeltas(ctx, tx, deltas); err != nil {
			return nil, err
		}
		return copies, nil
	})
}

// lockKeysInOrder takes the per-key lock for every supplied key, sorted and
// deduplicated first.
//
// Sorted for the same reason applyQuotaDeltas sorts backends: two concurrent
// batches sharing keys would otherwise take the same locks in caller-supplied
// order and deadlock. Sorted on a copy so the caller's slice is left alone.
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

// DeleteObjectLocation removes a single (key, backend) copy from the
// object ledger and debits the backend's bytes_used by that copy's size
// in the same transaction, keeping bytes_used in agreement with
// SUM(object_locations.size_bytes). Its callers are the paths that drop a
// row because the backend no longer holds the object: reconcile's
// stale-entry deleter, the replicator's stale-source prune, and drain's
// replica-source removal and purge. A row that is already gone is a
// benign no-op that leaves the quota untouched.
//
// The size comes from the same FOR-UPDATE re-read that guards the delete,
// so a concurrent overwrite cannot make the debit disagree with the row
// that was actually removed.
func DeleteObjectLocation(ctx context.Context, runner Runner, key, backendName string) error {
	return runner.WithTx(ctx, func(ctx context.Context, tx TxAdapter) error {
		if err := tx.AcquireKeyLock(ctx, key); err != nil {
			return err
		}
		existing, err := tx.GetExistingCopiesForUpdate(ctx, key)
		if err != nil {
			return err
		}
		size, found := copySizeForBackend(existing, backendName)
		if !found {
			return nil
		}
		if err := tx.DeleteObjectFromBackend(ctx, key, backendName); err != nil {
			return err
		}
		// Only the copy that was the object's last one takes its tags with
		// it. Removing one replica of a multi-copy object leaves the object
		// alive, and dropping its tags there would be silent data loss. The
		// copy list is already in hand for the quota debit, so this costs
		// no extra query.
		if len(existing) == 1 {
			if err := clearTagsForKey(ctx, tx, key); err != nil {
				return err
			}
		}
		return tx.DecrementBackendQuota(ctx, backendName, size)
	})
}

// -------------------------------------------------------------------------
// MOVE OBJECT LOCATION
// -------------------------------------------------------------------------

// MoveObjectLocation atomically moves a copy of an object from one
// backend to another. Uses row-level locks to prevent races. Returns
// (0, nil) if the source copy is gone or the target already has a
// copy.
func MoveObjectLocation(ctx context.Context, runner Runner, key, fromBackend, toBackend string) (int64, error) {
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
		// The description of the bytes is carried through the same conversion
		// every other path that moves them verbatim uses, rather than a
		// hand-listed subset of the source row's fields. A field omitted here is
		// a column describing bytes the moved copy then contradicts, which is
		// how this path came to drop the compression columns.
		dest := objectFromStoredForm(key, toBackend, src.SizeBytes, StoredFormFromLocation(src), src.Identity)
		if err := tx.InsertObjectLocation(ctx, dest); err != nil {
			return 0, err
		}
		if err := carryCompressionProbe(ctx, tx, src, key, toBackend); err != nil {
			return 0, err
		}
		// Apply both quota deltas in stable order: a concurrent
		// move in the opposite direction (b1->b2 vs b2->b1) used to
		// lock the two rows in opposite sequences and deadlock.
		if err := applyQuotaDeltas(ctx, tx, map[string]int64{
			fromBackend: -src.SizeBytes,
			toBackend:   src.SizeBytes,
		}); err != nil {
			return 0, err
		}
		return src.SizeBytes, nil
	})
}

// carryCompressionProbe copies a source copy's compression measurement onto the
// destination row of a move.
//
// A measurement of what the encoder produced for these bytes is not a
// description of them, so it rides here rather than through StoredForm. It
// still has to ride: the move is verbatim, so what was measured on the source
// holds on the destination, and dropping it has the next compression pass
// download the copy to learn it again.
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

// ImportObject records a pre-existing object in the database without
// overwriting. Returns true if the object was newly imported, false if
// ImportOutcome reports what an import did with a discovered key. A caller
// that only wants a count still has to tell a suppressed import from a row
// that was already there: the first says a delete is outstanding and the
// bytes are an orphan, the second says nothing at all.
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

// it already existed for this backend. Used by reconcile and the sync
// subcommand to bring existing bucket objects under proxy management.
//
// unmanaged marks an object that lives outside every configured virtual
// bucket prefix. It is still recorded, because it occupies backend quota
// that placement decisions have to account for, but the workers leave it
// alone.
//
// form carries what the caller established about the bytes on the backend.
// Passing nil records the object as plaintext, so callers that import bytes
// they have not inspected will publish ciphertext as if it were the object.
// A key whose delete is still outstanding is left alone. The bytes are on the
// backend because a delete could not reach it, not because the object is meant
// to be there, and importing them undoes the delete: the object comes back
// live, the replicator spreads it to reach the replication factor, and its
// created_at restarts so any lifecycle rule that expired it waits another full
// window. The cleanup queue already tracks the orphan and its bytes are already
// counted against the backend, so leaving the row absent is the accurate state.
func ImportObject(ctx context.Context, runner Runner, key, backend string, size int64, unmanaged bool, form *StoredForm) (ImportOutcome, error) {
	return WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) (ImportOutcome, error) {
		pending, err := tx.HasPendingCleanup(ctx, key, backend)
		if err != nil {
			return ImportSkippedExisting, err
		}
		if pending {
			return ImportSkippedPendingCleanup, nil
		}

		// No identity: an imported object's ETag is whatever the backend
		// reports, which is not known here and is not the same answer on every
		// copy. The first read that has to ask the backend records what it got
		// for every copy, so the value settles on first use instead of being
		// guessed at import.
		loc := objectFromStoredForm(key, backend, size, form, nil)
		loc.Unmanaged = unmanaged
		inserted, err := tx.InsertObjectLocationIfNotExists(ctx, loc)
		if err != nil {
			return ImportSkippedExisting, err
		}
		if !inserted {
			return ImportSkippedExisting, nil
		}
		if err := tx.IncrementBackendQuota(ctx, backend, size); err != nil {
			return ImportSkippedExisting, err
		}
		return ImportInserted, nil
	})
}
