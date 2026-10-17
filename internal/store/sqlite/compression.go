// -------------------------------------------------------------------------------
// Compression Admin Operations
//
// Author: Alex Freidah
//
// SQLite bindings for the two complementary listings compress-existing and
// decompress-existing walk.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// PROJECTION AND PREDICATES
// -------------------------------------------------------------------------

// uncompressedPredicate selects the copies compress-existing rewrites: stored
// verbatim, big enough to be worth encoding, and not already measured as unable
// to reach the configured ratio. Filtering here keeps every pass from spending
// a page slot, and for measured copies a download and an encode, on a copy it
// would decline again.
//
// The recorded measurement is judged against the current settings, so
// loosening the ratio returns those copies with no read. A measurement taken at
// a different level is ignored, since it describes an encoding the pass would
// no longer produce. The divisor is NULLIF'd so a zero-length copy compares
// NULL and is excluded, matching WorthStoring.
const uncompressedPredicate = `compression_algorithm IS NULL
	AND (CASE WHEN encrypted THEN plaintext_size ELSE size_bytes END) >= ?
	AND (compression_probe_size IS NULL
	     OR compression_probe_level IS NOT ?
	     OR CAST(compression_probe_size AS REAL)
	        / NULLIF(CASE WHEN encrypted THEN plaintext_size ELSE size_bytes END, 0)
	        <= ?)`

// compressedPredicate selects the copies decompress-existing rewrites. It needs
// no equivalent of the probe filter: every copy this pass succeeds on leaves the
// predicate, and there is no decision it can decline on.
const compressedPredicate = `compression_algorithm IS NOT NULL`

// -------------------------------------------------------------------------
// LISTINGS
// -------------------------------------------------------------------------

// ListUncompressedLocations returns a page of copies whose bytes carry no
// encoding, which is what compress-existing rewrites.
func (s *Store) ListUncompressedLocations(ctx context.Context, limit int, after core.Cursor, t core.CompressionThresholds, backend string) ([]core.ObjectLocation, error) {
	return s.listCopies(ctx, "uncompressed locations", uncompressedPredicate, limit, after, backend, t.MinSize, t.Level, t.MinRatio)
}

// ListCompressedLocations returns a page of copies whose bytes are an encoding,
// which is what decompress-existing rewrites.
func (s *Store) ListCompressedLocations(ctx context.Context, limit int, after core.Cursor, backend string) ([]core.ObjectLocation, error) {
	return s.listCopies(ctx, "compressed locations", compressedPredicate, limit, after, backend)
}
