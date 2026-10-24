// -------------------------------------------------------------------------------
// Replication Operations
//
// Author: Alex Freidah
//
// Implements the Postgres engine bindings for the replication-related
// queries: the copy lookups, the under- and over-replicated scans, and the
// backlog count. The under-replicated scan takes the backends the worker is
// skipping, such as draining or circuit-broken ones.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// -------------------------------------------------------------------------
// COPY LOOKUP
// -------------------------------------------------------------------------

// GetAllObjectLocations returns all copies of an object, ordered by created_at
// ascending (oldest/primary first), each carrying the key's tag count. Used
// for read failover.
func (s *Store) GetAllObjectLocations(ctx context.Context, key string) ([]core.ObjectLocation, error) {
	rows, err := s.queries.GetAllObjectLocations(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("failed to get object locations: %w", err)
	}

	if len(rows) == 0 {
		return nil, core.ErrObjectNotFound
	}

	locs := toIdentifiedObjectLocations(rows)
	for i := range locs {
		locs[i].TagCount = int(rows[i].TagCount)
	}
	return locs, nil
}

// GetObjectBackendsForKeys returns a map from each supplied object_key to
// the set of backend names that hold a copy. Empty input yields an empty
// map; keys with no copies are absent from the result. Used by the
// rebalancer planner to fold the per-key existence check into a single
// query per batch instead of N+1.
func (s *Store) GetObjectBackendsForKeys(ctx context.Context, keys []string) (map[string][]string, error) {
	if len(keys) == 0 {
		return map[string][]string{}, nil
	}
	rows, err := s.queries.GetObjectBackendsForKeys(ctx, keys)
	if err != nil {
		return nil, fmt.Errorf("failed to get object backends for keys: %w", err)
	}
	out := make(map[string][]string, len(keys))
	for _, r := range rows {
		out[r.ObjectKey] = append(out[r.ObjectKey], r.BackendName)
	}
	return out, nil
}

// -------------------------------------------------------------------------
// UNDER-REPLICATION
// -------------------------------------------------------------------------

// GetUnderReplicatedObjects finds objects with fewer copies than the target
// factor, not counting copies on the excluded backends. Returns all rows for
// those objects so callers know which backends already have copies.
func (s *Store) GetUnderReplicatedObjects(ctx context.Context, factor, limit int, excluded []string) ([]core.ObjectLocation, error) {
	if excluded == nil {
		// A nil slice binds as NULL, and != ALL(NULL) would match no row.
		excluded = []string{}
	}
	rows, err := s.queries.GetUnderReplicatedObjects(ctx, db.GetUnderReplicatedObjectsParams{
		Excluded: excluded,
		Factor:   int64(factor),
		MaxKeys:  rowLimit(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query under-replicated objects: %w", err)
	}

	return toFatObjectLocations(rows), nil
}

// -------------------------------------------------------------------------
// OVER-REPLICATION
// -------------------------------------------------------------------------

// GetOverReplicatedObjects finds objects with more copies than the target
// replication factor. Returns all rows for those objects so callers can
// score each copy and decide which to remove.
func (s *Store) GetOverReplicatedObjects(ctx context.Context, factor, limit int) ([]core.ObjectLocation, error) {
	rows, err := s.queries.GetOverReplicatedObjects(ctx, db.GetOverReplicatedObjectsParams{
		Factor:  int64(factor),
		MaxKeys: rowLimit(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query over-replicated objects: %w", err)
	}

	return toFatObjectLocations(rows), nil
}

// CountReplicationBacklog counts every key under and over the target factor,
// uncapped, in one pass.
func (s *Store) CountReplicationBacklog(ctx context.Context, factor int) (core.ReplicationBacklog, error) {
	row, err := s.queries.CountReplicationBacklog(ctx, int64(factor))
	if err != nil {
		return core.ReplicationBacklog{}, fmt.Errorf("failed to count replication backlog: %w", err)
	}
	return core.ReplicationBacklog{Under: row.UnderReplicated, Over: row.OverReplicated}, nil
}
