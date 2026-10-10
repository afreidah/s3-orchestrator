// -------------------------------------------------------------------------------
// Compression Admin Operations
//
// Author: Alex Freidah
//
// SQLite bindings for the bulk compression passes: the two complementary
// listings compress-existing and decompress-existing walk, and the update that
// records how a rewritten copy is now stored.
//
// The update also moves the backend's quota, because a rewrite changes how many
// bytes the copy occupies. Both happen in one transaction so an interrupted
// pass cannot leave object_locations.size_bytes and backend_quotas.bytes_used
// disagreeing.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// PROJECTION AND PREDICATES
// -------------------------------------------------------------------------

// rewritableColumns is the projection both compression listings read.
const rewritableColumns = `object_key, backend_name, storage_key, size_bytes, encrypted, encryption_key,
	key_id, plaintext_size, compression_algorithm, compression_level,
	compression_format_version, logical_size, etag`

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
func (s *Store) ListUncompressedLocations(ctx context.Context, limit int, after core.Cursor, t core.CompressionThresholds, backend string) ([]core.RewritableLocation, error) {
	return s.listRewritable(ctx, uncompressedPredicate, limit, after, backend, t.MinSize, t.Level, t.MinRatio)
}

// ListCompressedLocations returns a page of copies whose bytes are an encoding,
// which is what decompress-existing rewrites.
func (s *Store) ListCompressedLocations(ctx context.Context, limit int, after core.Cursor, backend string) ([]core.RewritableLocation, error) {
	return s.listRewritable(ctx, compressedPredicate, limit, after, backend)
}

// listRewritable runs one page of either listing. predicate is one of two
// package constants, never caller input, and args binds its placeholders. An
// empty backend selects every backend.
//
// Paging is by cursor because each processed row leaves the predicate that
// selected it, so an offset would skip the rows that moved up.
func (s *Store) listRewritable(ctx context.Context, predicate string, limit int, after core.Cursor, backend string, args ...any) ([]core.RewritableLocation, error) {
	args = append([]any{backend, backend}, args...)
	args = append(args, after.ObjectKey, after.BackendName, limit)
	//nolint:gosec // G202: predicate is one of two package constants, never caller input
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+rewritableColumns+`
		FROM object_locations
		WHERE (? = '' OR backend_name = ?)
		  AND `+predicate+`
		  AND (object_key, backend_name) > (?, ?)
		ORDER BY object_key, backend_name
		LIMIT ?`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("list rewritable locations: %w", err)
	}
	return collectRows(rows, "rewritable locations", scanRewritable)
}

// scanRewritable reads one row of the rewritable projection.
func scanRewritable(rows *sql.Rows) (core.RewritableLocation, error) {
	var (
		loc           core.RewritableLocation
		keyID         sql.NullString
		plaintextSize sql.NullInt64
		algorithm     sql.NullString
		level         sql.NullString
		formatVersion sql.NullInt64
		logicalSize   sql.NullInt64
		etag          sql.NullString
	)
	if err := rows.Scan(
		&loc.ObjectKey, &loc.BackendName, &loc.StorageKey, &loc.SizeBytes, &loc.Encrypted, &loc.EncryptionKey,
		&keyID, &plaintextSize, &algorithm, &level, &formatVersion, &logicalSize, &etag,
	); err != nil {
		return core.RewritableLocation{}, fmt.Errorf("scan rewritable location: %w", err)
	}
	loc.KeyID = nullStringValue(keyID)
	loc.PlaintextSize = plaintextSize.Int64
	loc.CompressionAlgorithm = nullStringValue(algorithm)
	loc.CompressionLevel = nullStringValue(level)
	loc.CompressionFormatVersion = int(formatVersion.Int64)
	loc.LogicalSize = logicalSize.Int64
	loc.Etag = nullStringValue(etag)
	return loc, nil
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
