// -------------------------------------------------------------------------------
// Pending Objects Store - In-Flight PUT Intent Tracking
//
// Author: Alex Freidah
//
// Persistence layer for the pending_objects table. The write path inserts an
// intent before the backend PUT and removes it on a successful metadata
// commit. Intents that survive a failed commit are resolved by the pending
// reaper via PromotePending, which mirrors RecordObject's quota and copy
// bookkeeping while honoring the conservative ambiguous-case contract: if
// the destination backend already holds a row for the key, the reaper
// leaves the intent for operator review rather than guessing whether the
// bytes-on-disk belong to the failed PUT or to a subsequent overwrite.
// -------------------------------------------------------------------------------

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	db "github.com/afreidah/s3-orchestrator/internal/store/sqlc"
)

// -------------------------------------------------------------------------
// PENDING OBJECT OPERATIONS
// -------------------------------------------------------------------------

// InsertPending records an in-flight PUT intent. Called before the backend
// upload so a metadata commit failure cannot silently destroy the prior
// copy of an overwritten key.
func (s *Store) InsertPending(ctx context.Context, p *PendingObject) error {
	if err := s.queries.InsertPendingObject(ctx, pendingInsertParams(p)); err != nil {
		return fmt.Errorf("insert pending object: %w", err)
	}
	return nil
}

// DeletePending removes a pending intent. Called by the write path on a
// successful commit (atomically inside the same transaction as RecordObject)
// and by the reaper on the HEAD-404 path.
func (s *Store) DeletePending(ctx context.Context, intentID string) error {
	if err := s.queries.DeletePendingObject(ctx, intentID); err != nil {
		return fmt.Errorf("delete pending object: %w", err)
	}
	return nil
}

// GetStalePending returns pending intents whose created_at is at or before
// olderThan, oldest first, capped at limit rows. Used by the reaper to
// resolve intents that have outlived their original PUT's commit window.
func (s *Store) GetStalePending(ctx context.Context, olderThan time.Time, limit int) ([]PendingObject, error) {
	rows, err := s.queries.GetStalePendingObjects(ctx, db.GetStalePendingObjectsParams{
		OlderThan: pgTimestamptz(olderThan),
		MaxKeys:   int32(limit), //nolint:gosec // G115: limit is a small caller-controlled batch size
	})
	if err != nil {
		return nil, fmt.Errorf("get stale pending objects: %w", err)
	}
	out := make([]PendingObject, len(rows))
	for i := range rows {
		out[i] = pendingFromRow(&rows[i])
	}
	return out, nil
}

// PendingDepth returns the total number of pending intents. Used by the
// reaper to publish a depth gauge.
func (s *Store) PendingDepth(ctx context.Context) (int64, error) {
	depth, err := s.queries.CountPendingObjects(ctx)
	if err != nil {
		return 0, fmt.Errorf("count pending objects: %w", err)
	}
	return depth, nil
}

// DeletePendingByBackend removes every intent for a backend. Called during
// drain finalization and admin remove so abandoned intents do not block
// the FK-cascade delete of the backend's row in backend_quotas.
func (s *Store) DeletePendingByBackend(ctx context.Context, backendName string) error {
	if err := s.queries.DeletePendingObjectsByBackend(ctx, backendName); err != nil {
		return fmt.Errorf("delete pending objects by backend: %w", err)
	}
	return nil
}

// PromotePending resolves a pending intent transactionally. The pending row
// is locked first so two reaper instances cannot promote the same intent
// concurrently. The destination is then inspected:
//
//   - If no row for (object_key, backend_name) exists in object_locations,
//     the pending row is promoted: any displaced copies on other backends
//     are cleared, the new row is inserted with the pending's metadata,
//     quotas are adjusted, and the pending row is deleted in the same tx.
//     The displaced copies are returned so the caller can enqueue cleanup.
//
//   - If a row already exists for the same (object_key, backend_name), the
//     promotion is ambiguous: the bytes on disk may belong to the pending
//     intent or to a subsequent successful PUT. The transaction is rolled
//     back and PendingPromoteAmbiguous is returned. The reaper records
//     this for operator review and leaves the row in place.
//
//   - If the pending row is already gone (another reaper resolved it
//     between GetStalePending and the lock acquire), the transaction is
//     rolled back and PendingPromoteAlreadyResolved is returned.
func (s *Store) PromotePending(ctx context.Context, p *PendingObject) (PendingPromoteResult, []DeletedCopy, error) {
	out, err := withTxVal(s, ctx, func(qtx *db.Queries) (promoteOutcome, error) {
		return promotePendingTx(ctx, qtx, p)
	})
	if err != nil {
		// On txn error the result code is meaningless; callers check err
		// first. Returning Ambiguous keeps the legacy contract intact for
		// any caller that ignores err and inspects the result anyway.
		return PendingPromoteAmbiguous, nil, err
	}
	return out.result, out.displaced, nil
}

// promoteOutcome carries the resolution result and any displaced copies
// out of the transactional body so the caller can fan out cleanups.
type promoteOutcome struct {
	result    PendingPromoteResult
	displaced []DeletedCopy
}

// promotePendingTx is the transactional body of PromotePending. Split out
// so the orchestration reads as four ordered steps: lock, supersession
// check, commit, and the same-tx delete of the pending row.
func promotePendingTx(ctx context.Context, qtx *db.Queries, p *PendingObject) (promoteOutcome, error) {
	resolved, err := lockPendingRow(ctx, qtx, p.IntentID)
	if err != nil {
		return promoteOutcome{}, err
	}
	if resolved {
		return promoteOutcome{result: PendingPromoteAlreadyResolved}, nil
	}

	if err := qtx.LockObjectKeyForWrite(ctx, p.ObjectKey); err != nil {
		return promoteOutcome{}, fmt.Errorf("lock object key: %w", err)
	}
	existing, err := qtx.GetExistingCopiesForUpdate(ctx, p.ObjectKey)
	if err != nil {
		return promoteOutcome{}, fmt.Errorf("query existing copies: %w", err)
	}

	if intentSuperseded(existing, p.CreatedAt) {
		if err := qtx.DeletePendingObject(ctx, p.IntentID); err != nil {
			return promoteOutcome{}, fmt.Errorf("delete superseded pending row: %w", err)
		}
		return promoteOutcome{result: PendingPromoteSuperseded}, nil
	}

	return commitPromotion(ctx, qtx, p, existing)
}

// lockPendingRow takes a SELECT FOR UPDATE on the pending row. Returns
// (true, nil) if the row is gone — another reaper instance already
// resolved it. Other errors propagate.
func lockPendingRow(ctx context.Context, qtx *db.Queries, intentID string) (alreadyResolved bool, err error) {
	if _, err := qtx.LockPendingForUpdate(ctx, intentID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return true, nil
		}
		return false, fmt.Errorf("lock pending row: %w", err)
	}
	return false, nil
}

// intentSuperseded reports whether any existing object_locations row was
// created after the pending intent. A newer row means a successful write
// happened later and is authoritative, so the intent is provably stale —
// dropping it avoids head-of-line blocking and prevents the reaper from
// corrupting metadata that a retry already committed.
func intentSuperseded(existing []db.GetExistingCopiesForUpdateRow, intentCreatedAt time.Time) bool {
	for _, ec := range existing {
		if ec.CreatedAt.Valid && ec.CreatedAt.Time.After(intentCreatedAt) {
			return true
		}
	}
	return false
}

// commitPromotion finalises a non-superseded intent: clears prior copies,
// inserts the new object_location row, increments the destination quota,
// and deletes the pending row in the same transaction. Returns the
// displaced copies so the caller can enqueue them for cleanup.
func commitPromotion(ctx context.Context, qtx *db.Queries, p *PendingObject, existing []db.GetExistingCopiesForUpdateRow) (promoteOutcome, error) {
	displaced, err := clearExistingCopies(ctx, qtx, p.ObjectKey, p.BackendName, existing)
	if err != nil {
		return promoteOutcome{}, err
	}
	if err := qtx.InsertObjectLocation(ctx, insertParamsFromEnc(p.ObjectKey, p.BackendName, p.SizeBytes, pendingEncryptionMeta(p))); err != nil {
		return promoteOutcome{}, fmt.Errorf("insert promoted location: %w", err)
	}
	if err := incrementBackendQuota(ctx, qtx, p.BackendName, p.SizeBytes); err != nil {
		return promoteOutcome{}, err
	}
	if err := qtx.DeletePendingObject(ctx, p.IntentID); err != nil {
		return promoteOutcome{}, fmt.Errorf("delete promoted pending row: %w", err)
	}
	return promoteOutcome{result: PendingPromoteCommitted, displaced: displaced}, nil
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// pendingInsertParams maps a PendingObject onto the sqlc insert struct.
// Pointer-typed columns stay nil when their string/int64 source is empty
// so the database stores SQL NULL rather than the zero value.
func pendingInsertParams(p *PendingObject) db.InsertPendingObjectParams {
	params := db.InsertPendingObjectParams{
		IntentID:      p.IntentID,
		ObjectKey:     p.ObjectKey,
		BackendName:   p.BackendName,
		SizeBytes:     p.SizeBytes,
		Encrypted:     p.Encrypted,
		EncryptionKey: p.EncryptionKey,
	}
	if p.KeyID != "" {
		params.KeyID = &p.KeyID
	}
	if p.PlaintextSize != 0 {
		params.PlaintextSize = &p.PlaintextSize
	}
	if p.ContentHash != "" {
		params.ContentHash = &p.ContentHash
	}
	return params
}

// pendingFromRow maps a sqlc PendingObject row onto the package type,
// dereferencing nullable columns to their zero value when SQL NULL.
func pendingFromRow(row *db.PendingObject) PendingObject {
	p := PendingObject{
		IntentID:      row.IntentID,
		ObjectKey:     row.ObjectKey,
		BackendName:   row.BackendName,
		SizeBytes:     row.SizeBytes,
		Encrypted:     row.Encrypted,
		EncryptionKey: row.EncryptionKey,
		CreatedAt:     row.CreatedAt.Time,
	}
	if row.KeyID != nil {
		p.KeyID = *row.KeyID
	}
	if row.PlaintextSize != nil {
		p.PlaintextSize = *row.PlaintextSize
	}
	if row.ContentHash != nil {
		p.ContentHash = *row.ContentHash
	}
	return p
}

// pendingEncryptionMeta builds an EncryptionMeta from a PendingObject so
// the promoted object_locations row carries the same encryption + integrity
// metadata as the original PUT recorded.
func pendingEncryptionMeta(p *PendingObject) *EncryptionMeta {
	if !p.Encrypted && p.ContentHash == "" {
		return nil
	}
	return &EncryptionMeta{
		Encrypted:     p.Encrypted,
		EncryptionKey: p.EncryptionKey,
		KeyID:         p.KeyID,
		PlaintextSize: p.PlaintextSize,
		ContentHash:   p.ContentHash,
	}
}
