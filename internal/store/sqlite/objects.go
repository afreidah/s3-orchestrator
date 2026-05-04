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
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// likeEscaper escapes SQL LIKE wildcards in prefix strings.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// errInvalidTimestamp is the wrap format string used everywhere a
// stored RFC 3339 created_at fails time.Parse. Centralised so the
// surfaced error stays consistent across listing helpers.
const errInvalidTimestamp = "invalid created_at timestamp %q: %w"

// -------------------------------------------------------------------------
// READ QUERIES
// -------------------------------------------------------------------------

// GetObjectBackendsForKeys returns a map from each supplied object_key to
// the backends that hold a copy. Empty input yields an empty map; keys
// with no copies are absent from the result. Used by the rebalancer
// planner to fold the per-key existence check into a single query per
// batch instead of N+1.
//
// The query uses SQLite's json_each so the SQL stays static and the
// keys array is passed as a single JSON-encoded parameter rather than
// interpolated into the SQL string.
func (s *Store) GetObjectBackendsForKeys(ctx context.Context, keys []string) (map[string][]string, error) {
	if len(keys) == 0 {
		return map[string][]string{}, nil
	}
	keysJSON, err := json.Marshal(keys)
	if err != nil {
		return nil, fmt.Errorf("marshal keys: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT object_key, backend_name
		FROM object_locations
		WHERE object_key IN (SELECT value FROM json_each(?))`, string(keysJSON))
	if err != nil {
		return nil, fmt.Errorf("failed to get object backends for keys: %w", err)
	}
	defer rows.Close()
	out := make(map[string][]string, len(keys))
	for rows.Next() {
		var key, backend string
		if err := rows.Scan(&key, &backend); err != nil {
			return nil, fmt.Errorf("failed to scan key/backend pair: %w", err)
		}
		out[key] = append(out[key], backend)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate key/backend rows: %w", err)
	}
	return out, nil
}

// GetAllObjectLocations returns all copies of an object, ordered by created_at
// ascending (oldest/primary first). Used for read failover.
func (s *Store) GetAllObjectLocations(ctx context.Context, key string) ([]core.ObjectLocation, error) {
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

	var locs []core.ObjectLocation
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
		return nil, core.ErrObjectNotFound
	}
	return locs, nil
}

// -------------------------------------------------------------------------
// WRITE OPERATIONS
// -------------------------------------------------------------------------

// RecordObject delegates to core.RecordObject which composes the
// engine-agnostic transactional sequence against the SQLite TxAdapter.
func (s *Store) RecordObject(ctx context.Context, key, backend string, size int64, enc *core.EncryptionMeta) ([]core.DeletedCopy, error) {
	return core.RecordObject(ctx, s, key, backend, size, enc)
}

// RecordObjectAndClearPending delegates to core. Inside the same
// transaction the pending row is deleted so the intent never outlives
// a committed location.
func (s *Store) RecordObjectAndClearPending(ctx context.Context, key, backend string, size int64, enc *core.EncryptionMeta, intentID string) ([]core.DeletedCopy, error) {
	return core.RecordObjectAndClearPending(ctx, s, key, backend, size, enc, intentID)
}

// DeleteObject delegates to core.DeleteObject.
func (s *Store) DeleteObject(ctx context.Context, key string) ([]core.DeletedCopy, error) {
	return core.DeleteObject(ctx, s, key)
}

// DeleteObjectsBatch delegates to core.DeleteObjectsBatch which
// removes every supplied key in one transaction.
func (s *Store) DeleteObjectsBatch(ctx context.Context, keys []string) (map[string][]core.DeletedCopy, error) {
	return core.DeleteObjectsBatch(ctx, s, keys)
}

// -------------------------------------------------------------------------
// LISTING
// -------------------------------------------------------------------------

// ListObjects returns objects matching the given prefix, sorted by key.
// Supports pagination via startAfter and maxKeys. Returns one extra row to
// detect truncation. Uses a subquery with GROUP BY to deduplicate replicated
// objects (equivalent to DISTINCT ON in PostgreSQL).
func (s *Store) ListObjects(ctx context.Context, prefix, startAfter string, maxKeys int) (*core.ListObjectsResult, error) {
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

	objects, err := scanSlimObjectLocations(rows)
	if err != nil {
		return nil, err
	}

	result := &core.ListObjectsResult{}
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
func (s *Store) ListExpiredObjects(ctx context.Context, prefix string, cutoff time.Time, limit int) ([]core.ObjectLocation, error) {
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

	return scanSlimObjectLocations(rows)
}

// ListObjectsByBackend returns objects stored on a specific backend, ordered by
// size ascending (smallest first). Used by the rebalancer to find movable objects.
func (s *Store) ListObjectsByBackend(ctx context.Context, backendName string, limit int) ([]core.ObjectLocation, error) {
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

	return scanSlimObjectLocations(rows)
}

// ListObjectsByBackendKeyAsc returns rows for a backend in ascending
// object_key order, starting strictly after afterKey. The empty string
// returns the first page. Used by ReconcileBackend's bounded-memory
// sorted-merge join against an S3 ListObjects walk; both sides are in lex
// order so the merge is O(limit) memory bounded.
func (s *Store) ListObjectsByBackendKeyAsc(ctx context.Context, backendName, afterKey string, limit int) ([]core.ObjectLocation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT object_key, backend_name, size_bytes, created_at
		FROM object_locations
		WHERE backend_name = ? AND object_key > ?
		ORDER BY object_key ASC
		LIMIT ?`, backendName, afterKey, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to page objects by backend: %w", err)
	}
	defer rows.Close()

	return scanSlimObjectLocations(rows)
}

// scanSlimObjectLocations consumes a *sql.Rows holding the slim
// (object_key, backend_name, size_bytes, created_at) projection used by
// ListObjects, ListExpiredObjects, and ListObjectsByBackend. Centralizes
// the per-row scan + RFC3339 timestamp parse so the three list helpers
// don't carry parallel loop bodies.
func scanSlimObjectLocations(rows *sql.Rows) ([]core.ObjectLocation, error) {
	var locs []core.ObjectLocation
	for rows.Next() {
		var loc core.ObjectLocation
		var createdAt string
		if err := rows.Scan(&loc.ObjectKey, &loc.BackendName, &loc.SizeBytes, &createdAt); err != nil {
			return nil, fmt.Errorf("failed to scan object location: %w", err)
		}
		var parseErr error
		loc.CreatedAt, parseErr = parseTime(createdAt)
		if parseErr != nil {
			return nil, fmt.Errorf(errInvalidTimestamp, createdAt, parseErr)
		}
		locs = append(locs, loc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate object locations: %w", err)
	}
	return locs, nil
}

// -------------------------------------------------------------------------
// LOCATION MUTATIONS
// -------------------------------------------------------------------------

// MoveObjectLocation delegates to core.MoveObjectLocation.
func (s *Store) MoveObjectLocation(ctx context.Context, key, fromBackend, toBackend string) (int64, error) {
	return core.MoveObjectLocation(ctx, s, key, fromBackend, toBackend)
}

// DeleteObjectLocation removes a single object_locations row for the given key
// and backend. Used by drain to remove source copies when a replica exists.
func (s *Store) DeleteObjectLocation(ctx context.Context, key, backendName string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM object_locations
		WHERE object_key = ? AND backend_name = ?`, key, backendName)
	return err
}

// ImportObject delegates to core.ImportObject.
func (s *Store) ImportObject(ctx context.Context, key, backend string, size int64) (bool, error) {
	return core.ImportObject(ctx, s, key, backend, size)
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

// -------------------------------------------------------------------------
// INTEGRITY
// -------------------------------------------------------------------------

// GetRandomHashedObjects returns random object locations that have a stored
// content hash. Used by the scrubber to verify data integrity. Uses
// ORDER BY RANDOM() LIMIT instead of PostgreSQL TABLESAMPLE BERNOULLI.
func (s *Store) GetRandomHashedObjects(ctx context.Context, limit int) ([]core.ObjectLocation, error) {
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

	var locs []core.ObjectLocation
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
func (s *Store) GetObjectsWithoutHash(ctx context.Context, limit, offset int) ([]core.ObjectLocation, error) {
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

	var locs []core.ObjectLocation
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

// -------------------------------------------------------------------------
// ROW SCANNERS
// -------------------------------------------------------------------------

// scanObjectLocation scans a full object location row including all encryption
// and integrity columns.
func scanObjectLocation(rows *sql.Rows) (core.ObjectLocation, error) {
	var (
		loc           core.ObjectLocation
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
		return core.ObjectLocation{}, fmt.Errorf("failed to scan object location: %w", err)
	}
	var parseErr error
	loc.CreatedAt, parseErr = parseTime(createdAt)
	if parseErr != nil {
		return core.ObjectLocation{}, fmt.Errorf(errInvalidTimestamp, createdAt, parseErr)
	}
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
