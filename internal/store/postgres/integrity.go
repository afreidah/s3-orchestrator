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

	"github.com/jackc/pgx/v5/pgtype"

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
func (s *Store) GetLeastRecentlyScrubbedObjects(ctx context.Context, limit int, backends []string, scrubbedBefore time.Time) ([]core.ObjectLocation, error) {
	if len(backends) == 0 {
		return nil, nil
	}
	safeLimit := int32(max(1, min(limit, math.MaxInt32))) //nolint:gosec // clamped above
	rows, err := s.queries.GetLeastRecentlyScrubbedObjects(ctx, db.GetLeastRecentlyScrubbedObjectsParams{
		BackendNames:   backends,
		ScrubbedBefore: pgtype.Timestamptz{Time: scrubbedBefore, Valid: true},
		RowLimit:       safeLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get least recently scrubbed objects: %w", err)
	}
	return toVerifiableObjectLocations(rows), nil
}

// CountScrubCandidatesOnBackends reports how many scrubbable copies live on the
// named backends. The scrubber uses it to say how much of the queue a cycle
// declined to read, which the batch it did read cannot show.
func (s *Store) CountScrubCandidatesOnBackends(ctx context.Context, backends []string, scrubbedBefore time.Time) (int64, error) {
	if len(backends) == 0 {
		return 0, nil
	}
	n, err := s.queries.CountScrubCandidatesOnBackends(ctx, db.CountScrubCandidatesOnBackendsParams{
		BackendNames:   backends,
		ScrubbedBefore: pgtype.Timestamptz{Time: scrubbedBefore, Valid: true},
	})
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

// GetObjectsWithoutHash returns object locations that have no stored content
// hash, in key order after the cursor. Used by the backfill command.
func (s *Store) GetObjectsWithoutHash(ctx context.Context, limit int, after core.Cursor, backend string) ([]core.ObjectLocation, error) {
	rows, err := s.queries.GetObjectsWithoutHash(ctx, db.GetObjectsWithoutHashParams{
		BackendFilter: backend,
		AfterKey:      after.ObjectKey,
		AfterBackend:  after.BackendName,
		RowLimit:      int32(max(0, min(limit, math.MaxInt32))), //nolint:gosec // clamped
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get objects without hash: %w", err)
	}
	return toFatObjectLocations(rows), nil
}

// ListUnreadableLocations returns up to limit copies after the cursor that are
// encrypted with no key, which nothing can decrypt.
func (s *Store) ListUnreadableLocations(ctx context.Context, limit int, after core.Cursor) ([]core.ObjectLocation, error) {
	rows, err := s.queries.ListUnreadableLocations(ctx, db.ListUnreadableLocationsParams{
		AfterKey:     after.ObjectKey,
		AfterBackend: after.BackendName,
		RowLimit:     int32(max(0, min(limit, math.MaxInt32))), //nolint:gosec // clamped
	})
	if err != nil {
		return nil, fmt.Errorf("list unreadable locations: %w", err)
	}
	return toSlimObjectLocations(rows), nil
}

// CountUnreadableLocations reports how many copies are encrypted with no key.
func (s *Store) CountUnreadableLocations(ctx context.Context) (int64, error) {
	n, err := s.queries.CountUnreadableLocations(ctx)
	if err != nil {
		return 0, fmt.Errorf("count unreadable locations: %w", err)
	}
	return n, nil
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
