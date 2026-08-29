// -------------------------------------------------------------------------------
// SQLite Object Operations - Location CRUD, Listing, and Integrity
//
// Author: Alex Freidah
//
// Implements object location CRUD, prefix-based listing with deduplication,
// expired object queries, backend-scoped listing, import, and integrity
// verification operations. Uses GROUP BY + MIN(rowid) subqueries to replace
// PostgreSQL's DISTINCT ON for replica deduplication.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// likeEscaper escapes SQL LIKE wildcards in prefix strings.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// errInvalidTimestamp is the wrap format string used everywhere a
// stored RFC 3339 created_at fails time.Parse. Centralised so the
// surfaced error stays consistent across listing helpers.
const errInvalidTimestamp = "invalid created_at timestamp %q: %w"

// -------------------------------------------------------------------------
// READ QUERIES
// -------------------------------------------------------------------------

// GetObjectBackendsForKeys returns a map from each supplied object_key to
// the backends that hold a copy. Empty input yields an empty map; keys
// with no copies are absent from the result. Used by the rebalancer
// planner to fold the per-key existence check into a single query per
// batch instead of N+1.
//
// The query uses SQLite's json_each so the SQL stays static and the
// keys array is passed as a single JSON-encoded parameter rather than
// interpolated into the SQL string.
func (s *Store) GetObjectBackendsForKeys(ctx context.Context, keys []string) (map[string][]string, error) {
	if len(keys) == 0 {
		return map[string][]string{}, nil
	}
	keysJSON, err := json.Marshal(keys)
	if err != nil {
		return nil, fmt.Errorf("marshal keys: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT object_key, backend_name
		FROM object_locations
		WHERE object_key IN (SELECT value FROM json_each(?))`, string(keysJSON))
	if err != nil {
		return nil, fmt.Errorf("failed to get object backends for keys: %w", err)
	}
	defer rows.Close()
	out := make(map[string][]string, len(keys))
	for rows.Next() {
		var key, backend string
		if err := rows.Scan(&key, &backend); err != nil {
			return nil, fmt.Errorf("failed to scan key/backend pair: %w", err)
		}
		out[key] = append(out[key], backend)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate key/backend rows: %w", err)
	}
	return out, nil
}

// GetAllObjectLocations returns all copies of an object, ordered by created_at
// ascending (oldest/primary first). Used for read failover.
func (s *Store) GetAllObjectLocations(ctx context.Context, key string) ([]core.ObjectLocation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT object_key, backend_name, size_bytes, encrypted, encryption_key,
		       key_id, plaintext_size, content_hash,
		       compression_algorithm, compression_level, compression_format_version, logical_size,
		       created_at, last_scrubbed_at
		FROM object_locations
		WHERE object_key = ?
		ORDER BY created_at ASC`, key)
	if err != nil {
		return nil, fmt.Errorf("failed to get object locations: %w", err)
	}
	defer rows.Close()

	var locs []core.ObjectLocation
	for rows.Next() {
		loc, err := scanObjectLocation(rows)
		if err != nil {
			return nil, err
		}
		locs = append(locs, loc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate object locations: %w", err)
	}

	if len(locs) == 0 {
		return nil, core.ErrObjectNotFound
	}
	return locs, nil
}

// -------------------------------------------------------------------------
// LISTING
// -------------------------------------------------------------------------

// ListObjects returns objects matching the given prefix, sorted by key.
// Supports pagination via startAfter and maxKeys. Returns one extra row to
// detect truncation. Uses a subquery with GROUP BY to deduplicate replicated
// objects (equivalent to DISTINCT ON in PostgreSQL).
func (s *Store) ListObjects(ctx context.Context, prefix, startAfter string, maxKeys int) (*core.ListObjectsResult, error) {
	if maxKeys <= 0 {
		maxKeys = 1000
	}

	escapedPrefix := likeEscaper.Replace(prefix)

	// Subquery with GROUP BY + MIN(rowid) replaces DISTINCT ON (object_key).
	rows, err := s.db.QueryContext(ctx, `
		SELECT ol.object_key, ol.backend_name, ol.size_bytes, ol.created_at
		FROM object_locations ol
		INNER JOIN (
			SELECT object_key, MIN(rowid) AS min_rowid
			FROM object_locations
			WHERE object_key LIKE ? || '%' ESCAPE '\'
			  AND object_key > ?
			GROUP BY object_key
		) dedup ON ol.rowid = dedup.min_rowid
		ORDER BY ol.object_key
		LIMIT ?`, escapedPrefix, startAfter, maxKeys+1)
	if err != nil {
		return nil, fmt.Errorf("failed to list objects: %w", err)
	}
	defer rows.Close()

	objects, err := scanSlimObjectLocations(rows)
	if err != nil {
		return nil, err
	}
	return core.BuildListPage(objects, maxKeys), nil
}

// ListObjectsDelimited groups a delimiter listing inside SQLite with a recursive
// CTE whose recursive term carries a scalar-subquery seek: each step jumps to
// the next key past the current group instead of scanning through it. Collation
// is SQLite's native BINARY (object_key byte order), and instr/substr/char
// compute each group plus its skip bound (the CommonPrefix with its last byte
// incremented, or the leaf key). Keys with a delimiter after the prefix fold
// into CommonPrefixes; the rest come back as leaf objects. The delimiter must be
// non-empty.
func (s *Store) ListObjectsDelimited(ctx context.Context, prefix, delimiter, startAfter string, maxKeys int) (*core.ListDelimitedResult, error) {
	if maxKeys <= 0 {
		maxKeys = 1000
	}
	escapedPrefix := likeEscaper.Replace(prefix)

	// dpos    = position of the first delimiter after the prefix (0 = none -> leaf)
	// cplen   = prefix length + delimiter offset = length of the CommonPrefix
	// skip    = CommonPrefix with its last char incremented (skips the whole
	//           group in one seek), or the key itself for a leaf
	const query = `
		WITH RECURSIVE walk(k) AS (
			SELECT (
				SELECT object_key FROM object_locations
				WHERE object_key LIKE :escprefix || '%' ESCAPE '\'
				  AND object_key > :startafter
				ORDER BY object_key LIMIT 1
			)
			UNION ALL
			SELECT (
				SELECT object_key FROM object_locations
				WHERE object_key LIKE :escprefix || '%' ESCAPE '\'
				  AND object_key > CASE
					WHEN instr(substr(walk.k, length(:prefix) + 1), :delim) > 0 THEN
						substr(walk.k, 1, length(:prefix) + instr(substr(walk.k, length(:prefix) + 1), :delim) + length(:delim) - 2)
						|| char(unicode(substr(walk.k, length(:prefix) + instr(substr(walk.k, length(:prefix) + 1), :delim) + length(:delim) - 1, 1)) + 1)
					ELSE walk.k
				  END
				ORDER BY object_key LIMIT 1
			)
			FROM walk WHERE walk.k IS NOT NULL
		)
		SELECT
			w.k,
			CASE WHEN instr(substr(w.k, length(:prefix) + 1), :delim) > 0 THEN 1 ELSE 0 END AS is_prefix,
			CASE WHEN instr(substr(w.k, length(:prefix) + 1), :delim) > 0
				THEN substr(w.k, 1, length(:prefix) + instr(substr(w.k, length(:prefix) + 1), :delim) + length(:delim) - 1)
				ELSE NULL END AS common_prefix,
			CASE
				WHEN instr(substr(w.k, length(:prefix) + 1), :delim) > 0 THEN
					substr(w.k, 1, length(:prefix) + instr(substr(w.k, length(:prefix) + 1), :delim) + length(:delim) - 2)
					|| char(unicode(substr(w.k, length(:prefix) + instr(substr(w.k, length(:prefix) + 1), :delim) + length(:delim) - 1, 1)) + 1)
				ELSE w.k
			END AS skip_bound,
			ol.backend_name, ol.size_bytes, ol.created_at
		FROM walk w
		LEFT JOIN object_locations ol ON ol.rowid = (
			SELECT MIN(rowid) FROM object_locations o2
			WHERE o2.object_key = w.k
			  AND instr(substr(w.k, length(:prefix) + 1), :delim) = 0
		)
		WHERE w.k IS NOT NULL
		ORDER BY w.k
		LIMIT :limit`

	rows, err := s.db.QueryContext(ctx, query,
		sql.Named("escprefix", escapedPrefix),
		sql.Named("prefix", prefix),
		sql.Named("delim", delimiter),
		sql.Named("startafter", startAfter),
		sql.Named("limit", maxKeys+1),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list objects (delimited): %w", err)
	}
	defer rows.Close()

	entries, err := scanDelimitedEntries(rows)
	if err != nil {
		return nil, err
	}
	return core.BuildDelimitedPage(entries, maxKeys), nil
}

// scanDelimitedEntries reads the loose-index-scan rows. Leaf columns are NULL on
// CommonPrefix rows (the LEFT JOIN only matches leaves).
func scanDelimitedEntries(rows *sql.Rows) ([]core.DelimitedEntry, error) {
	var entries []core.DelimitedEntry
	for rows.Next() {
		var (
			key       string
			isPrefix  int
			commonPfx sql.NullString
			skipBound string
			backend   sql.NullString
			sizeBytes sql.NullInt64
			createdAt sql.NullString
		)
		if err := rows.Scan(&key, &isPrefix, &commonPfx, &skipBound, &backend, &sizeBytes, &createdAt); err != nil {
			return nil, fmt.Errorf("failed to scan delimited entry: %w", err)
		}
		e := core.DelimitedEntry{IsPrefix: isPrefix != 0, CommonPrefix: commonPfx.String, SkipBound: skipBound}
		if !e.IsPrefix {
			e.Leaf.ObjectKey = key
			e.Leaf.BackendName = backend.String
			e.Leaf.SizeBytes = sizeBytes.Int64
			t, parseErr := parseTime(createdAt.String)
			if parseErr != nil {
				return nil, fmt.Errorf(errInvalidTimestamp, createdAt.String, parseErr)
			}
			e.Leaf.CreatedAt = t
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate delimited entries: %w", err)
	}
	return entries, nil
}

// ListExpiredObjects returns one row per unique key matching the query's
// filters whose created_at is older than its cutoff, up to Limit rows. Used by
// lifecycle expiration to find objects eligible for deletion.
//
// One EXISTS per tag, all required, which is what makes several tags an
// intersection. EXISTS rather than a join because the dedup subquery groups by
// object_key and a join would multiply its input row per matching tag.
func (s *Store) ListExpiredObjects(ctx context.Context, q core.ExpiredObjectsQuery) ([]core.ObjectLocation, error) {
	args := []any{likeEscaper.Replace(q.Prefix), formatTime(q.Cutoff)}

	var tagFilter strings.Builder
	for _, key := range sortedTagKeys(q.Tags) {
		tagFilter.WriteString(`
			  AND EXISTS (SELECT 1 FROM object_tags t
			              WHERE t.object_key = object_locations.object_key
			                AND t.tag_key = ? AND t.tag_value = ?)`)
		args = append(args, key, q.Tags[key])
	}
	args = append(args, q.Limit)

	// Subquery with GROUP BY + MIN(rowid) replaces DISTINCT ON (object_key).
	rows, err := s.db.QueryContext(ctx, `
		SELECT ol.object_key, ol.backend_name, ol.size_bytes, ol.created_at
		FROM object_locations ol
		INNER JOIN (
			SELECT object_key, MIN(rowid) AS min_rowid
			FROM object_locations
			WHERE object_key LIKE ? || '%' ESCAPE '\'
			  AND created_at < ?`+tagFilter.String()+`
			GROUP BY object_key
		) dedup ON ol.rowid = dedup.min_rowid
		ORDER BY ol.object_key
		LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list expired objects: %w", err)
	}
	defer rows.Close()

	return scanSlimObjectLocations(rows)
}

// sortedTagKeys orders a tag filter's keys so the generated SQL and its
// arguments are identical run to run, which keeps the statement cacheable and
// a failure reproducible.
func sortedTagKeys(tags map[string]string) []string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ListObjectsByBackend returns objects stored on a specific backend, ordered by
// size ascending (smallest first). Backs the rebalance, placement and drain
// candidate scans, so it returns managed rows only.
func (s *Store) ListObjectsByBackend(ctx context.Context, backendName string, limit int) ([]core.ObjectLocation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT object_key, backend_name, size_bytes, created_at
		FROM object_locations
		WHERE backend_name = ? AND managed
		ORDER BY size_bytes ASC
		LIMIT ?`, backendName, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list objects by backend: %w", err)
	}
	defer rows.Close()

	return scanSlimObjectLocations(rows)
}

// ListObjectsByBackendKeyAsc returns rows for a backend in ascending
// object_key order, starting strictly after afterKey. The empty string
// returns the first page. Used by ReconcileBackend's bounded-memory
// sorted-merge join against an S3 ListObjects walk; both sides are in lex
// order so the merge is O(limit) memory bounded.
func (s *Store) ListObjectsByBackendKeyAsc(ctx context.Context, backendName, afterKey string, limit int) ([]core.ObjectLocation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT object_key, backend_name, size_bytes, created_at
		FROM object_locations
		WHERE backend_name = ? AND object_key > ?
		ORDER BY object_key ASC
		LIMIT ?`, backendName, afterKey, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to page objects by backend: %w", err)
	}
	defer rows.Close()

	return scanSlimObjectLocations(rows)
}

// scanSlimObjectLocations consumes a *sql.Rows holding the slim
// (object_key, backend_name, size_bytes, created_at) projection used by
// ListObjects, ListExpiredObjects, and ListObjectsByBackend. Centralizes
// the per-row scan + RFC3339 timestamp parse so the three list helpers
// don't carry parallel loop bodies.
func scanSlimObjectLocations(rows *sql.Rows) ([]core.ObjectLocation, error) {
	var locs []core.ObjectLocation
	for rows.Next() {
		var loc core.ObjectLocation
		var createdAt string
		if err := rows.Scan(&loc.ObjectKey, &loc.BackendName, &loc.SizeBytes, &createdAt); err != nil {
			return nil, fmt.Errorf("failed to scan object location: %w", err)
		}
		var parseErr error
		loc.CreatedAt, parseErr = parseTime(createdAt)
		if parseErr != nil {
			return nil, fmt.Errorf(errInvalidTimestamp, createdAt, parseErr)
		}
		locs = append(locs, loc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate object locations: %w", err)
	}
	return locs, nil
}

// BackendObjectStats returns the object count and total bytes stored on a backend.
func (s *Store) BackendObjectStats(ctx context.Context, backendName string) (int64, int64, error) {
	var count, totalBytes int64
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(size_bytes), 0)
		FROM object_locations
		WHERE backend_name = ?`, backendName).Scan(&count, &totalBytes)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get backend object stats: %w", err)
	}
	return count, totalBytes, nil
}

// DeleteBackendData removes all database records for a backend in FK-safe order.
// Runs in a single transaction.
func (s *Store) DeleteBackendData(ctx context.Context, backendName string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		stmts := []string{
			`DELETE FROM cleanup_queue WHERE backend_name = ?`,
			`DELETE FROM multipart_parts WHERE upload_id IN (SELECT upload_id FROM multipart_uploads WHERE backend_name = ?)`,
			`DELETE FROM multipart_uploads WHERE backend_name = ?`,
			`DELETE FROM object_locations WHERE backend_name = ?`,
			`DELETE FROM backend_usage WHERE backend_name = ?`,
			`DELETE FROM backend_quotas WHERE backend_name = ?`,
		}
		for _, stmt := range stmts {
			if _, err := tx.ExecContext(ctx, stmt, backendName); err != nil {
				return fmt.Errorf("failed to execute %q: %w", stmt, err)
			}
		}
		return nil
	})
}

// -------------------------------------------------------------------------
// INTEGRITY
// -------------------------------------------------------------------------

// GetLeastRecentlyScrubbedObjects returns the copies least recently touched,
// by verification or by writing. Falling back to created_at keeps a freshly
// written copy from jumping the queue, so a write rate above the scrub rate
// cannot starve older data.
//
// backends restricts the batch to copies the scrubber can afford to read. An
// empty slice selects nothing: the caller has established that no backend can
// be read right now, and returning the whole queue would ignore it.
func (s *Store) GetLeastRecentlyScrubbedObjects(ctx context.Context, limit int, backends []string) ([]core.ObjectLocation, error) {
	if len(backends) == 0 {
		return nil, nil
	}
	backendsJSON, err := json.Marshal(backends)
	if err != nil {
		return nil, fmt.Errorf("encode scrub backend list: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT object_key, backend_name, size_bytes, encrypted, encryption_key,
		       key_id, plaintext_size, content_hash,
		       compression_algorithm, compression_level, compression_format_version, logical_size,
		       created_at, last_scrubbed_at
		FROM object_locations
		WHERE content_hash IS NOT NULL AND managed
		  AND backend_name IN (SELECT value FROM json_each(?))
		ORDER BY COALESCE(last_scrubbed_at, created_at) ASC, object_key ASC
		LIMIT ?`, string(backendsJSON), limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get least recently scrubbed objects: %w", err)
	}
	defer rows.Close()

	var locs []core.ObjectLocation
	for rows.Next() {
		loc, err := scanObjectLocation(rows)
		if err != nil {
			return nil, err
		}
		locs = append(locs, loc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate least recently scrubbed objects: %w", err)
	}
	return locs, nil
}

// CountScrubCandidatesOnBackends reports how many scrubbable copies live on the
// named backends, so a cycle can say how much of the queue it declined to read.
func (s *Store) CountScrubCandidatesOnBackends(ctx context.Context, backends []string) (int64, error) {
	if len(backends) == 0 {
		return 0, nil
	}
	backendsJSON, err := json.Marshal(backends)
	if err != nil {
		return 0, fmt.Errorf("encode scrub backend list: %w", err)
	}
	var n int64
	err = s.db.QueryRowContext(ctx, `
		SELECT count(*)
		FROM object_locations
		WHERE content_hash IS NOT NULL AND managed
		  AND backend_name IN (SELECT value FROM json_each(?))`, string(backendsJSON)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("failed to count scrub candidates: %w", err)
	}
	return n, nil
}

// MarkObjectScrubbed records that a copy was examined, which is what advances
// the sweep past it.
func (s *Store) MarkObjectScrubbed(ctx context.Context, key, backendName string) error {
	now := now()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE object_locations SET last_scrubbed_at = ?
		 WHERE object_key = ? AND backend_name = ?`,
		now, key, backendName,
	); err != nil {
		return fmt.Errorf("failed to mark object scrubbed: %w", err)
	}
	return nil
}

// OldestUnverifiedAge reports how stale the least recently verified copy is,
// and how many copies have never been verified at all.
func (s *Store) OldestUnverifiedAge(ctx context.Context) (time.Duration, int64, error) {
	var oldest sql.NullString
	var neverVerified int64
	err := s.db.QueryRowContext(ctx,
		`SELECT MIN(last_scrubbed_at),
		        COUNT(*) FILTER (WHERE last_scrubbed_at IS NULL)
		 FROM object_locations
		 WHERE content_hash IS NOT NULL AND managed`,
	).Scan(&oldest, &neverVerified)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to read oldest unverified age: %w", err)
	}
	if !oldest.Valid {
		return 0, neverVerified, nil
	}
	ts, err := time.Parse(time.RFC3339Nano, oldest.String)
	if err != nil {
		return 0, neverVerified, fmt.Errorf("failed to parse last_scrubbed_at %q: %w", oldest.String, err)
	}
	return time.Since(ts), neverVerified, nil
}

// GetObjectsWithoutHash returns object locations that have no stored content
// hash, ordered by creation time. Used by the backfill command.
func (s *Store) GetObjectsWithoutHash(ctx context.Context, limit, offset int) ([]core.ObjectLocation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT object_key, backend_name, size_bytes, encrypted, encryption_key,
		       key_id, plaintext_size, content_hash,
		       compression_algorithm, compression_level, compression_format_version, logical_size,
		       created_at, last_scrubbed_at
		FROM object_locations
		WHERE content_hash IS NULL AND managed
		ORDER BY created_at ASC
		LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to get objects without hash: %w", err)
	}
	defer rows.Close()

	var locs []core.ObjectLocation
	for rows.Next() {
		loc, err := scanObjectLocation(rows)
		if err != nil {
			return nil, err
		}
		locs = append(locs, loc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate objects without hash: %w", err)
	}
	return locs, nil
}

// UpdateContentHash sets the content hash for an object location.
func (s *Store) UpdateContentHash(ctx context.Context, key, backendName, hash string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE object_locations
		SET content_hash = ?
		WHERE object_key = ? AND backend_name = ?`, hash, key, backendName)
	return err
}

// -------------------------------------------------------------------------
// ROW SCANNERS
// -------------------------------------------------------------------------

// scanObjectLocation scans a full object location row including all encryption
// and integrity columns.
func scanObjectLocation(rows *sql.Rows) (core.ObjectLocation, error) {
	var (
		loc           core.ObjectLocation
		createdAt     string
		keyID         *string
		plaintextSize *int64
		contentHash   *string
		compAlgorithm *string
		compLevel     *string
		compVersion   *int64
		logicalSize   *int64
		lastScrubbed  *string
	)
	if err := rows.Scan(
		&loc.ObjectKey, &loc.BackendName, &loc.SizeBytes,
		&loc.Encrypted, &loc.EncryptionKey,
		&keyID, &plaintextSize, &contentHash,
		&compAlgorithm, &compLevel, &compVersion, &logicalSize,
		&createdAt, &lastScrubbed,
	); err != nil {
		return core.ObjectLocation{}, fmt.Errorf("failed to scan object location: %w", err)
	}
	var parseErr error
	loc.CreatedAt, parseErr = parseTime(createdAt)
	if parseErr != nil {
		return core.ObjectLocation{}, fmt.Errorf(errInvalidTimestamp, createdAt, parseErr)
	}
	if keyID != nil {
		loc.KeyID = *keyID
	}
	if plaintextSize != nil {
		loc.PlaintextSize = *plaintextSize
	}
	if contentHash != nil {
		loc.ContentHash = *contentHash
	}
	if compAlgorithm != nil {
		loc.CompressionAlgorithm = *compAlgorithm
	}
	if compLevel != nil {
		loc.CompressionLevel = *compLevel
	}
	if compVersion != nil {
		loc.CompressionFormatVersion = int(*compVersion)
	}
	if logicalSize != nil {
		loc.LogicalSize = *logicalSize
	}
	if lastScrubbed != nil {
		scrubbed, err := parseTime(*lastScrubbed)
		if err != nil {
			return core.ObjectLocation{}, fmt.Errorf(errInvalidTimestamp, *lastScrubbed, err)
		}
		loc.LastScrubbedAt = &scrubbed
	}
	return loc, nil
}
