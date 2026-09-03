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

// -------------------------------------------------------------------------
// SCRUB QUEUE
// -------------------------------------------------------------------------

// GetLeastRecentlyScrubbedObjects returns the copies most overdue for
// verification, never-checked ones first, restricted to backends. Ordering
// rather than sampling is what bounds how long any one copy can go unverified.
//
// An empty backends slice selects nothing: the caller has established that no
// backend can be read right now, and returning the whole queue would ignore it.
func (s *Store) GetLeastRecentlyScrubbedObjects(ctx context.Context, limit int, backends []string) ([]core.ObjectLocation, error) {
	if len(backends) == 0 {
		return nil, nil
	}
	safeLimit := int32(max(1, min(limit, math.MaxInt32))) //nolint:gosec // clamped above
	rows, err := s.queries.GetLeastRecentlyScrubbedObjects(ctx, db.GetLeastRecentlyScrubbedObjectsParams{
		BackendNames: backends,
		RowLimit:     safeLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get least recently scrubbed objects: %w", err)
	}
	return toVerifiableObjectLocations(rows), nil
}

// CountScrubCandidatesOnBackends reports how many scrubbable copies live on the
// named backends. The scrubber uses it to say how much of the queue a cycle
// declined to read, which the batch it did read cannot show.
func (s *Store) CountScrubCandidatesOnBackends(ctx context.Context, backends []string) (int64, error) {
	if len(backends) == 0 {
		return 0, nil
	}
	n, err := s.queries.CountScrubCandidatesOnBackends(ctx, backends)
	if err != nil {
		return 0, fmt.Errorf("failed to count scrub candidates: %w", err)
	}
	return n, nil
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

// -------------------------------------------------------------------------
// REPORTING
// -------------------------------------------------------------------------

// IntegrityCoverage reports how far behind verification is, split by whether
// the sweep can reach the copy. reachable is the same backend set the scrub
// queue draws from; a copy outside it can never be stamped, so counting it in
// the age would pin that figure to wall clock rather than to the backlog.
// A never-verified copy is measured from when it was written, matching the
// fallback the queue ordering itself uses.
func (s *Store) IntegrityCoverage(ctx context.Context, reachable []string) (core.CoverageStat, error) {
	row, err := s.queries.IntegrityCoverage(ctx, reachable)
	if err != nil {
		return core.CoverageStat{}, fmt.Errorf("failed to read integrity coverage: %w", err)
	}
	return core.CoverageStat{
		OldestUnverifiedAge: time.Duration(row.AgeSeconds) * time.Second,
		NeverVerified:       row.NeverVerified,
		Deferred:            row.Deferred,
	}, nil
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

// UpdateContentHash records the hash the backfill pass computed and stamps the
// copy as verified in the same statement, because the pass read the whole body
// to produce the digest.
func (s *Store) UpdateContentHash(ctx context.Context, key, backendName, hash string) error {
	return s.queries.UpdateContentHash(ctx, db.UpdateContentHashParams{
		ObjectKey:   key,
		BackendName: backendName,
		ContentHash: &hash,
	})
}
