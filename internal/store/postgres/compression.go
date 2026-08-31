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

// ListUncompressedLocations returns a page of copies whose bytes carry no
// encoding and that the supplied thresholds do not already exclude.
func (s *Store) ListUncompressedLocations(ctx context.Context, limit int, after core.Cursor, t core.CompressionThresholds) ([]core.RewritableLocation, error) {
	rows, err := s.queries.ListUncompressedLocations(ctx, db.ListUncompressedLocationsParams{
		MinSize:      t.MinSize,
		ProbeLevel:   t.Level,
		MinRatio:     t.MinRatio,
		AfterKey:     after.ObjectKey,
		AfterBackend: after.BackendName,
		RowLimit:     int32(limit), //nolint:gosec // G115: limit is a small caller-controlled batch size
	})
	if err != nil {
		return nil, fmt.Errorf("list uncompressed locations: %w", err)
	}
	out := make([]core.RewritableLocation, len(rows))
	for i := range rows {
		out[i] = rewritableFromRow((*rewritableRow)(&rows[i]))
	}
	return out, nil
}

// ListCompressedLocations returns a page of copies whose bytes are an encoding.
func (s *Store) ListCompressedLocations(ctx context.Context, limit int, after core.Cursor) ([]core.RewritableLocation, error) {
	rows, err := s.queries.ListCompressedLocations(ctx, db.ListCompressedLocationsParams{
		AfterKey:     after.ObjectKey,
		AfterBackend: after.BackendName,
		RowLimit:     int32(limit), //nolint:gosec // G115: limit is a small caller-controlled batch size
	})
	if err != nil {
		return nil, fmt.Errorf("list compressed locations: %w", err)
	}
	out := make([]core.RewritableLocation, len(rows))
	for i := range rows {
		out[i] = rewritableFromRow((*rewritableRow)(&rows[i]))
	}
	return out, nil
}

// CompressionStats reports per-backend compression totals for the dashboard.
// Backends holding no encoded copies are absent rather than present as zeroes,
// so a caller can tell "nothing compressed here" from "compressed to nothing".
func (s *Store) CompressionStats(ctx context.Context) (map[string]core.CompressionStat, error) {
	rows, err := s.queries.CompressionStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("compression stats: %w", err)
	}
	out := make(map[string]core.CompressionStat, len(rows))
	for i := range rows {
		out[rows[i].BackendName] = core.CompressionStat{
			Objects:      rows[i].Objects,
			LogicalBytes: rows[i].LogicalBytes,
			StoredBytes:  rows[i].StoredBytes,
		}
	}
	return out, nil
}

// RecordCompressionProbe stores what the encoder produced for a copy it
// declined to store compressed, so a later pass can reach the same verdict
// from the row rather than downloading and encoding the object again.
func (s *Store) RecordCompressionProbe(ctx context.Context, probe *core.CompressionProbe) error {
	if err := s.queries.RecordCompressionProbe(ctx, db.RecordCompressionProbeParams{
		ObjectKey:             probe.ObjectKey,
		BackendName:           probe.BackendName,
		CompressionProbeSize:  int64Ptr(probe.Size),
		CompressionProbeLevel: strPtr(probe.Level),
	}); err != nil {
		return fmt.Errorf("record compression probe: %w", err)
	}
	return nil
}

// rewritableRow is the shape both listings return. The two generated row types
// are structurally identical, so one conversion serves both.
type rewritableRow = db.ListUncompressedLocationsRow

// rewritableFromRow converts one generated row to the canonical type.
func rewritableFromRow(r *rewritableRow) core.RewritableLocation {
	return core.RewritableLocation{
		ObjectKey:                r.ObjectKey,
		BackendName:              r.BackendName,
		SizeBytes:                r.SizeBytes,
		Encrypted:                r.Encrypted,
		EncryptionKey:            r.EncryptionKey,
		KeyID:                    derefStr(r.KeyID),
		PlaintextSize:            derefInt64(r.PlaintextSize),
		CompressionAlgorithm:     derefStr(r.CompressionAlgorithm),
		CompressionLevel:         derefStr(r.CompressionLevel),
		CompressionFormatVersion: int(derefOr(r.CompressionFormatVersion, 0)),
		LogicalSize:              derefInt64(r.LogicalSize),
	}
}
