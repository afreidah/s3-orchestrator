// -------------------------------------------------------------------------------
// Compression Admin Operations
//
// Author: Alex Freidah
//
// Postgres bindings for the bulk compression passes: the two complementary
// listings compress-existing and decompress-existing walk, and the update that
// records how a rewritten copy is now stored.
//
// The update also moves the backend's quota, because a rewrite changes how many
// bytes the copy occupies. Doing both in one transaction is what keeps
// object_locations.size_bytes and backend_quotas.bytes_used from disagreeing
// when a pass is interrupted partway.
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
func (s *Store) ListUncompressedLocations(ctx context.Context, limit int, after core.Cursor, t core.CompressionThresholds, backend string) ([]core.RewritableLocation, error) {
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
	return rewritablesFrom(rows), nil
}

// ListCompressedLocations returns a page of copies whose bytes are an encoding.
func (s *Store) ListCompressedLocations(ctx context.Context, limit int, after core.Cursor, backend string) ([]core.RewritableLocation, error) {
	rows, err := s.queries.ListCompressedLocations(ctx, db.ListCompressedLocationsParams{
		BackendFilter: backend,
		AfterKey:      after.ObjectKey,
		AfterBackend:  after.BackendName,
		RowLimit:      int32(limit), //nolint:gosec // G115: limit is a small caller-controlled batch size
	})
	if err != nil {
		return nil, fmt.Errorf("list compressed locations: %w", err)
	}
	return rewritablesFrom(rows), nil
}

// -------------------------------------------------------------------------
// STATISTICS AND WRITES
// -------------------------------------------------------------------------

// RecordCompressionProbe stores what the encoder produced for a copy it
// declined to store compressed, so a later pass can reach the same verdict
// from the row rather than downloading and encoding the object again.
func (s *Store) RecordCompressionProbe(ctx context.Context, probe *core.CompressionProbe) error {
	return s.direct().RecordCompressionProbe(ctx, probe)
}

// rewritableRow is the shape both listings return. The two generated row types
// are structurally identical, so one conversion serves both.
type rewritableRow = db.ListUncompressedLocationsRow

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// rewritablesFrom converts a page from either listing to the canonical type.
func rewritablesFrom[R db.ListUncompressedLocationsRow | db.ListCompressedLocationsRow](rows []R) []core.RewritableLocation {
	out := make([]core.RewritableLocation, len(rows))
	for i := range rows {
		row := rewritableRow(rows[i])
		out[i] = rewritableFromRow(&row)
	}
	return out
}

// rewritableFromRow converts one generated row to the canonical type.
func rewritableFromRow(r *rewritableRow) core.RewritableLocation {
	return core.RewritableLocation{
		ObjectKey:                r.ObjectKey,
		BackendName:              r.BackendName,
		StorageKey:               r.StorageKey,
		SizeBytes:                r.SizeBytes,
		Encrypted:                r.Encrypted,
		EncryptionKey:            r.EncryptionKey,
		KeyID:                    derefStr(r.KeyID),
		PlaintextSize:            derefInt64(r.PlaintextSize),
		CompressionAlgorithm:     derefStr(r.CompressionAlgorithm),
		CompressionLevel:         derefStr(r.CompressionLevel),
		CompressionFormatVersion: int(derefOr(r.CompressionFormatVersion, 0)),
		LogicalSize:              derefInt64(r.LogicalSize),
		Etag:                     derefStr(r.Etag),
	}
}
