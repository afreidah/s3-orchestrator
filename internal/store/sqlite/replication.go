// -------------------------------------------------------------------------------
// SQLite Replication - Under/Over-Replication Queries and Replica Management
//
// Author: Alex Freidah
//
// Implements replication queries for the SQLite backend: finding under- and
// over-replicated objects via HAVING COUNT, recording new replicas with
// conflict detection, and removing excess copies with quota adjustment.
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

// GetUnderReplicatedObjects finds objects with fewer copies than the target
// replication factor. Returns all rows for those objects so callers know which
// backends already have copies.
func (s *Store) GetUnderReplicatedObjects(ctx context.Context, factor, limit int) ([]store.ObjectLocation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ol.object_key, ol.backend_name, ol.size_bytes, ol.encrypted,
		        ol.encryption_key, ol.key_id, ol.plaintext_size, ol.content_hash, ol.created_at
		 FROM object_locations ol
		 JOIN (
		     SELECT object_key
		     FROM object_locations
		     GROUP BY object_key
		     HAVING COUNT(*) < ?
		     LIMIT ?
		 ) ur ON ol.object_key = ur.object_key
		 ORDER BY ol.object_key, ol.created_at ASC`,
		factor, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query under-replicated objects: %w", err)
	}
	defer rows.Close()

	return scanObjectLocations(rows)
}

// GetUnderReplicatedObjectsExcluding finds objects with fewer copies than the
// target factor, ignoring copies on the excluded backends. Returns all rows
// for those objects so callers know the full picture.
func (s *Store) GetUnderReplicatedObjectsExcluding(ctx context.Context, factor, limit int, excludedBackends []string) ([]store.ObjectLocation, error) {
	if len(excludedBackends) == 0 {
		return s.GetUnderReplicatedObjects(ctx, factor, limit)
	}

	// Build dynamic NOT IN (?,?,...) placeholder list.
	placeholders := make([]string, len(excludedBackends))
	args := make([]any, 0, len(excludedBackends)+2)
	for i, b := range excludedBackends {
		placeholders[i] = "?"
		args = append(args, b)
	}
	args = append(args, factor, limit)

	query := fmt.Sprintf(
		`SELECT ol.object_key, ol.backend_name, ol.size_bytes, ol.encrypted,
		        ol.encryption_key, ol.key_id, ol.plaintext_size, ol.content_hash, ol.created_at
		 FROM object_locations ol
		 JOIN (
		     SELECT object_key
		     FROM object_locations
		     WHERE backend_name NOT IN (%s)
		     GROUP BY object_key
		     HAVING COUNT(*) < ?
		     LIMIT ?
		 ) ur ON ol.object_key = ur.object_key
		 ORDER BY ol.object_key, ol.created_at ASC`,
		strings.Join(placeholders, ","),
	)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query under-replicated objects (excluding): %w", err)
	}
	defer rows.Close()

	return scanObjectLocations(rows)
}

// RecordReplica inserts a replica copy of an object, but only if the source
// copy still exists and the target does not already have a copy. Returns true
// if the replica was inserted, false if skipped.
func (s *Store) RecordReplica(ctx context.Context, key, targetBackend, sourceBackend string, size int64) (bool, error) {
	return withTxVal(s, ctx, func(tx *sql.Tx) (bool, error) {
		now := time.Now().UTC().Format(time.RFC3339Nano)

		// Conditional insert: copy metadata from source, skip if target already has a copy.
		res, err := tx.ExecContext(ctx,
			`INSERT INTO object_locations (object_key, backend_name, size_bytes, encrypted, encryption_key, key_id, plaintext_size, content_hash, created_at)
			 SELECT ?, ?, ol.size_bytes, ol.encrypted, ol.encryption_key, ol.key_id, ol.plaintext_size, ol.content_hash, ?
			 FROM object_locations ol
			 WHERE ol.object_key = ? AND ol.backend_name = ?
			 ON CONFLICT (object_key, backend_name) DO NOTHING`,
			key, targetBackend, now, key, sourceBackend,
		)
		if err != nil {
			return false, fmt.Errorf("failed to insert replica: %w", err)
		}

		affected, err := res.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("failed to get rows affected: %w", err)
		}
		if affected == 0 {
			return false, nil
		}

		// Increment quota for target backend.
		updNow := time.Now().UTC().Format(time.RFC3339Nano)
		res, err = tx.ExecContext(ctx,
			`UPDATE backend_quotas
			 SET bytes_used = bytes_used + ?, updated_at = ?
			 WHERE backend_name = ?
			   AND (bytes_limit = 0 OR bytes_used + orphan_bytes + ? <= bytes_limit)`,
			size, updNow, targetBackend, size,
		)
		if err != nil {
			return false, fmt.Errorf("failed to update quota: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("failed to get rows affected: %w", err)
		}
		if n == 0 {
			return false, store.ErrNoSpaceAvailable
		}

		return true, nil
	})
}

// GetOverReplicatedObjects finds objects with more copies than the target
// replication factor. Returns all rows for those objects so callers can
// score each copy and decide which to remove.
func (s *Store) GetOverReplicatedObjects(ctx context.Context, factor, limit int) ([]store.ObjectLocation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ol.object_key, ol.backend_name, ol.size_bytes, ol.encrypted,
		        ol.encryption_key, ol.key_id, ol.plaintext_size, ol.content_hash, ol.created_at
		 FROM object_locations ol
		 JOIN (
		     SELECT object_key
		     FROM object_locations
		     GROUP BY object_key
		     HAVING COUNT(*) > ?
		     LIMIT ?
		 ) orep ON ol.object_key = orep.object_key
		 ORDER BY ol.object_key, ol.created_at ASC`,
		factor, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query over-replicated objects: %w", err)
	}
	defer rows.Close()

	return scanObjectLocations(rows)
}

// CountOverReplicatedObjects returns the total number of objects with more
// copies than the target replication factor.
func (s *Store) CountOverReplicatedObjects(ctx context.Context, factor int) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM (
		     SELECT object_key
		     FROM object_locations
		     GROUP BY object_key
		     HAVING COUNT(*) > ?
		 )`,
		factor,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count over-replicated objects: %w", err)
	}
	return count, nil
}

// RemoveExcessCopy deletes one copy of an object from the given backend inside
// a transaction, decrementing the backend quota atomically.
func (s *Store) RemoveExcessCopy(ctx context.Context, key, backendName string, size int64) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM object_locations WHERE object_key = ? AND backend_name = ?`,
			key, backendName,
		); err != nil {
			return fmt.Errorf("failed to delete excess copy: %w", err)
		}

		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx,
			`UPDATE backend_quotas
			 SET bytes_used = MAX(0, bytes_used - ?), updated_at = ?
			 WHERE backend_name = ?`,
			size, now, backendName,
		); err != nil {
			return fmt.Errorf("failed to decrement quota: %w", err)
		}
		return nil
	})
}

// scanObjectLocations converts sql.Rows into a slice of ObjectLocation.
func scanObjectLocations(rows *sql.Rows) ([]store.ObjectLocation, error) {
	var locs []store.ObjectLocation
	for rows.Next() {
		var (
			loc         store.ObjectLocation
			keyID       sql.NullString
			ptSize      sql.NullInt64
			contentHash sql.NullString
			createdAt   string
		)
		if err := rows.Scan(
			&loc.ObjectKey, &loc.BackendName, &loc.SizeBytes, &loc.Encrypted,
			&loc.EncryptionKey, &keyID, &ptSize, &contentHash, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan object location: %w", err)
		}
		if keyID.Valid {
			loc.KeyID = keyID.String
		}
		if ptSize.Valid {
			loc.PlaintextSize = ptSize.Int64
		}
		if contentHash.Valid {
			loc.ContentHash = contentHash.String
		}
		loc.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		locs = append(locs, loc)
	}
	return locs, rows.Err()
}
