// -------------------------------------------------------------------------------
// Integrity Verification Operations
//
// Author: Alex Freidah
//
// Implements the Postgres engine bindings for the integrity scrubber:
// random-sample selection of objects whose content_hash is set,
// listing of objects whose hash is null (so the backfill path can
// compute one), and the per-object UpdateContentHash. Uses TABLESAMPLE
// SYSTEM for cheap random sampling instead of ORDER BY random() so the
// scrubber stays linear-time as the table grows.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// GetLeastRecentlyScrubbedObjects returns the copies most overdue for
// verification, never-checked ones first. Ordering rather than sampling is
// what bounds how long any one copy can go unverified.
func (s *Store) GetLeastRecentlyScrubbedObjects(ctx context.Context, limit int) ([]core.ObjectLocation, error) {
	safeLimit := int32(max(1, min(limit, math.MaxInt32))) //nolint:gosec // clamped above
	rows, err := s.queries.GetLeastRecentlyScrubbedObjects(ctx, safeLimit)
	if err != nil {
		return nil, fmt.Errorf("failed to get least recently scrubbed objects: %w", err)
	}
	return toFatObjectLocations(rows), nil
}

// MarkObjectScrubbed records that a copy was examined, which is what advances
// the sweep past it.
func (s *Store) MarkObjectScrubbed(ctx context.Context, key, backendName string) error {
	if err := s.queries.MarkObjectScrubbed(ctx, db.MarkObjectScrubbedParams{
		ObjectKey:   key,
		BackendName: backendName,
	}); err != nil {
		return fmt.Errorf("failed to mark object scrubbed: %w", err)
	}
	return nil
}

// OldestUnverifiedAge reports how stale the least recently verified copy is,
// and how many copies have never been verified at all.
func (s *Store) OldestUnverifiedAge(ctx context.Context) (time.Duration, int64, error) {
	row, err := s.queries.OldestUnverifiedAge(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to read oldest unverified age: %w", err)
	}
	return time.Duration(row.AgeSeconds) * time.Second, row.NeverVerified, nil
}

// GetObjectsWithoutHash returns object locations that have no stored content
// hash, ordered by creation time. Used by the backfill command.
func (s *Store) GetObjectsWithoutHash(ctx context.Context, limit, offset int) ([]core.ObjectLocation, error) {
	safeLimit := int32(max(0, min(limit, math.MaxInt32)))   //nolint:gosec // clamped
	safeOffset := int32(max(0, min(offset, math.MaxInt32))) //nolint:gosec // clamped
	rows, err := s.queries.GetObjectsWithoutHash(ctx, db.GetObjectsWithoutHashParams{
		Limit:  safeLimit,
		Offset: safeOffset,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get objects without hash: %w", err)
	}
	return toFatObjectLocations(rows), nil
}

// UpdateContentHash sets the content hash for an object location.
func (s *Store) UpdateContentHash(ctx context.Context, key, backendName, hash string) error {
	return s.queries.UpdateContentHash(ctx, db.UpdateContentHashParams{
		ObjectKey:   key,
		BackendName: backendName,
		ContentHash: &hash,
	})
}
