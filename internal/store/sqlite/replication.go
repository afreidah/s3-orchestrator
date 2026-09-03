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

// -------------------------------------------------------------------------
// UNDER-REPLICATION
// -------------------------------------------------------------------------

// GetUnderReplicatedObjects finds objects with fewer copies than the target
// replication factor. Returns all rows for those objects so callers know which
// backends already have copies.
func (s *Store) GetUnderReplicatedObjects(ctx context.Context, factor, limit int) ([]core.ObjectLocation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ol.object_key, ol.backend_name, ol.size_bytes, ol.encrypted,
		        ol.encryption_key, ol.key_id, ol.plaintext_size, ol.content_hash,
		        ol.compression_algorithm, ol.compression_level, ol.compression_format_version,
		        ol.logical_size, ol.created_at
		 FROM object_locations ol
		 JOIN (
		     SELECT object_key
		     FROM object_locations
		     WHERE managed
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
	// json_each so the query body stays a fixed literal  -  no Go-side
	// string concatenation building the IN clause.
	excludedJSON, err := json.Marshal(excludedBackends)
	if err != nil {
		return nil, fmt.Errorf("encode excluded backend list: %w", err)
	}

	const query = `
		SELECT ol.object_key, ol.backend_name, ol.size_bytes, ol.encrypted,
		       ol.encryption_key, ol.key_id, ol.plaintext_size, ol.content_hash,
		       ol.compression_algorithm, ol.compression_level, ol.compression_format_version,
		       ol.logical_size, ol.created_at
		FROM object_locations ol
		JOIN (
		    SELECT object_key
		    FROM object_locations
		    WHERE backend_name NOT IN (SELECT value FROM json_each(?)) AND managed
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

// -------------------------------------------------------------------------
// OVER-REPLICATION
// -------------------------------------------------------------------------

// GetOverReplicatedObjects finds objects with more copies than the target
// replication factor. Returns all rows for those objects so callers can
// score each copy and decide which to remove.
func (s *Store) GetOverReplicatedObjects(ctx context.Context, factor, limit int) ([]core.ObjectLocation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ol.object_key, ol.backend_name, ol.size_bytes, ol.encrypted,
		        ol.encryption_key, ol.key_id, ol.plaintext_size, ol.content_hash,
		        ol.compression_algorithm, ol.compression_level, ol.compression_format_version,
		        ol.logical_size, ol.created_at
		 FROM object_locations ol
		 JOIN (
		     SELECT object_key
		     FROM object_locations
		     WHERE managed
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
	return s.countRows(ctx, "over-replicated objects",
		`SELECT COUNT(*) FROM (
		     SELECT object_key
		     FROM object_locations
		     WHERE managed
		     GROUP BY object_key
		     HAVING COUNT(*) > ?
		 )`,
		factor)
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// scanObjectLocations converts sql.Rows into a slice of ObjectLocation.
func scanObjectLocations(rows *sql.Rows) ([]core.ObjectLocation, error) {
	return collectRows(rows, rowsObjectLocations, func(rows *sql.Rows) (core.ObjectLocation, error) {
		var (
			loc           core.ObjectLocation
			keyID         sql.NullString
			ptSize        sql.NullInt64
			contentHash   sql.NullString
			compAlgorithm sql.NullString
			compLevel     sql.NullString
			compVersion   sql.NullInt64
			logicalSize   sql.NullInt64
			createdAt     string
		)
		if err := rows.Scan(
			&loc.ObjectKey, &loc.BackendName, &loc.SizeBytes, &loc.Encrypted,
			&loc.EncryptionKey, &keyID, &ptSize, &contentHash,
			&compAlgorithm, &compLevel, &compVersion, &logicalSize, &createdAt,
		); err != nil {
			return core.ObjectLocation{}, fmt.Errorf("failed to scan object location: %w", err)
		}
		loc.KeyID = nullStringValue(keyID)
		loc.PlaintextSize = nullInt64Value(ptSize)
		loc.ContentHash = nullStringValue(contentHash)
		loc.CompressionAlgorithm = nullStringValue(compAlgorithm)
		loc.CompressionLevel = nullStringValue(compLevel)
		loc.CompressionFormatVersion = int(nullInt64Value(compVersion))
		loc.LogicalSize = nullInt64Value(logicalSize)
		var parseErr error
		loc.CreatedAt, parseErr = parseTime(createdAt)
		if parseErr != nil {
			return core.ObjectLocation{}, fmt.Errorf("invalid created_at timestamp %q: %w", createdAt, parseErr)
		}
		return loc, nil
	})
}
