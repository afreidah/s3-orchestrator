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

package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// -------------------------------------------------------------------------
// PENDING OBJECT OPERATIONS
// -------------------------------------------------------------------------

// InsertPending records an in-flight PUT intent. Called before the backend
// upload so a metadata commit failure cannot silently destroy the prior
// copy of an overwritten key.
func (s *Store) InsertPending(ctx context.Context, p *core.PendingObject) error {
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
func (s *Store) GetStalePending(ctx context.Context, olderThan time.Time, limit int) ([]core.PendingObject, error) {
	rows, err := s.queries.GetStalePendingObjects(ctx, db.GetStalePendingObjectsParams{
		OlderThan: pgTimestamptz(olderThan),
		MaxKeys:   int32(limit), //nolint:gosec // G115: limit is a small caller-controlled batch size
	})
	if err != nil {
		return nil, fmt.Errorf("get stale pending objects: %w", err)
	}
	return mapSlice(rows, pendingFromRow), nil
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

// PromotePending resolves a pending intent transactionally. Delegates to
// core.PromotePending which composes the lock, supersession check,
// commit, and same-tx pending delete against the per-engine TxAdapter.
func (s *Store) PromotePending(ctx context.Context, p *core.PendingObject) (core.PendingPromoteResult, []core.DeletedCopy, error) {
	return core.PromotePending(ctx, s, p)
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// pendingInsertParams maps a PendingObject onto the sqlc insert struct.
// Pointer-typed columns stay nil when their string/int64 source is empty
// so the database stores SQL NULL rather than the zero value.
func pendingInsertParams(p *core.PendingObject) db.InsertPendingObjectParams {
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
func pendingFromRow(row *db.PendingObject) core.PendingObject {
	p := core.PendingObject{
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
