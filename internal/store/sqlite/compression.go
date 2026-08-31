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

// rewritableColumns is the projection both compression listings read.
const rewritableColumns = `object_key, backend_name, size_bytes, encrypted, encryption_key,
	key_id, plaintext_size, compression_algorithm, compression_level,
	compression_format_version, logical_size`

// uncompressedPredicate selects the copies compress-existing rewrites: stored
// verbatim, big enough to be worth encoding, and not already measured as unable
// to reach the configured ratio.
//
// Both exclusions are durable answers, which is why they belong here rather
// than in the pass. A copy under the size floor is never a candidate, and one
// already measured stays declined until a setting changes - so listing either
// only to decline it again spends a page slot, and in the measured case a
// download and an encode, on every pass forever.
//
// The recorded measurement is judged against the current settings rather than
// treated as a verdict: loosening the ratio returns those copies to the pass
// with no read at all. A measurement taken at a different level is ignored,
// since it describes an encoding the pass would no longer produce and the
// levels are names from an ordered set rather than numbers.
//
// The divisor is NULLIF'd because a zero-length copy cannot shrink: the
// comparison goes NULL and the row is excluded, matching WorthStoring.
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

// ListUncompressedLocations returns a page of copies whose bytes carry no
// encoding, which is what compress-existing rewrites.
func (s *Store) ListUncompressedLocations(ctx context.Context, limit int, after core.Cursor, t core.CompressionThresholds) ([]core.RewritableLocation, error) {
	return s.listRewritable(ctx, uncompressedPredicate, limit, after, t.MinSize, t.Level, t.MinRatio)
}

// ListCompressedLocations returns a page of copies whose bytes are an encoding,
// which is what decompress-existing rewrites.
func (s *Store) ListCompressedLocations(ctx context.Context, limit int, after core.Cursor) ([]core.RewritableLocation, error) {
	return s.listRewritable(ctx, compressedPredicate, limit, after)
}

// listRewritable runs one page of either listing. The predicate is the only
// difference between them, and it is one of two package constants rather than
// anything a caller supplies; args binds whatever placeholders it carries.
//
// Paging is by cursor because the passes that walk these listings rewrite the
// rows they read: each one processed leaves the predicate that selected it, so
// an offset would advance into a set that shrank and skip the rows that moved
// up to fill the gap.
func (s *Store) listRewritable(ctx context.Context, predicate string, limit int, after core.Cursor, args ...any) ([]core.RewritableLocation, error) {
	args = append(args, after.ObjectKey, after.BackendName, limit)
	//nolint:gosec // G202: predicate is one of two package constants, never caller input
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+rewritableColumns+`
		FROM object_locations
		WHERE `+predicate+`
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
	)
	if err := rows.Scan(
		&loc.ObjectKey, &loc.BackendName, &loc.SizeBytes, &loc.Encrypted, &loc.EncryptionKey,
		&keyID, &plaintextSize, &algorithm, &level, &formatVersion, &logicalSize,
	); err != nil {
		return core.RewritableLocation{}, fmt.Errorf("scan rewritable location: %w", err)
	}
	loc.KeyID = nullStringValue(keyID)
	loc.PlaintextSize = plaintextSize.Int64
	loc.CompressionAlgorithm = nullStringValue(algorithm)
	loc.CompressionLevel = nullStringValue(level)
	loc.CompressionFormatVersion = int(formatVersion.Int64)
	loc.LogicalSize = logicalSize.Int64
	return loc, nil
}

// CompressionStats reports per-backend compression totals for the dashboard.
// Backends holding no encoded copies are absent rather than present as zeroes,
// so a caller can tell "nothing compressed here" from "compressed to nothing".
func (s *Store) CompressionStats(ctx context.Context) (map[string]core.CompressionStat, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT backend_name, COUNT(*),
		       COALESCE(SUM(logical_size), 0), COALESCE(SUM(size_bytes), 0)
		FROM object_locations
		WHERE compression_algorithm IS NOT NULL
		GROUP BY backend_name`)
	if err != nil {
		return nil, fmt.Errorf("compression stats: %w", err)
	}
	return collectMap(rows, "compression stats", func(rows *sql.Rows) (string, core.CompressionStat, error) {
		var (
			name string
			stat core.CompressionStat
		)
		if err := rows.Scan(&name, &stat.Objects, &stat.LogicalBytes, &stat.StoredBytes); err != nil {
			return "", core.CompressionStat{}, fmt.Errorf("scan compression stats: %w", err)
		}
		return name, stat, nil
	})
}

// RecordCompressionProbe stores what the encoder produced for a copy it
// declined to store compressed, so a later pass can reach the same verdict
// from the row rather than downloading and encoding the object again.
func (s *Store) RecordCompressionProbe(ctx context.Context, probe *core.CompressionProbe) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE object_locations
		 SET compression_probe_size = ?, compression_probe_level = ?
		 WHERE object_key = ? AND backend_name = ?`,
		probe.Size, probe.Level, probe.ObjectKey, probe.BackendName,
	); err != nil {
		return fmt.Errorf("record compression probe: %w", err)
	}
	return nil
}
