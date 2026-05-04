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
	"fmt"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// -------------------------------------------------------------------------
// OBJECT LOCATION OPERATIONS
// -------------------------------------------------------------------------

// RecordObject atomically inserts or updates an object location, handling
// overwrites by returning displaced copies for cleanup. Delegates to
// core.RecordObject which composes lock, displacement, insert, and
// quota update against the postgres TxAdapter.
func (s *Store) RecordObject(ctx context.Context, key, backend string, size int64, enc *core.EncryptionMeta) ([]core.DeletedCopy, error) {
	return core.RecordObject(ctx, s, key, backend, size, enc)
}

// RecordObjectAndClearPending performs the same atomic commit as
// RecordObject and additionally deletes the matching pending_objects
// intent inside the same transaction. Delegates to core.
func (s *Store) RecordObjectAndClearPending(ctx context.Context, key, backend string, size int64, enc *core.EncryptionMeta, intentID string) ([]core.DeletedCopy, error) {
	return core.RecordObjectAndClearPending(ctx, s, key, backend, size, enc, intentID)
}

// insertParamsFromEnc builds InsertObjectLocationParams, attaching
// encryption and content-hash metadata when provided.
func insertParamsFromEnc(key, backend string, size int64, enc *core.EncryptionMeta) db.InsertObjectLocationParams {
	params := db.InsertObjectLocationParams{
		ObjectKey:   key,
		BackendName: backend,
		SizeBytes:   size,
	}
	if enc == nil {
		return params
	}
	if enc.Encrypted {
		params.Encrypted = true
		params.EncryptionKey = enc.EncryptionKey
		params.KeyID = &enc.KeyID
		params.PlaintextSize = &enc.PlaintextSize
	}
	if enc.ContentHash != "" {
		params.ContentHash = &enc.ContentHash
	}
	return params
}

// DeleteObject removes all copies of an object and decrements their
// quotas. Returns all deleted copies, or ErrObjectNotFound if the
// object doesn't exist. Delegates to core.DeleteObject.
func (s *Store) DeleteObject(ctx context.Context, key string) ([]core.DeletedCopy, error) {
	return core.DeleteObject(ctx, s, key)
}

// DeleteObjectsBatch delegates to core.DeleteObjectsBatch which
// removes every supplied key in one transaction and returns per-key
// displaced copies for backend cleanup.
func (s *Store) DeleteObjectsBatch(ctx context.Context, keys []string) (map[string][]core.DeletedCopy, error) {
	return core.DeleteObjectsBatch(ctx, s, keys)
}

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

// MoveObjectLocation atomically moves a copy of an object from one
// backend to another. Returns (0, nil) if the source copy is gone or
// the target already has a copy. Delegates to core.MoveObjectLocation.
func (s *Store) MoveObjectLocation(ctx context.Context, key, fromBackend, toBackend string) (int64, error) {
	return core.MoveObjectLocation(ctx, s, key, fromBackend, toBackend)
}

// ListObjects returns objects matching the given prefix, sorted by key.
// Supports pagination via startAfter and maxKeys. Returns one extra row to
// detect truncation.
func (s *Store) ListObjects(ctx context.Context, prefix, startAfter string, maxKeys int) (*core.ListObjectsResult, error) {
	if maxKeys <= 0 {
		maxKeys = 1000
	}

	// --- Escape LIKE wildcards in prefix ---
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

	result := &core.ListObjectsResult{}
	if len(objects) > maxKeys {
		result.IsTruncated = true
		result.NextContinuationToken = objects[maxKeys-1].ObjectKey
		result.Objects = objects[:maxKeys]
	} else {
		result.Objects = objects
	}

	return result, nil
}

// ListExpiredObjects returns one row per unique key matching the given prefix
// whose created_at is older than cutoff, up to limit rows. Used by lifecycle
// expiration to find objects eligible for deletion.
func (s *Store) ListExpiredObjects(ctx context.Context, prefix string, cutoff time.Time, limit int) ([]core.ObjectLocation, error) {
	escapedPrefix := likeEscaper.Replace(prefix)
	rows, err := s.queries.ListExpiredObjects(ctx, db.ListExpiredObjectsParams{
		Prefix:  escapedPrefix,
		Cutoff:  pgTimestamptz(cutoff),
		MaxKeys: int32(limit), //nolint:gosec // G115: limit is a small caller-controlled batch size
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

	// Get aggregate stats for all immediate children (dirs + files).
	stats, err := s.queries.GetDirectoryStats(ctx, escapedPrefix)
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

// -------------------------------------------------------------------------
// SYNC OPERATIONS
// -------------------------------------------------------------------------

// ImportObject records a pre-existing object in the database without
// overwriting. Returns true if the object was imported, false if it
// already existed for this backend. Delegates to core.ImportObject.
func (s *Store) ImportObject(ctx context.Context, key, backend string, size int64) (bool, error) {
	return core.ImportObject(ctx, s, key, backend, size)
}
