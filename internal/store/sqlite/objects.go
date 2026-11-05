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
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// READ QUERIES
// -------------------------------------------------------------------------

// GetObjectBackendsForKeys returns a map from each supplied object_key to the
// backends that hold a copy, in one query passing the keys as a JSON parameter
// via json_each. Keys with no copies are absent from the result.
func (s *Store) GetObjectBackendsForKeys(ctx context.Context, keys []string) (map[string][]string, error) {
	if len(keys) == 0 {
		return map[string][]string{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT object_key, backend_name
		FROM object_locations
		WHERE object_key IN (SELECT value FROM json_each(?))`, jsonList(keys))
	if err != nil {
		return nil, fmt.Errorf("failed to get object backends for keys: %w", err)
	}
	pairs, err := collectRows(rows, "key/backend rows", func(rows *sql.Rows) (keyBackend, error) {
		var kb keyBackend
		if err := rows.Scan(&kb.key, &kb.backend); err != nil {
			return keyBackend{}, fmt.Errorf("failed to scan key/backend pair: %w", err)
		}
		return kb, nil
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(keys))
	for _, kb := range pairs {
		out[kb.key] = append(out[kb.key], kb.backend)
	}
	return out, nil
}

// keyBackend is one (object_key, backend_name) row, grouped into the
// key -> backends map the caller asked for once the page has been read.
type keyBackend struct {
	key     string
	backend string
}

// GetAllObjectLocations returns all copies of an object, ordered by created_at
// ascending (oldest/primary first), each carrying the key's tag count. Used
// for read failover.
func (s *Store) GetAllObjectLocations(ctx context.Context, key string) ([]core.ObjectLocation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+objectColumns+`,
		       (SELECT count(*) FROM object_tags WHERE object_tags.object_key = ?1) AS tag_count
		FROM object_locations
		WHERE object_key = ?1
		ORDER BY created_at ASC`, key)
	if err != nil {
		return nil, fmt.Errorf("failed to get object locations: %w", err)
	}
	locs, err := collectRows(rows, rowsObjectLocations, scanTaggedObjectLocation)
	if err != nil {
		return nil, err
	}
	if len(locs) == 0 {
		return nil, core.ErrObjectNotFound
	}
	return locs, nil
}

// -------------------------------------------------------------------------
// LISTING
// -------------------------------------------------------------------------

// ListObjects returns one page of the keys under prefix after startAfter. A
// delimiter rolls the keys below it into CommonPrefixes; an empty one lists
// every key flat. Unmanaged rows are left out because clients cannot read
// them.
func (s *Store) ListObjects(ctx context.Context, prefix, delimiter, startAfter string, maxKeys int) (*core.ListObjectsResult, error) {
	return core.ListPage(ctx, prefix, delimiter, startAfter, maxKeys, s.listFlat, s.listDelimited)
}

// listFlat returns up to limit keys in the range, sorted by key, one row per
// key. A subquery with GROUP BY deduplicates replicated objects (equivalent to
// DISTINCT ON in PostgreSQL), and carries the limit so grouping stops at the
// page instead of running over every key in the range.
func (s *Store) listFlat(ctx context.Context, from, end string, limit int) ([]core.ObjectLocation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ol.object_key, ol.backend_name, ol.storage_key,
		       ol.object_size AS size_bytes, ol.created_at, ol.etag
		FROM object_locations ol
		INNER JOIN (
			SELECT object_key, MIN(rowid) AS min_rowid
			FROM object_locations
			WHERE object_key >= ? AND object_key < ?
			  AND managed
			GROUP BY object_key
			ORDER BY object_key
			LIMIT ?
		) dedup ON ol.rowid = dedup.min_rowid
		ORDER BY ol.object_key`, from, end, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list objects: %w", err)
	}
	return scanSlimObjectLocations(rows, true)
}

// CountObjectsByPrefix returns how many distinct keys live under a prefix,
// which is what answers whether a bucket still holds anything.
func (s *Store) CountObjectsByPrefix(ctx context.Context, prefix string) (int64, error) {
	return s.countRows(ctx, "objects by prefix", `
		SELECT COUNT(DISTINCT object_key)
		FROM object_locations
		WHERE object_key >= ? AND object_key < ?`, prefix, core.PrefixEnd(prefix))
}

// listDelimited walks a delimiter listing inside SQLite with a recursive CTE that
// seeks past each group instead of scanning through it, returning one row per
// group or leaf in BINARY collation (byte order). A step states one lower bound
// because SQLite seeks from only one, and a second would rescan from the prefix.
func (s *Store) listDelimited(ctx context.Context, prefix, delimiter, from, end string, limit int) ([]core.ObjectLocation, error) {
	// From a leaf the next seek starts just past the key. From a group it starts
	// at the common prefix with its last character raised, which a key may equal.
	// A group's row takes placeholder leaf columns, which the fold discards.
	const query = `
		WITH RECURSIVE walk(k, n) AS (
			SELECT (
				SELECT object_key FROM object_locations
				WHERE object_key >= :from AND object_key < :end
				  AND managed
				ORDER BY object_key LIMIT 1
			), 1
			UNION ALL
			SELECT (
				SELECT object_key FROM object_locations
				WHERE object_key < :end
				  AND managed
				  AND object_key >= CASE
					WHEN instr(substr(walk.k, length(:prefix) + 1), :delim) > 0 THEN
						substr(walk.k, 1, length(:prefix) + instr(substr(walk.k, length(:prefix) + 1), :delim) + length(:delim) - 2)
						|| char(unicode(substr(walk.k, length(:prefix) + instr(substr(walk.k, length(:prefix) + 1), :delim) + length(:delim) - 1, 1)) + 1)
					ELSE walk.k || char(1)
				  END
				ORDER BY object_key LIMIT 1
			), walk.n + 1
			FROM walk WHERE walk.k IS NOT NULL AND walk.n < :limit
		)
		SELECT w.k, COALESCE(ol.backend_name, ''), COALESCE(ol.storage_key, ''),
		       COALESCE(ol.object_size, 0), COALESCE(ol.created_at, '1970-01-01T00:00:00Z'), ol.etag
		FROM walk w
		LEFT JOIN object_locations ol ON ol.rowid = (
			SELECT MIN(rowid) FROM object_locations o2
			WHERE o2.object_key = w.k
			  AND o2.managed
			  AND instr(substr(w.k, length(:prefix) + 1), :delim) = 0
		)
		WHERE w.k IS NOT NULL
		ORDER BY w.k`

	rows, err := s.db.QueryContext(ctx, query,
		sql.Named("from", from),
		sql.Named("end", end),
		sql.Named("prefix", prefix),
		sql.Named("delim", delimiter),
		sql.Named("limit", limit),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list objects (delimited): %w", err)
	}
	return scanSlimObjectLocations(rows, true)
}

// ListExpiredObjects returns one row per unique key matching the query's
// filters whose created_at is older than its cutoff, up to Limit rows. Used by
// lifecycle expiration to find objects eligible for deletion.
//
// One EXISTS per tag, all required, which is what makes several tags an
// intersection. EXISTS rather than a join because the dedup subquery groups by
// object_key and a join would multiply its input row per matching tag.
func (s *Store) ListExpiredObjects(ctx context.Context, q core.ExpiredObjectsQuery) ([]core.ObjectLocation, error) {
	args := []any{core.KeysFrom(q.Prefix, q.After), core.PrefixEnd(q.Prefix), formatTime(q.Cutoff)}

	// Sorted keys keep the generated SQL and its arguments identical run to run.
	var tagFilter strings.Builder
	for _, key := range slices.Sorted(maps.Keys(q.Tags)) {
		tagFilter.WriteString(`
			  AND EXISTS (SELECT 1 FROM object_tags t
			              WHERE t.object_key = object_locations.object_key
			                AND t.tag_key = ? AND t.tag_value = ?)`)
		args = append(args, key, q.Tags[key])
	}
	args = append(args, q.Limit)

	// Subquery with GROUP BY + MIN(rowid) replaces DISTINCT ON (object_key).
	rows, err := s.db.QueryContext(ctx, `
		SELECT ol.object_key, ol.backend_name, ol.storage_key, ol.size_bytes, ol.created_at
		FROM object_locations ol
		INNER JOIN (
			SELECT object_key, MIN(rowid) AS min_rowid
			FROM object_locations
			WHERE object_key >= ? AND object_key < ?
			  AND created_at < ?`+tagFilter.String()+`
			GROUP BY object_key
			ORDER BY object_key
			LIMIT ?
		) dedup ON ol.rowid = dedup.min_rowid
		ORDER BY ol.object_key`, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list expired objects: %w", err)
	}
	return scanSlimObjectLocations(rows, false)
}

// ListObjectsByBackend returns up to limit managed objects on a backend after
// the cursor, smallest first. Backs the rebalance, placement, drain and purge
// scans.
func (s *Store) ListObjectsByBackend(ctx context.Context, backendName string, limit int, after core.SizeCursor) ([]core.ObjectLocation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT object_key, backend_name, storage_key, size_bytes, created_at
		FROM object_locations
		WHERE backend_name = ? AND managed
		  AND (size_bytes, object_key) > (?, ?)
		ORDER BY size_bytes ASC, object_key ASC
		LIMIT ?`, backendName, after.SizeBytes, after.ObjectKey, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list objects by backend: %w", err)
	}
	return scanSlimObjectLocations(rows, false)
}

// ListObjectsByBackendKeyAsc returns rows for a backend in ascending
// storage_key order, starting strictly after afterStorageKey (empty for the
// first page). Reconcile merges it against a backend listing, which returns
// storage paths, so the order is by storage_key rather than object key.
func (s *Store) ListObjectsByBackendKeyAsc(ctx context.Context, backendName, afterStorageKey string, limit int) ([]core.ObjectLocation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT object_key, backend_name, storage_key, size_bytes, created_at
		FROM object_locations
		WHERE backend_name = ? AND storage_key > ?
		ORDER BY storage_key ASC
		LIMIT ?`, backendName, afterStorageKey, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to page objects by backend: %w", err)
	}
	return scanSlimObjectLocations(rows, false)
}

// ListedPathStates reports what the ledger says about paths the backend
// listed, in one query for a whole listing page.
func (s *Store) ListedPathStates(ctx context.Context, backend string, paths []string) (map[string]core.PathState, error) {
	return s.direct().ListedPathStates(ctx, backend, paths)
}

// scanSlimObjectLocations consumes rows holding the slim (object_key,
// backend_name, storage_key, size_bytes, created_at) projection. A listing
// selects the ETag after it, because a Contents entry reports one; NULL leaves
// the entry without an identity, as for an object that has not learned it yet.
func scanSlimObjectLocations(rows *sql.Rows, withETag bool) ([]core.ObjectLocation, error) {
	return collectRows(rows, rowsObjectLocations, func(rows *sql.Rows) (core.ObjectLocation, error) {
		var (
			loc       core.ObjectLocation
			createdAt string
			etag      sql.NullString
		)
		dest := []any{&loc.ObjectKey, &loc.BackendName, &loc.StorageKey, &loc.SizeBytes, &createdAt}
		if withETag {
			dest = append(dest, &etag)
		}
		if err := rows.Scan(dest...); err != nil {
			return core.ObjectLocation{}, fmt.Errorf("failed to scan object location: %w", err)
		}
		var err error
		if loc.CreatedAt, err = parseTime(createdAt); err != nil {
			return core.ObjectLocation{}, err
		}
		loc.Identity = core.ListedIdentity(etag.String)
		return loc, nil
	})
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
	return s.withTx(ctx, func(a *sqliteTxAdapter) error {
		stmts := []string{
			`DELETE FROM cleanup_queue WHERE backend_name = ?`,
			`DELETE FROM cleanup_dlq WHERE backend_name = ?`,
			`DELETE FROM pending_objects WHERE backend_name = ?`,
			`DELETE FROM multipart_parts WHERE upload_id IN (SELECT upload_id FROM multipart_uploads WHERE backend_name = ?)`,
			`DELETE FROM multipart_uploads WHERE backend_name = ?`,
			`DELETE FROM object_locations WHERE backend_name = ?`,
			`DELETE FROM backend_usage WHERE backend_name = ?`,
			`DELETE FROM backend_request_usage WHERE backend_name = ?`,
			`DELETE FROM backend_quotas WHERE backend_name = ?`,
		}
		for _, stmt := range stmts {
			if err := a.exec(ctx, fmt.Sprintf("failed to execute %q", stmt), stmt, backendName); err != nil {
				return err
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
func (s *Store) GetLeastRecentlyScrubbedObjects(ctx context.Context, limit int, backends []string, scrubbedBefore time.Time) ([]core.ObjectLocation, error) {
	if len(backends) == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+objectColumns+`
		FROM object_locations
		WHERE content_hash IS NOT NULL AND managed
		  AND backend_name IN (SELECT value FROM json_each(?))
		  AND COALESCE(last_scrubbed_at, created_at) < ?
		ORDER BY COALESCE(last_scrubbed_at, created_at) ASC, object_key ASC
		LIMIT ?`, jsonList(backends), formatTime(scrubbedBefore), limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get least recently scrubbed objects: %w", err)
	}
	return collectRows(rows, "least recently scrubbed objects", scanObjectLocation)
}

// CountScrubCandidatesOnBackends reports how many scrubbable copies live on the
// named backends, so a cycle can say how much of the queue it declined to read.
func (s *Store) CountScrubCandidatesOnBackends(ctx context.Context, backends []string, scrubbedBefore time.Time) (int64, error) {
	if len(backends) == 0 {
		return 0, nil
	}
	return s.countRows(ctx, "scrub candidates", `
		SELECT count(*)
		FROM object_locations
		WHERE content_hash IS NOT NULL AND managed
		  AND backend_name IN (SELECT value FROM json_each(?))
		  AND COALESCE(last_scrubbed_at, created_at) < ?`, jsonList(backends), formatTime(scrubbedBefore))
}

// ListCopiesByForm returns up to limit copies after the cursor that the filter
// selects, in key order: every flag of With and none of Without, narrowed by an
// optional backend and key ID. Paged by cursor, since a pass usually takes the
// copies it acts on out of its own filter and an offset would skip rows.
//
// Candidates adds the compression candidate test: a copy already measured at
// the configured level as unable to reach the ratio is excluded, so it is not
// encoded again to reach the same verdict. A zero object_size makes the
// division NULL and excludes the copy, matching WorthStoring.
func (s *Store) ListCopiesByForm(ctx context.Context, f *core.FormFilter, limit int, after core.Cursor) ([]core.ObjectLocation, error) {
	var t core.CompressionThresholds
	if f.Candidates != nil {
		t = *f.Candidates
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+objectColumns+`
		FROM object_locations
		WHERE (form & ?) = ? AND (form & ?) = 0
		  AND (NOT ? OR managed)
		  AND (? = '' OR backend_name = ?)
		  AND (? = '' OR key_id = ?)
		  AND (? = '' OR key_id IS NOT ?)
		  AND object_size >= ?
		  AND (? = ''
		       OR compression_probe_size IS NULL
		       OR compression_probe_level IS NOT ?
		       OR CAST(compression_probe_size AS REAL) / NULLIF(object_size, 0) <= ?)
		  AND (object_key, backend_name) > (?, ?)
		ORDER BY object_key, backend_name
		LIMIT ?`,
		f.With, f.With, f.Without,
		f.ManagedOnly,
		f.Backend, f.Backend,
		f.KeyID, f.KeyID,
		f.ExceptKeyID, f.ExceptKeyID,
		t.MinSize,
		t.Level, t.Level, t.MinRatio,
		after.ObjectKey, after.BackendName,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list copies by form: %w", err)
	}
	return collectRows(rows, "copies by form", scanObjectLocation)
}

// CountCopiesByForm reports how many copies carry every flag of with and none
// of without.
func (s *Store) CountCopiesByForm(ctx context.Context, with, without core.StoredFlags) (int64, error) {
	return s.countRows(ctx, "copies by form",
		`SELECT count(*) FROM object_locations WHERE (form & ?) = ? AND (form & ?) = 0`,
		with, with, without)
}

// -------------------------------------------------------------------------
// ROW SCANNERS
// -------------------------------------------------------------------------

// scanObjectLocation scans a whole object_locations row selected as
// objectColumns.
func scanObjectLocation(rows *sql.Rows) (core.ObjectLocation, error) {
	var loc core.ObjectLocation
	if err := scanObjectRow(rows, &loc); err != nil {
		return core.ObjectLocation{}, err
	}
	return loc, nil
}

// scanTaggedObjectLocation scans a whole row followed by the key's tag count,
// which only the read path's query selects.
func scanTaggedObjectLocation(rows *sql.Rows) (core.ObjectLocation, error) {
	var loc core.ObjectLocation
	if err := scanObjectRow(rows, &loc, &loc.TagCount); err != nil {
		return core.ObjectLocation{}, err
	}
	return loc, nil
}

// storedFormColumns holds the nullable StoredForm columns object_locations and
// pending_objects share, so their order and NULL handling are stated once.
type storedFormColumns struct {
	keyID         *string
	plaintextSize *int64
	contentHash   *string
	compAlgorithm *string
	compLevel     *string
	compVersion   *int64
	logicalSize   *int64
}

// scanDest returns the scan targets for encrypted through logical_size, in
// the order every query selects them.
func (c *storedFormColumns) scanDest(f *core.StoredForm) []any {
	return []any{&f.Encrypted, &f.EncryptionKey, &c.keyID, &c.plaintextSize, &c.contentHash,
		&c.compAlgorithm, &c.compLevel, &c.compVersion, &c.logicalSize}
}

// apply dereferences the scanned columns onto f, leaving the zero value for
// each NULL.
func (c *storedFormColumns) apply(f *core.StoredForm) {
	f.KeyID = deref(c.keyID)
	f.PlaintextSize = deref(c.plaintextSize)
	f.ContentHash = deref(c.contentHash)
	f.CompressionAlgorithm = deref(c.compAlgorithm)
	f.CompressionLevel = deref(c.compLevel)
	f.CompressionFormatVersion = int(deref(c.compVersion))
	f.LogicalSize = deref(c.logicalSize)
}

// scannedObjectColumns holds the nullable columns of an object_locations row
// between the scan and the conversion, so the column order and the NULL
// handling are each stated once.
type scannedObjectColumns struct {
	storedFormColumns
	createdAt    string
	probeSize    *int64
	probeLevel   *string
	lastScrubbed *string
	etag         sql.NullString
	contentType  sql.NullString
	userMetadata sql.NullString
	managed      bool
}

// objectColumns is every column of an object_locations row a copy is built
// from, in the order scanObjectRow reads them.
const objectColumns = `object_key, backend_name, storage_key, size_bytes, encrypted, encryption_key,
	key_id, plaintext_size, content_hash,
	compression_algorithm, compression_level, compression_format_version, logical_size,
	compression_probe_size, compression_probe_level,
	created_at, last_scrubbed_at, etag, content_type, user_metadata, managed`

// scanObjectRow reads one row selected as objectColumns into loc, then any
// extra columns the query selects after them.
func scanObjectRow(rows *sql.Rows, loc *core.ObjectLocation, extra ...any) error {
	var c scannedObjectColumns
	dest := []any{&loc.ObjectKey, &loc.BackendName, &loc.StorageKey, &loc.SizeBytes}
	dest = append(dest, c.scanDest(&loc.StoredForm)...)
	dest = append(dest, &c.probeSize, &c.probeLevel, &c.createdAt, &c.lastScrubbed,
		&c.etag, &c.contentType, &c.userMetadata, &c.managed)
	if err := rows.Scan(append(dest, extra...)...); err != nil {
		return fmt.Errorf("failed to scan object location: %w", err)
	}
	return c.apply(loc)
}

// apply dereferences the scanned columns onto loc. A metadata decode failure
// leaves the identity nil, costing a backend round trip rather than the read.
func (c *scannedObjectColumns) apply(loc *core.ObjectLocation) error {
	var err error
	if loc.CreatedAt, err = parseTime(c.createdAt); err != nil {
		return err
	}
	c.storedFormColumns.apply(&loc.StoredForm)
	loc.CompressionProbeSize = deref(c.probeSize)
	loc.CompressionProbeLevel = deref(c.probeLevel)
	loc.Identity = identityFromColumns(c.etag, c.contentType, c.userMetadata)
	loc.Unmanaged = !c.managed
	if c.lastScrubbed != nil {
		scrubbed, err := parseTime(*c.lastScrubbed)
		if err != nil {
			return err
		}
		loc.LastScrubbedAt = &scrubbed
	}
	return nil
}

// RecordObjectIdentity fills the identity columns a read had to ask a backend
// for. Applied to every copy of the key: a per-copy value is what lets a
// failover change the ETag under a conditional request. Columns already set
// are left alone, so what a write computed over the client's own bytes
// outranks what a backend reports about the bytes as stored.
//
// Written only while loc, the copy that was read, is still recorded. An
// overwrite replaces every copy of the key, so while that one survives the
// others belong to the same write.
func (s *Store) RecordObjectIdentity(ctx context.Context, loc *core.ObjectLocation, id *core.ObjectIdentity) error {
	if id == nil {
		return nil
	}
	etag, contentType, userMetadata := identityColumns(id)
	return s.exec(ctx, "record object identity", `
		UPDATE object_locations
		SET etag          = COALESCE(etag, ?),
		    content_type  = COALESCE(content_type, ?),
		    user_metadata = COALESCE(user_metadata, ?)
		WHERE object_key = ?
		  AND EXISTS (SELECT 1 FROM object_locations AS read_copy
		              WHERE read_copy.object_key = ?
		                AND read_copy.backend_name = ?
		                AND read_copy.storage_key = ?)`,
		etag, contentType, userMetadata,
		loc.ObjectKey, loc.ObjectKey, loc.BackendName, loc.StorageKey)
}

// UpdateCopyMetadata writes a patch to the copy loc names, or returns
// core.ErrCopyChanged when a newer write replaced it.
func (s *Store) UpdateCopyMetadata(ctx context.Context, loc *core.ObjectLocation, p *core.CopyPatch) error {
	return s.direct().UpdateCopyMetadata(ctx, loc, p)
}
