// -------------------------------------------------------------------------------
// SQLite Object Operations - Location CRUD, Listing, and Integrity
//
// Author: Alex Freidah
//
// Implements object location CRUD, prefix-based listing with deduplication,
// expired object queries, backend-scoped listing, import, and integrity
// verification operations. Uses GROUP BY + MIN(rowid) subqueries to replace
// PostgreSQL's DISTINCT ON for replica deduplication.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store"
)

// likeEscaper escapes SQL LIKE wildcards in prefix strings.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// GetAllObjectLocations returns all copies of an object, ordered by created_at
// ascending (oldest/primary first). Used for read failover.
func (s *Store) GetAllObjectLocations(ctx context.Context, key string) ([]store.ObjectLocation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT object_key, backend_name, size_bytes, encrypted, encryption_key,
		       key_id, plaintext_size, content_hash, created_at
		FROM object_locations
		WHERE object_key = ?
		ORDER BY created_at ASC`, key)
	if err != nil {
		return nil, fmt.Errorf("failed to get object locations: %w", err)
	}
	defer rows.Close()

	var locs []store.ObjectLocation
	for rows.Next() {
		loc, err := scanObjectLocation(rows)
		if err != nil {
			return nil, err
		}
		locs = append(locs, loc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate object locations: %w", err)
	}

	if len(locs) == 0 {
		return nil, store.ErrObjectNotFound
	}
	return locs, nil
}

// RecordObject atomically inserts or updates an object location, handling
// overwrites by returning displaced copies for cleanup.
func (s *Store) RecordObject(ctx context.Context, key, backend string, size int64, enc *store.EncryptionMeta) ([]store.DeletedCopy, error) {
	return withTxVal(s, ctx, func(tx *sql.Tx) ([]store.DeletedCopy, error) {
		// SQLite single-writer serializes; no advisory lock needed.

		// --- Collect all existing copies for this key ---
		existRows, err := tx.QueryContext(ctx, `
			SELECT backend_name, size_bytes
			FROM object_locations
			WHERE object_key = ?`, key)
		if err != nil {
			return nil, fmt.Errorf("failed to query existing copies: %w", err)
		}

		type existingCopy struct {
			backendName string
			sizeBytes   int64
		}
		var existing []existingCopy
		for existRows.Next() {
			var ec existingCopy
			if err := existRows.Scan(&ec.backendName, &ec.sizeBytes); err != nil {
				existRows.Close()
				return nil, fmt.Errorf("failed to scan existing copy: %w", err)
			}
			existing = append(existing, ec)
		}
		existRows.Close()
		if err := existRows.Err(); err != nil {
			return nil, fmt.Errorf("failed to iterate existing copies: %w", err)
		}

		// --- Delete all existing copies and decrement their quotas ---
		var displaced []store.DeletedCopy
		if len(existing) > 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM object_locations WHERE object_key = ?`, key); err != nil {
				return nil, fmt.Errorf("failed to delete existing copies: %w", err)
			}

			now := time.Now().UTC().Format(time.RFC3339Nano)
			for _, ec := range existing {
				if _, err := tx.ExecContext(ctx, `
					UPDATE backend_quotas
					SET bytes_used = MAX(0, bytes_used - ?), updated_at = ?
					WHERE backend_name = ?`, ec.sizeBytes, now, ec.backendName); err != nil {
					return nil, fmt.Errorf("failed to decrement quota for %s: %w", ec.backendName, err)
				}
				if ec.backendName != backend {
					displaced = append(displaced, store.DeletedCopy{
						BackendName: ec.backendName,
						SizeBytes:   ec.sizeBytes,
					})
				}
			}
		}

		// --- Build insert params with optional encryption metadata ---
		var (
			encrypted     bool
			encryptionKey []byte
			keyID         *string
			plaintextSize *int64
			contentHash   *string
		)
		if enc != nil {
			if enc.Encrypted {
				encrypted = true
				encryptionKey = enc.EncryptionKey
				keyID = &enc.KeyID
				plaintextSize = &enc.PlaintextSize
			}
			if enc.ContentHash != "" {
				contentHash = &enc.ContentHash
			}
		}

		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO object_locations
			  (object_key, backend_name, size_bytes, encrypted, encryption_key, key_id, plaintext_size, content_hash, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			key, backend, size, encrypted, encryptionKey, keyID, plaintextSize, contentHash, now); err != nil {
			return nil, fmt.Errorf("failed to insert object location: %w", err)
		}

		// --- Increment quota for new backend ---
		res, err := tx.ExecContext(ctx, `
			UPDATE backend_quotas
			SET bytes_used = bytes_used + ?, updated_at = ?
			WHERE backend_name = ?
			  AND (bytes_limit = 0 OR bytes_used + orphan_bytes + ? <= bytes_limit)`,
			size, now, backend, size)
		if err != nil {
			return nil, fmt.Errorf("failed to update quota: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("failed to check quota update: %w", err)
		}
		if n == 0 {
			return nil, store.ErrNoSpaceAvailable
		}

		return displaced, nil
	})
}

// DeleteObject removes all copies of an object and decrements their quotas.
// Returns all deleted copies, or ErrObjectNotFound if the object doesn't exist.
func (s *Store) DeleteObject(ctx context.Context, key string) ([]store.DeletedCopy, error) {
	return withTxVal(s, ctx, func(tx *sql.Tx) ([]store.DeletedCopy, error) {
		// --- Get all copies ---
		rows, err := tx.QueryContext(ctx, `
			SELECT backend_name, size_bytes
			FROM object_locations
			WHERE object_key = ?`, key)
		if err != nil {
			return nil, fmt.Errorf("failed to get object locations: %w", err)
		}

		type existingCopy struct {
			backendName string
			sizeBytes   int64
		}
		var existing []existingCopy
		for rows.Next() {
			var ec existingCopy
			if err := rows.Scan(&ec.backendName, &ec.sizeBytes); err != nil {
				rows.Close()
				return nil, fmt.Errorf("failed to scan existing copy: %w", err)
			}
			existing = append(existing, ec)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("failed to iterate existing copies: %w", err)
		}

		if len(existing) == 0 {
			return nil, store.ErrObjectNotFound
		}

		// --- Delete all location records ---
		if _, err := tx.ExecContext(ctx, `DELETE FROM object_locations WHERE object_key = ?`, key); err != nil {
			return nil, fmt.Errorf("failed to delete object locations: %w", err)
		}

		// --- Decrement quota for each backend ---
		now := time.Now().UTC().Format(time.RFC3339Nano)
		copies := make([]store.DeletedCopy, len(existing))
		for i, ec := range existing {
			copies[i] = store.DeletedCopy{
				BackendName: ec.backendName,
				SizeBytes:   ec.sizeBytes,
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE backend_quotas
				SET bytes_used = MAX(0, bytes_used - ?), updated_at = ?
				WHERE backend_name = ?`, ec.sizeBytes, now, ec.backendName); err != nil {
				return nil, fmt.Errorf("failed to decrement quota for %s: %w", ec.backendName, err)
			}
		}

		return copies, nil
	})
}

// ListObjects returns objects matching the given prefix, sorted by key.
// Supports pagination via startAfter and maxKeys. Returns one extra row to
// detect truncation. Uses a subquery with GROUP BY to deduplicate replicated
// objects (equivalent to DISTINCT ON in PostgreSQL).
func (s *Store) ListObjects(ctx context.Context, prefix, startAfter string, maxKeys int) (*store.ListObjectsResult, error) {
	if maxKeys <= 0 {
		maxKeys = 1000
	}

	escapedPrefix := likeEscaper.Replace(prefix)

	// Subquery with GROUP BY + MIN(rowid) replaces DISTINCT ON (object_key).
	rows, err := s.db.QueryContext(ctx, `
		SELECT ol.object_key, ol.backend_name, ol.size_bytes, ol.created_at
		FROM object_locations ol
		INNER JOIN (
			SELECT object_key, MIN(rowid) AS min_rowid
			FROM object_locations
			WHERE object_key LIKE ? || '%' ESCAPE '\'
			  AND object_key > ?
			GROUP BY object_key
		) dedup ON ol.rowid = dedup.min_rowid
		ORDER BY ol.object_key
		LIMIT ?`, escapedPrefix, startAfter, maxKeys+1)
	if err != nil {
		return nil, fmt.Errorf("failed to list objects: %w", err)
	}
	defer rows.Close()

	var objects []store.ObjectLocation
	for rows.Next() {
		var loc store.ObjectLocation
		var createdAt string
		if err := rows.Scan(&loc.ObjectKey, &loc.BackendName, &loc.SizeBytes, &createdAt); err != nil {
			return nil, fmt.Errorf("failed to scan object: %w", err)
		}
		loc.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		objects = append(objects, loc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate objects: %w", err)
	}

	result := &store.ListObjectsResult{}
	if len(objects) > maxKeys {
		result.IsTruncated = true
		result.NextContinuationToken = objects[maxKeys-1].ObjectKey
		result.Objects = objects[:maxKeys]
	} else {
		result.Objects = objects
	}

	return result, nil
}

// ListExpiredObjects returns one row per unique key matching the given prefix
// whose created_at is older than cutoff, up to limit rows. Used by lifecycle
// expiration to find objects eligible for deletion.
func (s *Store) ListExpiredObjects(ctx context.Context, prefix string, cutoff time.Time, limit int) ([]store.ObjectLocation, error) {
	escapedPrefix := likeEscaper.Replace(prefix)
	cutoffStr := cutoff.UTC().Format(time.RFC3339Nano)

	// Subquery with GROUP BY + MIN(rowid) replaces DISTINCT ON (object_key).
	rows, err := s.db.QueryContext(ctx, `
		SELECT ol.object_key, ol.backend_name, ol.size_bytes, ol.created_at
		FROM object_locations ol
		INNER JOIN (
			SELECT object_key, MIN(rowid) AS min_rowid
			FROM object_locations
			WHERE object_key LIKE ? || '%' ESCAPE '\'
			  AND created_at < ?
			GROUP BY object_key
		) dedup ON ol.rowid = dedup.min_rowid
		ORDER BY ol.object_key
		LIMIT ?`, escapedPrefix, cutoffStr, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list expired objects: %w", err)
	}
	defer rows.Close()

	var locs []store.ObjectLocation
	for rows.Next() {
		var loc store.ObjectLocation
		var createdAt string
		if err := rows.Scan(&loc.ObjectKey, &loc.BackendName, &loc.SizeBytes, &createdAt); err != nil {
			return nil, fmt.Errorf("failed to scan expired object: %w", err)
		}
		loc.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		locs = append(locs, loc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate expired objects: %w", err)
	}
	return locs, nil
}

// ListObjectsByBackend returns objects stored on a specific backend, ordered by
// size ascending (smallest first). Used by the rebalancer to find movable objects.
func (s *Store) ListObjectsByBackend(ctx context.Context, backendName string, limit int) ([]store.ObjectLocation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT object_key, backend_name, size_bytes, created_at
		FROM object_locations
		WHERE backend_name = ?
		ORDER BY size_bytes ASC
		LIMIT ?`, backendName, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list objects by backend: %w", err)
	}
	defer rows.Close()

	var locs []store.ObjectLocation
	for rows.Next() {
		var loc store.ObjectLocation
		var createdAt string
		if err := rows.Scan(&loc.ObjectKey, &loc.BackendName, &loc.SizeBytes, &createdAt); err != nil {
			return nil, fmt.Errorf("failed to scan object: %w", err)
		}
		loc.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		locs = append(locs, loc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate objects: %w", err)
	}
	return locs, nil
}

// MoveObjectLocation atomically moves a copy of an object from one backend to
// another. Returns (0, nil) if the source copy is gone or the target already
// has a copy.
func (s *Store) MoveObjectLocation(ctx context.Context, key, fromBackend, toBackend string) (int64, error) {
	return withTxVal(s, ctx, func(tx *sql.Tx) (int64, error) {
		// --- Check if target backend already has a copy ---
		var exists bool
		err := tx.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM object_locations
				WHERE object_key = ? AND backend_name = ?
			)`, key, toBackend).Scan(&exists)
		if err != nil {
			return 0, fmt.Errorf("failed to check target: %w", err)
		}
		if exists {
			return 0, nil
		}

		// --- Get the source row (no FOR UPDATE needed in SQLite) ---
		var (
			sizeBytes     int64
			encrypted     bool
			encryptionKey []byte
			keyID         *string
			plaintextSize *int64
			contentHash   *string
		)
		err = tx.QueryRowContext(ctx, `
			SELECT size_bytes, encrypted, encryption_key, key_id, plaintext_size, content_hash
			FROM object_locations
			WHERE object_key = ? AND backend_name = ?`, key, fromBackend).
			Scan(&sizeBytes, &encrypted, &encryptionKey, &keyID, &plaintextSize, &contentHash)
		if err == sql.ErrNoRows {
			return 0, nil
		}
		if err != nil {
			return 0, fmt.Errorf("failed to get source object: %w", err)
		}

		// --- Delete source row ---
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM object_locations
			WHERE object_key = ? AND backend_name = ?`, key, fromBackend); err != nil {
			return 0, fmt.Errorf("failed to delete source location: %w", err)
		}

		// --- Insert destination row preserving encryption and integrity metadata ---
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO object_locations
			  (object_key, backend_name, size_bytes, encrypted, encryption_key, key_id, plaintext_size, content_hash, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			key, toBackend, sizeBytes, encrypted, encryptionKey, keyID, plaintextSize, contentHash, now); err != nil {
			return 0, fmt.Errorf("failed to insert destination location: %w", err)
		}

		// --- Decrement source quota ---
		if _, err := tx.ExecContext(ctx, `
			UPDATE backend_quotas
			SET bytes_used = MAX(0, bytes_used - ?), updated_at = ?
			WHERE backend_name = ?`, sizeBytes, now, fromBackend); err != nil {
			return 0, fmt.Errorf("failed to decrement source quota: %w", err)
		}

		// --- Increment destination quota ---
		res, err := tx.ExecContext(ctx, `
			UPDATE backend_quotas
			SET bytes_used = bytes_used + ?, updated_at = ?
			WHERE backend_name = ?
			  AND (bytes_limit = 0 OR bytes_used + orphan_bytes + ? <= bytes_limit)`,
			sizeBytes, now, toBackend, sizeBytes)
		if err != nil {
			return 0, fmt.Errorf("failed to increment destination quota: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("failed to check quota update: %w", err)
		}
		if n == 0 {
			return 0, store.ErrNoSpaceAvailable
		}

		return sizeBytes, nil
	})
}

// DeleteObjectLocation removes a single object_locations row for the given key
// and backend. Used by drain to remove source copies when a replica exists.
func (s *Store) DeleteObjectLocation(ctx context.Context, key, backendName string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM object_locations
		WHERE object_key = ? AND backend_name = ?`, key, backendName)
	return err
}

// ImportObject records a pre-existing object in the database without overwriting.
// Returns true if the object was imported, false if it already existed for this
// backend. Used by the sync subcommand to bring existing bucket objects under
// proxy management.
func (s *Store) ImportObject(ctx context.Context, key, backend string, size int64) (bool, error) {
	return withTxVal(s, ctx, func(tx *sql.Tx) (bool, error) {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		res, err := tx.ExecContext(ctx, `
			INSERT INTO object_locations
			  (object_key, backend_name, size_bytes, encrypted, encryption_key, key_id, plaintext_size, content_hash, created_at)
			VALUES (?, ?, ?, FALSE, NULL, NULL, NULL, NULL, ?)
			ON CONFLICT (object_key, backend_name) DO NOTHING`, key, backend, size, now)
		if err != nil {
			return false, fmt.Errorf("failed to import object %s: %w", key, err)
		}

		n, err := res.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("failed to check import result: %w", err)
		}
		if n == 0 {
			return false, nil
		}

		// --- Increment quota for the backend ---
		qRes, err := tx.ExecContext(ctx, `
			UPDATE backend_quotas
			SET bytes_used = bytes_used + ?, updated_at = ?
			WHERE backend_name = ?
			  AND (bytes_limit = 0 OR bytes_used + orphan_bytes + ? <= bytes_limit)`,
			size, now, backend, size)
		if err != nil {
			return false, fmt.Errorf("failed to increment quota for %s: %w", backend, err)
		}
		qn, err := qRes.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("failed to check quota update: %w", err)
		}
		if qn == 0 {
			return false, store.ErrNoSpaceAvailable
		}

		return true, nil
	})
}

// BackendObjectStats returns the object count and total bytes stored on a backend.
func (s *Store) BackendObjectStats(ctx context.Context, backendName string) (int64, int64, error) {
	var count, totalBytes int64
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(size_bytes), 0)
		FROM object_locations
		WHERE backend_name = ?`, backendName).Scan(&count, &totalBytes)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get backend object stats: %w", err)
	}
	return count, totalBytes, nil
}

// DeleteBackendData removes all database records for a backend in FK-safe order.
// Runs in a single transaction.
func (s *Store) DeleteBackendData(ctx context.Context, backendName string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		stmts := []string{
			`DELETE FROM cleanup_queue WHERE backend_name = ?`,
			`DELETE FROM multipart_parts WHERE upload_id IN (SELECT upload_id FROM multipart_uploads WHERE backend_name = ?)`,
			`DELETE FROM multipart_uploads WHERE backend_name = ?`,
			`DELETE FROM object_locations WHERE backend_name = ?`,
			`DELETE FROM backend_usage WHERE backend_name = ?`,
			`DELETE FROM backend_quotas WHERE backend_name = ?`,
		}
		for _, stmt := range stmts {
			if _, err := tx.ExecContext(ctx, stmt, backendName); err != nil {
				return fmt.Errorf("failed to execute %q: %w", stmt, err)
			}
		}
		return nil
	})
}

// GetRandomHashedObjects returns random object locations that have a stored
// content hash. Used by the scrubber to verify data integrity. Uses
// ORDER BY RANDOM() LIMIT instead of PostgreSQL TABLESAMPLE BERNOULLI.
func (s *Store) GetRandomHashedObjects(ctx context.Context, limit int) ([]store.ObjectLocation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT object_key, backend_name, size_bytes, encrypted, encryption_key,
		       key_id, plaintext_size, content_hash, created_at
		FROM object_locations
		WHERE content_hash IS NOT NULL
		ORDER BY RANDOM()
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get random hashed objects: %w", err)
	}
	defer rows.Close()

	var locs []store.ObjectLocation
	for rows.Next() {
		loc, err := scanObjectLocation(rows)
		if err != nil {
			return nil, err
		}
		locs = append(locs, loc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate random hashed objects: %w", err)
	}
	return locs, nil
}

// GetObjectsWithoutHash returns object locations that have no stored content
// hash, ordered by creation time. Used by the backfill command.
func (s *Store) GetObjectsWithoutHash(ctx context.Context, limit, offset int) ([]store.ObjectLocation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT object_key, backend_name, size_bytes, encrypted, encryption_key,
		       key_id, plaintext_size, content_hash, created_at
		FROM object_locations
		WHERE content_hash IS NULL
		ORDER BY created_at ASC
		LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to get objects without hash: %w", err)
	}
	defer rows.Close()

	var locs []store.ObjectLocation
	for rows.Next() {
		loc, err := scanObjectLocation(rows)
		if err != nil {
			return nil, err
		}
		locs = append(locs, loc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate objects without hash: %w", err)
	}
	return locs, nil
}

// UpdateContentHash sets the content hash for an object location.
func (s *Store) UpdateContentHash(ctx context.Context, key, backendName, hash string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE object_locations
		SET content_hash = ?
		WHERE object_key = ? AND backend_name = ?`, hash, key, backendName)
	return err
}

// scanner is satisfied by both *sql.Rows and *sql.Row.
type scanner interface {
	Scan(dest ...any) error
}

// scanObjectLocation scans a full object location row including all encryption
// and integrity columns.
func scanObjectLocation(rows *sql.Rows) (store.ObjectLocation, error) {
	var (
		loc           store.ObjectLocation
		createdAt     string
		keyID         *string
		plaintextSize *int64
		contentHash   *string
	)
	if err := rows.Scan(
		&loc.ObjectKey, &loc.BackendName, &loc.SizeBytes,
		&loc.Encrypted, &loc.EncryptionKey,
		&keyID, &plaintextSize, &contentHash,
		&createdAt,
	); err != nil {
		return store.ObjectLocation{}, fmt.Errorf("failed to scan object location: %w", err)
	}
	loc.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	if keyID != nil {
		loc.KeyID = *keyID
	}
	if plaintextSize != nil {
		loc.PlaintextSize = *plaintextSize
	}
	if contentHash != nil {
		loc.ContentHash = *contentHash
	}
	return loc, nil
}
