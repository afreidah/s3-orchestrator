// -------------------------------------------------------------------------------
// Object Location Operations
//
// Author: Alex Freidah
//
// Implements the Postgres engine bindings for object_locations - the
// canonical (object_key, backend_name) ledger of which backends hold
// which objects. Wraps the engine-agnostic core orchestration
// (RecordObject, DeleteObject, MoveObjectLocation, ImportObject) and
// exposes the read-side queries the manager and dashboard consume.
// Listing queries use the (object_key, created_at) index for stable
// pagination; pagination tokens are advanced past emitted CommonPrefix
// entries so a delimiter scan never re-emits the same prefix.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// -------------------------------------------------------------------------
// OBJECT LOCATION OPERATIONS
// -------------------------------------------------------------------------

// ListObjectsByBackend returns objects stored on a specific backend, ordered by
// size ascending (smallest first). Used by the rebalancer to find movable objects.
func (s *Store) ListObjectsByBackend(ctx context.Context, backendName string, limit int) ([]core.ObjectLocation, error) {
	rows, err := s.queries.ListObjectsByBackend(ctx, db.ListObjectsByBackendParams{
		BackendName: backendName,
		Limit:       int32(limit), //nolint:gosec // G115: limit is a small caller-controlled batch size
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list objects by backend: %w", err)
	}
	return toSlimObjectLocations(rows), nil
}

// ListObjectsByBackendKeyAsc returns rows for a backend in ascending
// object_key order, starting strictly after the supplied cursor. The empty
// string returns the first page. Used by ReconcileBackend to drive a
// bounded-memory sorted-merge join against an S3 ListObjects walk; both
// sides are in lex order so the merge is O(n) memory bounded by limit.
func (s *Store) ListObjectsByBackendKeyAsc(ctx context.Context, backendName, afterKey string, limit int) ([]core.ObjectLocation, error) {
	rows, err := s.queries.ListObjectsByBackendKeyAsc(ctx, db.ListObjectsByBackendKeyAscParams{
		BackendName: backendName,
		ObjectKey:   afterKey,
		Limit:       int32(limit), //nolint:gosec // G115: limit is a small caller-controlled batch size
	})
	if err != nil {
		return nil, fmt.Errorf("failed to page objects by backend: %w", err)
	}
	return toSlimObjectLocations(rows), nil
}

// ListObjects returns objects matching the given prefix, sorted by key.
// Supports pagination via startAfter and maxKeys. Returns one extra row to
// detect truncation.
func (s *Store) ListObjects(ctx context.Context, prefix, startAfter string, maxKeys int) (*core.ListObjectsResult, error) {
	if maxKeys <= 0 {
		maxKeys = 1000
	}

	escapedPrefix := likeEscaper.Replace(prefix)

	// Fetch one extra to detect truncation
	rows, err := s.queries.ListObjectsByPrefix(ctx, db.ListObjectsByPrefixParams{
		Prefix:     escapedPrefix,
		StartAfter: startAfter,
		MaxKeys:    int32(maxKeys + 1),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list objects: %w", err)
	}

	objects := toSlimObjectLocations(rows)
	for i := range rows {
		objects[i].Identity = listedIdentity(rows[i].Etag)
	}
	return core.BuildListPage(objects, maxKeys), nil
}

// listedIdentity builds the identity a listing row carries. A listing selects
// the ETag and nothing else of the identity, which is all a Contents entry
// reports; NULL means the object has not learned one yet and the entry carries
// no ETag rather than a wrong one.
func listedIdentity(etag *string) *core.ObjectIdentity {
	if etag == nil || *etag == "" {
		return nil
	}
	return &core.ObjectIdentity{ETag: *etag}
}

// ListObjectsDelimited groups a delimiter listing in Postgres through the
// sqlc-generated recursive-CTE query (see sqlc/queries/objects.sql). Every
// object_key comparison and ORDER BY runs under COLLATE "C", backed by the
// idx_object_locations_key_collate_c index, so the loose index scan seeks
// group-to-group in byte order that matches SQLite and S3. The generated rows
// arrive flattened (placeholder values on the branch is_prefix does not select);
// this maps them back into CommonPrefix and leaf entries. The delimiter must be
// non-empty; callers route empty-delimiter lists to ListObjects.
func (s *Store) ListObjectsDelimited(ctx context.Context, prefix, delimiter, startAfter string, maxKeys int) (*core.ListDelimitedResult, error) {
	if maxKeys <= 0 {
		maxKeys = 1000
	}
	escapedPrefix := likeEscaper.Replace(prefix)

	rows, err := s.queries.ListObjectsDelimited(ctx, db.ListObjectsDelimitedParams{
		Escprefix:  escapedPrefix,
		Prefix:     prefix,
		Delim:      delimiter,
		StartAfter: startAfter,
		Lim:        int32(maxKeys + 1),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list objects (delimited): %w", err)
	}

	entries := make([]core.DelimitedEntry, len(rows))
	for i := range rows {
		r := rows[i]
		e := core.DelimitedEntry{IsPrefix: r.IsPrefix, CommonPrefix: r.CommonPrefix, SkipBound: r.SkipBound}
		if !r.IsPrefix {
			e.Leaf = core.ObjectLocation{
				ObjectKey:   r.ObjectKey,
				BackendName: r.BackendName,
				SizeBytes:   r.SizeBytes,
				CreatedAt:   r.CreatedAt.Time,
				Identity:    listedIdentity(&r.Etag),
			}
		}
		entries[i] = e
	}
	return core.BuildDelimitedPage(entries, maxKeys), nil
}

// ListExpiredObjects returns one row per unique key matching the query's
// filters whose created_at is older than its cutoff, up to Limit rows. Used by
// lifecycle expiration to find objects eligible for deletion.
//
// Tags travel as a JSON object rather than parallel arrays because sqlc's
// catalog has no two-argument unnest, and pairing a key to its own value is
// what makes the filter an intersection rather than a cross product.
func (s *Store) ListExpiredObjects(ctx context.Context, q core.ExpiredObjectsQuery) ([]core.ObjectLocation, error) {
	// Encoded as {} rather than null when unset: SQL does not promise to
	// short-circuit the OR that guards the subquery, and jsonb_each_text
	// errors on a JSON null where it yields no rows for an empty object.
	filter := q.Tags
	if filter == nil {
		filter = map[string]string{}
	}
	tags, err := json.Marshal(filter)
	if err != nil {
		return nil, fmt.Errorf("failed to encode lifecycle tag filter: %w", err)
	}
	rows, err := s.queries.ListExpiredObjects(ctx, db.ListExpiredObjectsParams{
		Prefix:   likeEscaper.Replace(q.Prefix),
		Cutoff:   pgTimestamptz(q.Cutoff),
		TagCount: int32(len(q.Tags)), //nolint:gosec // G115: capped at MaxTagsPerObject by config validation
		Tags:     tags,
		MaxKeys:  int32(q.Limit), //nolint:gosec // G115: limit is a small caller-controlled batch size
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list expired objects: %w", err)
	}
	return toSlimObjectLocations(rows), nil
}

// -------------------------------------------------------------------------
// DIRECTORY LISTING (DASHBOARD)
// -------------------------------------------------------------------------

// ListDirectoryChildren returns the immediate children of a directory prefix
// with aggregate stats for subdirectories. Files include backend and creation
// time. Prefix must end with "/" (or be "" for root).
func (s *Store) ListDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*core.DirectoryListResult, error) {
	if maxKeys <= 0 {
		maxKeys = 200
	}

	escapedPrefix := likeEscaper.Replace(prefix)

	// Get aggregate stats for all immediate children (dirs + files). NameStart
	// uses the unescaped prefix length so likeEscaper's '\_' insertions don't
	// shift the child-name substring offset (see GetDirectoryStats).
	stats, err := s.queries.GetDirectoryStats(ctx, db.GetDirectoryStatsParams{
		Prefix:    escapedPrefix,
		NameStart: int32(len(prefix) + 1), //nolint:gosec // G115: directory prefix length is small and bounded
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get directory stats: %w", err)
	}

	// Get per-file detail for direct file children (paginated).
	fileRows, err := s.queries.ListDirectChildren(ctx, db.ListDirectChildrenParams{
		Prefix:     escapedPrefix,
		StartAfter: startAfter,
		MaxKeys:    int32(maxKeys + 1),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list direct children: %w", err)
	}

	// Build a lookup of file details by relative name.
	type fileDetail struct {
		Backends  []string
		SizeBytes int64
		CreatedAt string
	}
	fileLookup := make(map[string]fileDetail, len(fileRows))
	hasMore := len(fileRows) > maxKeys
	if hasMore {
		fileRows = fileRows[:maxKeys]
	}
	for _, row := range fileRows {
		relName := row.ObjectKey[len(prefix):]
		fileLookup[relName] = fileDetail{
			Backends:  row.BackendNames,
			SizeBytes: row.SizeBytes,
			CreatedAt: row.CreatedAt.Time.Format("2006-01-02 15:04"),
		}
	}

	result := &core.DirectoryListResult{
		Entries: make([]core.DirEntry, 0, len(stats)),
	}

	for _, s := range stats {
		entry := core.DirEntry{
			Name:      prefix + s.Name,
			IsDir:     s.IsDir,
			FileCount: s.FileCount,
			TotalSize: s.TotalSize,
		}
		if !s.IsDir {
			detail, ok := fileLookup[s.Name]
			if !ok {
				// File is outside the current page.
				continue
			}
			entry.Backends = detail.Backends
			entry.CreatedAt = detail.CreatedAt
			// Per-file row reports the logical object size, not the
			// replica-sum returned by GetDirectoryStats; SizeBytes is
			// a single replica's size and matches what the user
			// uploaded.
			entry.TotalSize = detail.SizeBytes
		}
		result.Entries = append(result.Entries, entry)
	}

	if hasMore {
		lastKey := fileRows[len(fileRows)-1].ObjectKey
		result.HasMore = true
		result.NextCursor = lastKey
	}

	return result, nil
}

// RecordObjectIdentity fills the identity columns a read had to ask a backend
// for. Applied to every copy of the key: a per-copy value is what lets a
// failover change the ETag under a conditional request.
func (s *Store) RecordObjectIdentity(ctx context.Context, key string, id *core.ObjectIdentity) error {
	if id == nil {
		return nil
	}
	meta, err := core.EncodeUserMetadata(id.UserMetadata)
	if err != nil {
		return err
	}
	if err := s.queries.RecordObjectIdentity(ctx, db.RecordObjectIdentityParams{
		ObjectKey:    key,
		Etag:         strPtr(id.ETag),
		ContentType:  strPtr(id.ContentType),
		UserMetadata: meta,
	}); err != nil {
		return fmt.Errorf("record object identity: %w", err)
	}
	return nil
}
