// -------------------------------------------------------------------------------
// Replication Operations
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package postgres

import (
	"context"
	"fmt"
	"math"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// GetAllObjectLocations returns all copies of an object, ordered by created_at
// ascending (oldest/primary first). Used for read failover.
func (s *Store) GetAllObjectLocations(ctx context.Context, key string) ([]ObjectLocation, error) {
	rows, err := s.queries.GetAllObjectLocations(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("failed to get object locations: %w", err)
	}

	if len(rows) == 0 {
		return nil, ErrObjectNotFound
	}

	return toFatObjectLocations(rows), nil
}

// GetUnderReplicatedObjects finds objects with fewer copies than the target
// replication factor. Returns all rows for those objects so callers know which
// backends already have copies.
func (s *Store) GetUnderReplicatedObjects(ctx context.Context, factor, limit int) ([]ObjectLocation, error) {
	rows, err := s.queries.GetUnderReplicatedObjects(ctx, db.GetUnderReplicatedObjectsParams{
		Factor:  int64(factor),
		MaxKeys: int32(limit), //nolint:gosec // G115: limit is a small caller-controlled batch size
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query under-replicated objects: %w", err)
	}

	return toFatObjectLocations(rows), nil
}

// GetUnderReplicatedObjectsExcluding finds objects with fewer copies than the
// target factor, ignoring copies on the excluded backends. Returns all rows
// for those objects so callers know the full picture.
func (s *Store) GetUnderReplicatedObjectsExcluding(ctx context.Context, factor, limit int, excludedBackends []string) ([]ObjectLocation, error) {
	rows, err := s.queries.GetUnderReplicatedObjectsExcluding(ctx, db.GetUnderReplicatedObjectsExcludingParams{
		Excluded: excludedBackends,
		Factor:   int64(factor),
		MaxKeys:  int32(limit), //nolint:gosec // G115: limit is a small caller-controlled batch size
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query under-replicated objects (excluding): %w", err)
	}

	return toFatObjectLocations(rows), nil
}

// RecordReplica inserts a replica copy of an object, but only if the
// source copy still exists. Delegates to core.RecordReplica.
func (s *Store) RecordReplica(ctx context.Context, key, targetBackend, sourceBackend string, size int64) (bool, error) {
	return core.RecordReplica(ctx, s, key, targetBackend, sourceBackend, size)
}

// GetOverReplicatedObjects finds objects with more copies than the target
// replication factor. Returns all rows for those objects so callers can
// score each copy and decide which to remove.
func (s *Store) GetOverReplicatedObjects(ctx context.Context, factor, limit int) ([]ObjectLocation, error) {
	var maxKeys int32
	switch {
	case limit <= 0:
		maxKeys = 0
	case limit > math.MaxInt32:
		maxKeys = math.MaxInt32
	default:
		maxKeys = int32(limit)
	}

	rows, err := s.queries.GetOverReplicatedObjects(ctx, db.GetOverReplicatedObjectsParams{
		Factor:  int64(factor),
		MaxKeys: maxKeys,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query over-replicated objects: %w", err)
	}

	return toFatObjectLocations(rows), nil
}

// CountOverReplicatedObjects returns the total number of objects with more
// copies than the target replication factor.
func (s *Store) CountOverReplicatedObjects(ctx context.Context, factor int) (int64, error) {
	count, err := s.queries.CountOverReplicatedObjects(ctx, int64(factor))
	if err != nil {
		return 0, fmt.Errorf("failed to count over-replicated objects: %w", err)
	}
	return count, nil
}

// RemoveExcessCopy deletes one copy of an object from the given
// backend inside a transaction, decrementing the backend quota
// atomically. Delegates to core.RemoveExcessCopy.
func (s *Store) RemoveExcessCopy(ctx context.Context, key, backendName string, size int64) error {
	return core.RemoveExcessCopy(ctx, s, key, backendName, size)
}

// GetObjectCopiesForUpdate retrieves all copies of an object under a FOR
// UPDATE lock, suitable for use inside a transaction to prevent concurrent
// modification during over-replication cleanup.
func (s *Store) GetObjectCopiesForUpdate(ctx context.Context, key string) ([]ObjectLocation, error) {
	rows, err := s.queries.GetObjectCopiesForUpdate(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("failed to get copies for update: %w", err)
	}
	return toFatObjectLocations(rows), nil
}
