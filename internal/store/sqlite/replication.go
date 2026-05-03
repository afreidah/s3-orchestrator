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
	"encoding/json"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// GetUnderReplicatedObjects finds objects with fewer copies than the target
// replication factor. Returns all rows for those objects so callers know which
// backends already have copies.
func (s *Store) GetUnderReplicatedObjects(ctx context.Context, factor, limit int) ([]core.ObjectLocation, error) {
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
func (s *Store) GetUnderReplicatedObjectsExcluding(ctx context.Context, factor, limit int, excludedBackends []string) ([]core.ObjectLocation, error) {
	if len(excludedBackends) == 0 {
		return s.GetUnderReplicatedObjects(ctx, factor, limit)
	}

	// Expand the excluded list inside SQLite via the JSON1 extension's
	// json_each so the query body stays a fixed literal — no Go-side
	// string concatenation building the IN clause.
	excludedJSON, err := json.Marshal(excludedBackends)
	if err != nil {
		return nil, fmt.Errorf("encode excluded backend list: %w", err)
	}

	const query = `
		SELECT ol.object_key, ol.backend_name, ol.size_bytes, ol.encrypted,
		       ol.encryption_key, ol.key_id, ol.plaintext_size, ol.content_hash, ol.created_at
		FROM object_locations ol
		JOIN (
		    SELECT object_key
		    FROM object_locations
		    WHERE backend_name NOT IN (SELECT value FROM json_each(?))
		    GROUP BY object_key
		    HAVING COUNT(*) < ?
		    LIMIT ?
		) ur ON ol.object_key = ur.object_key
		ORDER BY ol.object_key, ol.created_at ASC`

	rows, err := s.db.QueryContext(ctx, query, string(excludedJSON), factor, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query under-replicated objects (excluding): %w", err)
	}
	defer rows.Close()

	return scanObjectLocations(rows)
}

// RecordReplica delegates to core.RecordReplica.
func (s *Store) RecordReplica(ctx context.Context, key, targetBackend, sourceBackend string) (int64, bool, error) {
	return core.RecordReplica(ctx, s, key, targetBackend, sourceBackend)
}

// GetOverReplicatedObjects finds objects with more copies than the target
// replication factor. Returns all rows for those objects so callers can
// score each copy and decide which to remove.
func (s *Store) GetOverReplicatedObjects(ctx context.Context, factor, limit int) ([]core.ObjectLocation, error) {
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

// RemoveExcessCopy delegates to core.RemoveExcessCopy.
func (s *Store) RemoveExcessCopy(ctx context.Context, key, backendName string, size int64) error {
	return core.RemoveExcessCopy(ctx, s, key, backendName, size)
}

// scanObjectLocations converts sql.Rows into a slice of ObjectLocation.
func scanObjectLocations(rows *sql.Rows) ([]core.ObjectLocation, error) {
	var locs []core.ObjectLocation
	for rows.Next() {
		var (
			loc         core.ObjectLocation
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
		var parseErr error
		loc.CreatedAt, parseErr = parseTime(createdAt)
		if parseErr != nil {
			return nil, fmt.Errorf("invalid created_at timestamp %q: %w", createdAt, parseErr)
		}
		locs = append(locs, loc)
	}
	return locs, rows.Err()
}
