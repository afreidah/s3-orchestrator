// -------------------------------------------------------------------------------
// Object Location Operations
//
// Author: Alex Freidah
//
// Implements the Postgres read-side bindings for object_locations - the
// canonical (object_key, backend_name) ledger of which backends hold which
// objects - that the manager and dashboard consume; the writes are core's
// shared transactions. Listing queries use the (object_key, created_at) index
// for stable pagination; pagination tokens are advanced past emitted
// CommonPrefix entries so a delimiter scan never re-emits the same prefix.
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

// ListObjectsByBackend returns up to limit managed objects on a backend after
// the cursor, smallest first. Backs the rebalance, placement, drain and purge
// scans.
func (s *Store) ListObjectsByBackend(ctx context.Context, backendName string, limit int, after core.SizeCursor) ([]core.ObjectLocation, error) {
	rows, err := s.queries.ListObjectsByBackend(ctx, db.ListObjectsByBackendParams{
		BackendName: backendName,
		AfterSize:   after.SizeBytes,
		AfterKey:    after.ObjectKey,
		RowLimit:    rowLimit(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list objects by backend: %w", err)
	}
	return toSlimObjectLocations(rows), nil
}

// ListObjectsByBackendKeyAsc returns rows for a backend in ascending
// storage_key order, starting strictly after the supplied cursor (empty for
// the first page). Reconcile merges it against a backend listing, which
// returns storage paths, so the order is by storage_key rather than object key.
func (s *Store) ListObjectsByBackendKeyAsc(ctx context.Context, backendName, afterStorageKey string, limit int) ([]core.ObjectLocation, error) {
	rows, err := s.queries.ListObjectsByBackendKeyAsc(ctx, db.ListObjectsByBackendKeyAscParams{
		BackendName: backendName,
		StorageKey:  afterStorageKey,
		Limit:       rowLimit(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to page objects by backend: %w", err)
	}
	return toSlimObjectLocations(rows), nil
}

// ListedPathStates reports what the ledger says about paths the backend
// listed, in one query for a whole listing page.
func (s *Store) ListedPathStates(ctx context.Context, backend string, paths []string) (map[string]core.PathState, error) {
	return s.direct().ListedPathStates(ctx, backend, paths)
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
		MaxKeys:    rowLimit(maxKeys + 1),
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

// CountObjectsByPrefix returns how many distinct keys live under a prefix,
// which is what answers whether a bucket still holds anything.
func (s *Store) CountObjectsByPrefix(ctx context.Context, prefix string) (int64, error) {
	n, err := s.queries.CountObjectsByPrefix(ctx, likeEscaper.Replace(prefix))
	if err != nil {
		return 0, fmt.Errorf("failed to count objects by prefix: %w", err)
	}
	return n, nil
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

// ListObjectsDelimited groups a delimiter listing in Postgres with a
// recursive-CTE loose index scan (sqlc/queries/objects.sql). Comparisons run
// under COLLATE "C", backed by idx_object_locations_key_collate_c, so groups
// come back in the byte order SQLite and S3 use. The delimiter must be
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
		Lim:        rowLimit(maxKeys + 1),
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
		AfterKey: q.After,
		Cutoff:   pgTimestamptz(q.Cutoff),
		TagCount: int32(len(q.Tags)), //nolint:gosec // G115: capped at MaxTagsPerObject by config validation
		Tags:     tags,
		MaxKeys:  rowLimit(q.Limit),
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
		MaxKeys:    rowLimit(maxKeys + 1),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list direct children: %w", err)
	}

	return core.BuildDirectoryListing(prefix,
		mapSlice(stats, func(r *db.GetDirectoryStatsRow) core.DirStat {
			return core.DirStat{Name: r.Name, IsDir: r.IsDir, FileCount: r.FileCount, TotalSize: r.TotalSize}
		}),
		mapSlice(fileRows, func(r *db.ListDirectChildrenRow) core.DirFile {
			return core.DirFile{ObjectKey: r.ObjectKey, Backends: r.BackendNames, SizeBytes: r.SizeBytes, CreatedAt: r.CreatedAt.Time}
		}),
		maxKeys), nil
}

// RecordObjectIdentity fills the identity columns a read had to ask a backend
// for. Applied to every copy of the key, while loc - the copy that was read -
// is still recorded: a per-copy value is what lets a failover change the ETag
// under a conditional request.
func (s *Store) RecordObjectIdentity(ctx context.Context, loc *core.ObjectLocation, id *core.ObjectIdentity) error {
	if id == nil {
		return nil
	}
	meta, err := core.EncodeUserMetadata(id.UserMetadata)
	if err != nil {
		return err
	}
	if err := s.queries.RecordObjectIdentity(ctx, db.RecordObjectIdentityParams{
		ObjectKey:    loc.ObjectKey,
		BackendName:  loc.BackendName,
		StorageKey:   loc.StorageKey,
		Etag:         strPtr(id.ETag),
		ContentType:  strPtr(id.ContentType),
		UserMetadata: meta,
	}); err != nil {
		return fmt.Errorf("record object identity: %w", err)
	}
	return nil
}

// UpdateCopyMetadata writes a patch to the copy loc names, or returns
// core.ErrCopyChanged when a newer write replaced it.
func (s *Store) UpdateCopyMetadata(ctx context.Context, loc *core.ObjectLocation, p *core.CopyPatch) error {
	return s.direct().UpdateCopyMetadata(ctx, loc, p)
}
