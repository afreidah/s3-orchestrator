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

// ListUncompressedLocations returns a page of copies whose bytes carry no
// encoding, which is what compress-existing rewrites.
func (s *Store) ListUncompressedLocations(ctx context.Context, limit int, after core.Cursor) ([]core.RewritableLocation, error) {
	return s.listRewritable(ctx, "compression_algorithm IS NULL", limit, after)
}

// ListCompressedLocations returns a page of copies whose bytes are an encoding,
// which is what decompress-existing rewrites.
func (s *Store) ListCompressedLocations(ctx context.Context, limit int, after core.Cursor) ([]core.RewritableLocation, error) {
	return s.listRewritable(ctx, "compression_algorithm IS NOT NULL", limit, after)
}

// listRewritable runs one page of either listing. The predicate is the only
// difference between them, and it is a constant at both call sites rather than
// anything a caller supplies.
//
// Paging is by cursor because the passes that walk these listings rewrite the
// rows they read: each one processed leaves the predicate that selected it, so
// an offset would advance into a set that shrank and skip the rows that moved
// up to fill the gap.
func (s *Store) listRewritable(ctx context.Context, predicate string, limit int, after core.Cursor) ([]core.RewritableLocation, error) {
	//nolint:gosec // G202: predicate is one of two package constants, never caller input
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+rewritableColumns+`
		FROM object_locations
		WHERE `+predicate+`
		  AND (object_key, backend_name) > (?, ?)
		ORDER BY object_key, backend_name
		LIMIT ?`,
		after.ObjectKey, after.BackendName, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list rewritable locations: %w", err)
	}
	defer rows.Close()

	var locs []core.RewritableLocation
	for rows.Next() {
		loc, err := scanRewritable(rows)
		if err != nil {
			return nil, err
		}
		locs = append(locs, loc)
	}
	return locs, rows.Err()
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
	defer rows.Close()

	out := make(map[string]core.CompressionStat)
	for rows.Next() {
		var (
			name string
			stat core.CompressionStat
		)
		if err := rows.Scan(&name, &stat.Objects, &stat.LogicalBytes, &stat.StoredBytes); err != nil {
			return nil, fmt.Errorf("scan compression stats: %w", err)
		}
		out[name] = stat
	}
	return out, rows.Err()
}

// MarkObjectCompressed records the new stored form of a rewritten copy and
// moves the backend's quota by the difference between what the copy occupied
// before and what it occupies now.
//
// The envelope columns are rewritten too: re-encrypting an object mints a new
// base nonce and wrapped key, so leaving the old ones would describe bytes
// nothing can decrypt.
func (s *Store) MarkObjectCompressed(ctx context.Context, u *core.CompressedUpdate, previousSize int64) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE object_locations
			SET compression_algorithm = ?, compression_level = ?,
			    compression_format_version = ?, logical_size = ?,
			    size_bytes = ?, plaintext_size = ?,
			    encryption_key = ?, key_id = ?
			WHERE object_key = ? AND backend_name = ?`,
			nullableString(u.Algorithm), nullableString(u.Level),
			nullableInt64(int64(u.FormatVersion)), nullableInt64(u.LogicalSize),
			u.SizeBytes, nullableInt64(u.PlaintextSize),
			u.EncryptionKey, nullableString(u.KeyID),
			u.ObjectKey, u.BackendName,
		)
		if err != nil {
			return fmt.Errorf("mark compressed: %w", err)
		}

		sizeDelta := u.SizeBytes - previousSize
		if sizeDelta == 0 {
			return nil
		}
		// MAX(0, ...) for the same reason the encryption pass clamps: a stale
		// size must not leave the counter negative, which would over-admit
		// every later write.
		if _, err := tx.ExecContext(ctx, `
			UPDATE backend_quotas
			SET bytes_used = MAX(0, bytes_used + ?), updated_at = ?
			WHERE backend_name = ?`,
			sizeDelta, now(), u.BackendName,
		); err != nil {
			return fmt.Errorf("adjust quota for compression: %w", err)
		}
		return nil
	})
}
