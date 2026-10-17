// -------------------------------------------------------------------------------
// Compression Admin Operations
//
// Author: Alex Freidah
//
// Postgres bindings for the two complementary listings compress-existing and
// decompress-existing walk.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// -------------------------------------------------------------------------
// LISTINGS
// -------------------------------------------------------------------------

// ListUncompressedLocations returns a page of copies whose bytes carry no
// encoding and that the supplied thresholds do not already exclude.
func (s *Store) ListUncompressedLocations(ctx context.Context, limit int, after core.Cursor, t core.CompressionThresholds, backend string) ([]core.ObjectLocation, error) {
	rows, err := s.queries.ListUncompressedLocations(ctx, db.ListUncompressedLocationsParams{
		BackendFilter: backend,
		MinSize:       t.MinSize,
		ProbeLevel:    t.Level,
		MinRatio:      t.MinRatio,
		AfterKey:      after.ObjectKey,
		AfterBackend:  after.BackendName,
		RowLimit:      int32(limit), //nolint:gosec // G115: limit is a small caller-controlled batch size
	})
	if err != nil {
		return nil, fmt.Errorf("list uncompressed locations: %w", err)
	}
	return toWholeObjectLocations(rows), nil
}

// ListCompressedLocations returns a page of copies whose bytes are an encoding.
func (s *Store) ListCompressedLocations(ctx context.Context, limit int, after core.Cursor, backend string) ([]core.ObjectLocation, error) {
	rows, err := s.queries.ListCompressedLocations(ctx, db.ListCompressedLocationsParams{
		BackendFilter: backend,
		AfterKey:      after.ObjectKey,
		AfterBackend:  after.BackendName,
		RowLimit:      int32(limit), //nolint:gosec // G115: limit is a small caller-controlled batch size
	})
	if err != nil {
		return nil, fmt.Errorf("list compressed locations: %w", err)
	}
	return toWholeObjectLocations(rows), nil
}
