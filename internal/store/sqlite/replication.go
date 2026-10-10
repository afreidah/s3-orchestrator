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
//
// A key's copies are the rows it holds plus the intents still uploading one. A
// write that places several copies commits them a moment apart, and counting
// rows alone would have the worker copy the object to a backend the write is
// already landing on.
//
// Every live intent counts, whatever its role: the first copy to land may be a
// companion while the primary is still in flight. An intent left by a write
// that never finished holds replication off only until the reaper clears it.
func (s *Store) GetUnderReplicatedObjects(ctx context.Context, factor, limit int) ([]core.ObjectLocation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ol.object_key, ol.backend_name, ol.storage_key, ol.size_bytes, ol.encrypted,
		        ol.encryption_key, ol.key_id, ol.plaintext_size, ol.content_hash,
		        ol.compression_algorithm, ol.compression_level, ol.compression_format_version,
		        ol.logical_size, ol.created_at
		 FROM object_locations ol
		 JOIN (
		     SELECT ol2.object_key
		     FROM object_locations ol2
		     LEFT JOIN (
		         SELECT object_key, COUNT(*) AS copies
		         FROM pending_objects
		         GROUP BY object_key
		     ) i ON i.object_key = ol2.object_key
		     WHERE ol2.managed
		     GROUP BY ol2.object_key, i.copies
		     HAVING COUNT(*) + COALESCE(i.copies, 0) < ?
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
// target factor, ignoring copies on the excluded backends, and returns all
// rows for those objects. In-flight intents count as in
// GetUnderReplicatedObjects, including those on an excluded backend: excluding
// a backend stops new copies going there, not one already on its way.
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
		SELECT ol.object_key, ol.backend_name, ol.storage_key, ol.size_bytes, ol.encrypted,
		       ol.encryption_key, ol.key_id, ol.plaintext_size, ol.content_hash,
		       ol.compression_algorithm, ol.compression_level, ol.compression_format_version,
		       ol.logical_size, ol.created_at
		FROM object_locations ol
		JOIN (
		    SELECT ol2.object_key
		    FROM object_locations ol2
		    LEFT JOIN (
		        SELECT object_key, COUNT(*) AS copies
		        FROM pending_objects
		        GROUP BY object_key
		    ) i ON i.object_key = ol2.object_key
		    WHERE ol2.backend_name NOT IN (SELECT value FROM json_each(?)) AND ol2.managed
		    GROUP BY ol2.object_key, i.copies
		    HAVING COUNT(*) + COALESCE(i.copies, 0) < ?
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
		`SELECT ol.object_key, ol.backend_name, ol.storage_key, ol.size_bytes, ol.encrypted,
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

// CountReplicationBacklog counts every key under and over the target factor,
// uncapped, in one pass. Under applies GetUnderReplicatedObjects' in-flight
// rule; over counts recorded copies only, as GetOverReplicatedObjects does.
func (s *Store) CountReplicationBacklog(ctx context.Context, factor int) (core.ReplicationBacklog, error) {
	var b core.ReplicationBacklog
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FILTER (WHERE held + inflight < ?1),
		        COUNT(*) FILTER (WHERE held > ?1)
		 FROM (
		     SELECT COUNT(*) AS held, COALESCE(i.copies, 0) AS inflight
		     FROM object_locations ol
		     LEFT JOIN (
		         SELECT object_key, COUNT(*) AS copies
		         FROM pending_objects
		         GROUP BY object_key
		     ) i ON i.object_key = ol.object_key
		     WHERE ol.managed
		     GROUP BY ol.object_key, i.copies
		 )`,
		factor,
	).Scan(&b.Under, &b.Over)
	if err != nil {
		return core.ReplicationBacklog{}, fmt.Errorf("failed to count replication backlog: %w", err)
	}
	return b, nil
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
			&loc.ObjectKey, &loc.BackendName, &loc.StorageKey, &loc.SizeBytes, &loc.Encrypted,
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
