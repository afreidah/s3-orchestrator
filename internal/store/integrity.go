// -------------------------------------------------------------------------------
// Integrity Verification Operations
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package store

import (
	"context"
	"fmt"
	"math"

	db "github.com/afreidah/s3-orchestrator/internal/store/sqlc"
)

// GetRandomHashedObjects returns random object locations that have a stored
// content hash. Used by the scrubber to verify data integrity.
func (s *Store) GetRandomHashedObjects(ctx context.Context, limit int) ([]ObjectLocation, error) {
	safeLimit := int32(max(1, min(limit, math.MaxInt32))) //nolint:gosec // clamped above
	rows, err := s.queries.GetRandomHashedObjects(ctx, safeLimit)
	if err != nil {
		return nil, fmt.Errorf("failed to get random hashed objects: %w", err)
	}
	return toObjectLocations(rows), nil
}

// GetObjectsWithoutHash returns object locations that have no stored content
// hash, ordered by creation time. Used by the backfill command.
func (s *Store) GetObjectsWithoutHash(ctx context.Context, limit, offset int) ([]ObjectLocation, error) {
	safeLimit := int32(max(0, min(limit, math.MaxInt32)))   //nolint:gosec // clamped
	safeOffset := int32(max(0, min(offset, math.MaxInt32))) //nolint:gosec // clamped
	rows, err := s.queries.GetObjectsWithoutHash(ctx, db.GetObjectsWithoutHashParams{
		Limit:  safeLimit,
		Offset: safeOffset,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get objects without hash: %w", err)
	}
	return toObjectLocations(rows), nil
}

// UpdateContentHash sets the content hash for an object location.
func (s *Store) UpdateContentHash(ctx context.Context, key, backendName, hash string) error {
	return s.queries.UpdateContentHash(ctx, db.UpdateContentHashParams{
		ObjectKey:   key,
		BackendName: backendName,
		ContentHash: &hash,
	})
}
