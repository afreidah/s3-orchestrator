// -------------------------------------------------------------------------------
// Object Location Operations
//
// Author: Alex Freidah
//
// Implements the Postgres read-side bindings for object_locations - the
// canonical (object_key, backend_name) ledger of which backends hold which
// objects - that the manager and dashboard consume; the writes are core's
// shared transactions. Listing queries scan a byte range of the
// (object_key, created_at) index for stable pagination; core builds the range
// and folds the rows into pages.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"unicode/utf8"

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

// ListObjects returns one page of the keys under prefix after startAfter. A
// delimiter rolls the keys below it into CommonPrefixes; an empty one lists
// every key flat.
func (s *Store) ListObjects(ctx context.Context, prefix, delimiter, startAfter string, maxKeys int) (*core.ListObjectsResult, error) {
	return core.ListPage(ctx, prefix, delimiter, startAfter, maxKeys, s.listFlat, s.listDelimited)
}

// listFlat returns up to limit keys in the range, sorted by key, one row per
// key. The rows carry no storage path, which no listing reads.
func (s *Store) listFlat(ctx context.Context, from, end string, limit int) ([]core.ObjectLocation, error) {
	rows, err := s.queries.ListObjectsByPrefix(ctx, db.ListObjectsByPrefixParams{
		FromKey: from,
		EndKey:  end,
		MaxKeys: rowLimit(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list objects: %w", err)
	}

	objects := toSlimObjectLocations(rows)
	for i := range rows {
		objects[i].Identity = core.ListedIdentity(derefStr(rows[i].Etag))
	}
	return objects, nil
}

// CountObjectsByPrefix returns how many distinct keys live under a prefix,
// which is what answers whether a bucket still holds anything.
func (s *Store) CountObjectsByPrefix(ctx context.Context, prefix string) (int64, error) {
	n, err := s.queries.CountObjectsByPrefix(ctx, db.CountObjectsByPrefixParams{
		FromKey: prefix,
		EndKey:  core.PrefixEnd(prefix),
	})
	if err != nil {
		return 0, fmt.Errorf("failed to count objects by prefix: %w", err)
	}
	return n, nil
}

// listDelimited walks a delimiter listing in Postgres with a recursive-CTE loose
// index scan (sqlc/queries/objects.sql), returning one row per group or leaf in
// the byte order SQLite and S3 use. Comparisons run under COLLATE "C", backed by
// idx_object_locations_listing.
func (s *Store) listDelimited(ctx context.Context, prefix, delimiter, from, end string, limit int) ([]core.ObjectLocation, error) {
	rows, err := s.queries.ListObjectsDelimited(ctx, db.ListObjectsDelimitedParams{
		FromKey: from,
		EndKey:  end,
		Prefix:  prefix,
		Delim:   delimiter,
		Lim:     rowLimit(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list objects (delimited): %w", err)
	}
	return mapSlice(rows, func(r *db.ListObjectsDelimitedRow) core.ObjectLocation {
		return core.ObjectLocation{
			ObjectKey:   r.ObjectKey,
			BackendName: r.BackendName,
			SizeBytes:   r.SizeBytes,
			CreatedAt:   r.CreatedAt.Time,
			Identity:    core.ListedIdentity(r.Etag),
		}
	}), nil
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
		FromKey:  core.KeysFrom(q.Prefix, q.After),
		EndKey:   core.PrefixEnd(q.Prefix),
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
	end := core.PrefixEnd(prefix)

	// Get aggregate stats for all immediate children (dirs + files). NameStart
	// counts characters, the unit substr cuts in.
	stats, err := s.queries.GetDirectoryStats(ctx, db.GetDirectoryStatsParams{
		FromKey:   prefix,
		EndKey:    end,
		NameStart: int32(utf8.RuneCountInString(prefix) + 1), //nolint:gosec // G115: directory prefix length is small and bounded
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get directory stats: %w", err)
	}

	// Get per-file detail for direct file children (paginated).
	fileRows, err := s.queries.ListDirectChildren(ctx, db.ListDirectChildrenParams{
		FromKey: core.KeysFrom(prefix, startAfter),
		EndKey:  end,
		Prefix:  prefix,
		MaxKeys: rowLimit(maxKeys + 1),
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
	p := db.RecordObjectIdentityParams{ObjectKey: loc.ObjectKey, BackendName: loc.BackendName, StorageKey: loc.StorageKey}
	p.Etag, p.ContentType, p.UserMetadata = identityColumns(id)
	if err := s.queries.RecordObjectIdentity(ctx, p); err != nil {
		return fmt.Errorf("record object identity: %w", err)
	}
	return nil
}

// UpdateCopyMetadata writes a patch to the copy loc names, or returns
// core.ErrCopyChanged when a newer write replaced it.
func (s *Store) UpdateCopyMetadata(ctx context.Context, loc *core.ObjectLocation, p *core.CopyPatch) error {
	return s.direct().UpdateCopyMetadata(ctx, loc, p)
}
