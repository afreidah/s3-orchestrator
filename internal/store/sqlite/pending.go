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

// InsertPendingIfFits claims the bytes and records the intent in one statement,
// so admission and the durable record of it cannot disagree. Reports false when
// the backend had no room or carries a flag outside accepts, which is the
// caller's cue to try the next candidate. SQLite serializes writers, so the
// headroom it reads already counts every claim committed before it.
func (a *sqliteTxAdapter) InsertPendingIfFits(ctx context.Context, p *core.PendingObject, accepts core.BackendFlags) (bool, error) {
	return a.execChanged(ctx, "insert pending object",
		// created_at is set here rather than left to the column default: the
		// default renders milliseconds while every other write renders
		// nanoseconds, and the reaper's min-age check compares the two as text.
		`INSERT INTO pending_objects
		   (intent_id, object_key, storage_key, backend_name, size_bytes,
		    encrypted, encryption_key, key_id, plaintext_size, content_hash,
		    compression_algorithm, compression_level, compression_format_version, logical_size,
		    etag, content_type, user_metadata,
		    created_at, role)
		 SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		 FROM backend_capacity c
		 WHERE c.backend_name = ?
		   AND (c.flags & ~?) = 0
		   AND (c.available_bytes IS NULL OR c.available_bytes >= ?)`,
		p.IntentID, p.ObjectKey, core.StoragePath(p.ObjectKey, p.StorageKey), p.BackendName, p.SizeBytes,
		boolToInt(p.Encrypted), p.EncryptionKey, nullableString(p.KeyID), nullableInt64(p.PlaintextSize), nullableString(p.ContentHash),
		nullableString(p.CompressionAlgorithm), nullableString(p.CompressionLevel),
		nullableInt64(int64(p.CompressionFormatVersion)), nullableInt64(p.LogicalSize),
		identityETag(p.Identity), identityContentType(p.Identity), identityMetadataJSON(p.Identity),
		now(), string(p.RoleOrDefault()),
		p.BackendName, accepts, p.SizeBytes,
	)
}

// DeletePending removes a pending intent.
func (s *Store) DeletePending(ctx context.Context, intentID string) error {
	return s.direct().DeletePending(ctx, intentID)
}

// GetStalePending returns pending intents at or older than olderThan,
// oldest first, capped at limit.
func (s *Store) GetStalePending(ctx context.Context, olderThan time.Time, limit int) ([]core.PendingObject, error) {
	cutoff := formatTime(olderThan)
	rows, err := s.db.QueryContext(ctx,
		`SELECT intent_id, object_key, storage_key, backend_name, size_bytes,
		        encrypted, encryption_key, key_id, plaintext_size,
		        content_hash, compression_algorithm, compression_level,
		        compression_format_version, logical_size, created_at,
		        etag, content_type, user_metadata, role
		   FROM pending_objects
		  WHERE created_at <= ?
		  ORDER BY created_at ASC
		  LIMIT ?`,
		cutoff, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get stale pending objects: %w", err)
	}
	return collectRows(rows, "pending rows", func(rows *sql.Rows) (core.PendingObject, error) {
		var (
			p            core.PendingObject
			form         storedFormColumns
			createdAt    string
			etag         sql.NullString
			contentType  sql.NullString
			userMetadata sql.NullString
			role         string
		)
		dest := append([]any{&p.IntentID, &p.ObjectKey, &p.StorageKey, &p.BackendName, &p.SizeBytes},
			form.scanDest(&p.StoredForm)...)
		if err := rows.Scan(append(dest, &createdAt, &etag, &contentType, &userMetadata, &role)...); err != nil {
			return core.PendingObject{}, fmt.Errorf("scan pending row: %w", err)
		}
		form.apply(&p.StoredForm)
		p.Role = core.PendingRole(role)
		p.Identity = identityFromColumns(etag, contentType, userMetadata)
		var err error
		if p.CreatedAt, err = parseTime(createdAt); err != nil {
			return core.PendingObject{}, fmt.Errorf(errInvalidTimestamp, createdAt, err)
		}
		return p, nil
	})
}

// -------------------------------------------------------------------------
// REAPER SUPPORT
// -------------------------------------------------------------------------

// PendingDepth returns the total number of pending intents.
func (s *Store) PendingDepth(ctx context.Context) (int64, error) {
	return s.countRows(ctx, "pending objects", `SELECT COUNT(*) FROM pending_objects`)
}
