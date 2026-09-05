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
// the backend had no room, which is the caller's cue to try the next candidate.
//
// The headroom is read inside the statement rather than from a snapshot: every
// instance's committed bytes, orphans, and writes in progress are rows here, so
// two instances admitting at once are judged against the same totals. A
// bytes_limit of zero is unlimited, matching every other reader of the column.
func (s *Store) InsertPendingIfFits(ctx context.Context, p *core.PendingObject) (bool, error) {
	keyID := nullableString(p.KeyID)
	plaintextSize := nullableInt64(p.PlaintextSize)
	contentHash := nullableString(p.ContentHash)
	encrypted := 0
	if p.Encrypted {
		encrypted = 1
	}
	res, err := s.db.ExecContext(ctx,
		// created_at is set here rather than left to the column default: the
		// default renders milliseconds while every other write renders
		// nanoseconds, and the reaper's min-age check compares the two as text.
		`INSERT INTO pending_objects
		   (intent_id, object_key, backend_name, size_bytes,
		    encrypted, encryption_key, key_id, plaintext_size, content_hash,
		    compression_algorithm, compression_level, compression_format_version, logical_size,
		    etag, content_type, user_metadata,
		    created_at)
		 SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		 FROM backend_quotas q
		 LEFT JOIN (
		     SELECT backend_name, SUM(bytes_used) AS bytes_used
		     FROM backend_quota_stripes GROUP BY backend_name
		 ) s ON s.backend_name = q.backend_name
		 LEFT JOIN (
		     SELECT mu.backend_name, SUM(mp.size_bytes) AS inflight
		     FROM multipart_uploads mu
		     JOIN multipart_parts mp ON mp.upload_id = mu.upload_id
		     GROUP BY mu.backend_name
		 ) m ON m.backend_name = q.backend_name
		 LEFT JOIN (
		     SELECT backend_name, SUM(size_bytes) AS inflight
		     FROM pending_objects GROUP BY backend_name
		 ) pi ON pi.backend_name = q.backend_name
		 WHERE q.backend_name = ?
		   AND (q.bytes_limit = 0
		        OR q.bytes_limit
		           - MAX(0, COALESCE(s.bytes_used, 0))
		           - q.orphan_bytes
		           - COALESCE(m.inflight, 0)
		           - COALESCE(pi.inflight, 0) >= ?)`,
		p.IntentID, p.ObjectKey, p.BackendName, p.SizeBytes,
		encrypted, p.EncryptionKey, keyID, plaintextSize, contentHash,
		nullableString(p.CompressionAlgorithm), nullableString(p.CompressionLevel),
		nullableInt64(int64(p.CompressionFormatVersion)), nullableInt64(p.LogicalSize),
		identityETag(p.Identity), identityContentType(p.Identity), identityMetadataJSON(p.Identity),
		now(),
		p.BackendName, p.SizeBytes,
	)
	if err != nil {
		return false, fmt.Errorf("insert pending object: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("check pending insert: %w", err)
	}
	return n > 0, nil
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
	cutoff := formatTime(olderThan)
	rows, err := s.db.QueryContext(ctx,
		`SELECT intent_id, object_key, backend_name, size_bytes,
		        encrypted, encryption_key, key_id, plaintext_size,
		        content_hash, compression_algorithm, compression_level,
		        compression_format_version, logical_size, created_at,
		        etag, content_type, user_metadata
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
			p             core.PendingObject
			encrypted     int
			keyID         sql.NullString
			plaintextSize sql.NullInt64
			contentHash   sql.NullString
			compAlgorithm sql.NullString
			compLevel     sql.NullString
			compVersion   sql.NullInt64
			logicalSize   sql.NullInt64
			createdAt     string
			encKey        []byte
			etag          sql.NullString
			contentType   sql.NullString
			userMetadata  sql.NullString
		)
		if err := rows.Scan(&p.IntentID, &p.ObjectKey, &p.BackendName, &p.SizeBytes,
			&encrypted, &encKey, &keyID, &plaintextSize, &contentHash,
			&compAlgorithm, &compLevel, &compVersion, &logicalSize, &createdAt,
			&etag, &contentType, &userMetadata,
		); err != nil {
			return core.PendingObject{}, fmt.Errorf("scan pending row: %w", err)
		}
		p.Identity = identityFromColumns(etag, contentType, userMetadata)
		p.Encrypted = encrypted != 0
		p.EncryptionKey = encKey
		p.KeyID = nullStringValue(keyID)
		p.PlaintextSize = nullInt64Value(plaintextSize)
		p.ContentHash = nullStringValue(contentHash)
		p.CompressionAlgorithm = nullStringValue(compAlgorithm)
		p.CompressionLevel = nullStringValue(compLevel)
		p.CompressionFormatVersion = int(nullInt64Value(compVersion))
		p.LogicalSize = nullInt64Value(logicalSize)
		if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
			p.CreatedAt = t
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
