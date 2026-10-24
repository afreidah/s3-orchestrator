// -------------------------------------------------------------------------------
// Integrity Verification Operations
//
// Author: Alex Freidah
//
// Implements the Postgres engine bindings for the integrity scrubber's queue,
// and the copy listing every pass over the ledger selects by stored form:
// hash backfill, the unreadable purge, and the encryption and compression
// rewrites.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"
	"fmt"
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
	rows, err := s.queries.GetLeastRecentlyScrubbedObjects(ctx, db.GetLeastRecentlyScrubbedObjectsParams{
		BackendNames:   backends,
		ScrubbedBefore: pgtype.Timestamptz{Time: scrubbedBefore, Valid: true},
		RowLimit:       rowLimit(max(1, limit)),
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

// -------------------------------------------------------------------------
// COPIES BY STORED FORM
// -------------------------------------------------------------------------

// ListCopiesByForm returns up to limit copies after the cursor that the filter
// selects, in key order. Candidates applies the compression thresholds; with
// none, the size floor is zero and no probe is judged.
func (s *Store) ListCopiesByForm(ctx context.Context, f *core.FormFilter, limit int, after core.Cursor) ([]core.ObjectLocation, error) {
	p := db.ListCopiesByFormParams{
		WithFlags:     int64(f.With),
		WithoutFlags:  int64(f.Without),
		ManagedOnly:   f.ManagedOnly,
		BackendFilter: f.Backend,
		KeyID:         f.KeyID,
		ExceptKeyID:   f.ExceptKeyID,
		AfterKey:      after.ObjectKey,
		AfterBackend:  after.BackendName,
		RowLimit:      rowLimit(limit),
	}
	if t := f.Candidates; t != nil {
		p.MinSize, p.ProbeLevel, p.MinRatio = t.MinSize, t.Level, t.MinRatio
	}
	rows, err := s.queries.ListCopiesByForm(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("list copies by form: %w", err)
	}
	return toWholeObjectLocations(rows), nil
}

// CountCopiesByForm reports how many copies carry every flag of with and none
// of without.
func (s *Store) CountCopiesByForm(ctx context.Context, with, without core.StoredFlags) (int64, error) {
	n, err := s.queries.CountCopiesByForm(ctx, db.CountCopiesByFormParams{
		WithFlags:    int64(with),
		WithoutFlags: int64(without),
	})
	if err != nil {
		return 0, fmt.Errorf("count copies by form: %w", err)
	}
	return n, nil
}
