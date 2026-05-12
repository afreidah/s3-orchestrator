// -------------------------------------------------------------------------------
// SQLite Pending Objects - In-Flight PUT Intent Tracking
//
// Author: Alex Freidah
//
// SQLite mirror of the Postgres pending_objects table. The write path inserts
// an intent before the backend PUT and the same transaction that commits the
// object_locations row clears the intent. Intents that survive a failed
// commit are resolved by the pending reaper, which calls PromotePending after
// HEADing the backend.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// INTENT LIFECYCLE
// -------------------------------------------------------------------------

// InsertPending records an in-flight PUT intent.
func (s *Store) InsertPending(ctx context.Context, p *core.PendingObject) error {
	keyID := nullableString(p.KeyID)
	plaintextSize := nullableInt64(p.PlaintextSize)
	contentHash := nullableString(p.ContentHash)
	encrypted := 0
	if p.Encrypted {
		encrypted = 1
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO pending_objects
		   (intent_id, object_key, backend_name, size_bytes,
		    encrypted, encryption_key, key_id, plaintext_size, content_hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.IntentID, p.ObjectKey, p.BackendName, p.SizeBytes,
		encrypted, p.EncryptionKey, keyID, plaintextSize, contentHash,
	); err != nil {
		return fmt.Errorf("insert pending object: %w", err)
	}
	return nil
}

// DeletePending removes a pending intent.
func (s *Store) DeletePending(ctx context.Context, intentID string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM pending_objects WHERE intent_id = ?`, intentID,
	); err != nil {
		return fmt.Errorf("delete pending object: %w", err)
	}
	return nil
}

// GetStalePending returns pending intents at or older than olderThan,
// oldest first, capped at limit.
func (s *Store) GetStalePending(ctx context.Context, olderThan time.Time, limit int) ([]core.PendingObject, error) {
	cutoff := olderThan.UTC().Format(time.RFC3339Nano)
	rows, err := s.db.QueryContext(ctx,
		`SELECT intent_id, object_key, backend_name, size_bytes,
		        encrypted, encryption_key, key_id, plaintext_size,
		        content_hash, created_at
		   FROM pending_objects
		  WHERE created_at <= ?
		  ORDER BY created_at ASC
		  LIMIT ?`,
		cutoff, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get stale pending objects: %w", err)
	}
	defer rows.Close()

	var out []core.PendingObject
	for rows.Next() {
		var (
			p             core.PendingObject
			encrypted     int
			keyID         sql.NullString
			plaintextSize sql.NullInt64
			contentHash   sql.NullString
			createdAt     string
			encKey        []byte
		)
		if err := rows.Scan(&p.IntentID, &p.ObjectKey, &p.BackendName, &p.SizeBytes,
			&encrypted, &encKey, &keyID, &plaintextSize, &contentHash, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan pending row: %w", err)
		}
		p.Encrypted = encrypted != 0
		p.EncryptionKey = encKey
		p.KeyID = nullStringValue(keyID)
		p.PlaintextSize = nullInt64Value(plaintextSize)
		p.ContentHash = nullStringValue(contentHash)
		if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
			p.CreatedAt = t
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending rows: %w", err)
	}
	return out, nil
}

// -------------------------------------------------------------------------
// REAPER SUPPORT
// -------------------------------------------------------------------------

// PendingDepth returns the total number of pending intents.
func (s *Store) PendingDepth(ctx context.Context) (int64, error) {
	var depth int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_objects`).Scan(&depth); err != nil {
		return 0, fmt.Errorf("count pending objects: %w", err)
	}
	return depth, nil
}

// DeletePendingByBackend removes every intent for a backend. Used during
// backend drain finalization so abandoned intents do not block the
// FK-protected delete of the backend's row in backend_quotas.
func (s *Store) DeletePendingByBackend(ctx context.Context, backendName string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM pending_objects WHERE backend_name = ?`, backendName,
	); err != nil {
		return fmt.Errorf("delete pending objects by backend: %w", err)
	}
	return nil
}

// PromotePending resolves a pending intent transactionally. SQLite serializes
// writers so no row-level lock is needed. The destination is inspected:
//
//   - If any object_locations row for the key was created after this intent
//     was inserted, the intent is provably stale and is dropped (Superseded):
//     the authoritative state is the newer row, and the intent's bytes are
//     either overwritten or stranded orphans.
//   - Otherwise the intent is promoted: any prior copies are cleared, the
//     new row is inserted, quotas are adjusted, and the pending row is
//     deleted in the same transaction.
//   - If the pending row is already gone, the call is a benign no-op.
//
// PromotePending delegates to core.PromotePending which composes the
// engine-agnostic claim, supersession check, commit, and same-tx
// delete of the pending row against the SQLite TxAdapter.
func (s *Store) PromotePending(ctx context.Context, p *core.PendingObject) (core.PendingPromoteResult, []core.DeletedCopy, error) {
	return core.PromotePending(ctx, s, p)
}

