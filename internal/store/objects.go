// -------------------------------------------------------------------------------
// Object Location Operations
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	db "github.com/afreidah/s3-orchestrator/internal/store/sqlc"
)

// -------------------------------------------------------------------------
// OBJECT LOCATION OPERATIONS
// -------------------------------------------------------------------------

// RecordObject records an object's location and updates the backend quota.
// On overwrite, all existing copies (including replicas) are removed and their
// quotas decremented before inserting the new primary copy.
// EncryptionMeta holds encryption metadata to store alongside an object
// location. Zero value represents an unencrypted object.
type EncryptionMeta struct {
	Encrypted     bool
	EncryptionKey []byte
	KeyID         string
	PlaintextSize int64
	ContentHash   string // SHA-256 hex digest of plaintext (empty = not computed)
}

// RecordObject atomically inserts or updates an object location, handling
// overwrites by returning displaced copies for cleanup.
func (s *Store) RecordObject(ctx context.Context, key, backend string, size int64, enc *EncryptionMeta) ([]DeletedCopy, error) {
	return withTxVal(s, ctx, func(qtx *db.Queries) ([]DeletedCopy, error) {
		if err := qtx.LockObjectKeyForWrite(ctx, key); err != nil {
			return nil, fmt.Errorf("failed to acquire object key lock: %w", err)
		}
		existing, err := qtx.GetExistingCopiesForUpdate(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("failed to query existing copies: %w", err)
		}
		displaced, err := clearExistingCopies(ctx, qtx, key, backend, existing)
		if err != nil {
			return nil, err
		}
		if err := qtx.InsertObjectLocation(ctx, insertParamsFromEnc(key, backend, size, enc)); err != nil {
			return nil, fmt.Errorf("failed to insert object location: %w", err)
		}
		if err := incrementBackendQuota(ctx, qtx, backend, size); err != nil {
			return nil, err
		}
		return displaced, nil
	})
}

// clearExistingCopies deletes any prior copies of the object and decrements
// their backend quotas. Copies on backends other than the new target are
// returned as DeletedCopy entries so the caller can enqueue them for
// physical orphan cleanup.
func clearExistingCopies(ctx context.Context, qtx *db.Queries, key, newBackend string, existing []db.GetExistingCopiesForUpdateRow) ([]DeletedCopy, error) {
	if len(existing) == 0 {
		return nil, nil
	}
	if err := qtx.DeleteObjectCopies(ctx, key); err != nil {
		return nil, fmt.Errorf("failed to delete existing copies: %w", err)
	}
	var displaced []DeletedCopy
	for _, ec := range existing {
		if err := qtx.DecrementQuota(ctx, db.DecrementQuotaParams{
			Amount:      ec.SizeBytes,
			BackendName: ec.BackendName,
		}); err != nil {
			return nil, fmt.Errorf("failed to decrement quota for %s: %w", ec.BackendName, err)
		}
		// The new PutObject overwrites in place on newBackend; stale copies
		// on every other backend become orphans requiring cleanup.
		if ec.BackendName != newBackend {
			displaced = append(displaced, DeletedCopy{BackendName: ec.BackendName, SizeBytes: ec.SizeBytes})
		}
	}
	return displaced, nil
}

// insertParamsFromEnc builds InsertObjectLocationParams, attaching
// encryption and content-hash metadata when provided.
func insertParamsFromEnc(key, backend string, size int64, enc *EncryptionMeta) db.InsertObjectLocationParams {
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

// incrementBackendQuota credits `size` bytes to `backend`'s quota and
// returns ErrNoSpaceAvailable when the row reports zero rows updated
// (meaning the quota would be exceeded).
func incrementBackendQuota(ctx context.Context, qtx *db.Queries, backend string, size int64) error {
	n, err := qtx.IncrementQuota(ctx, db.IncrementQuotaParams{
		Amount:      size,
		BackendName: backend,
	})
	if err != nil {
		return fmt.Errorf("failed to update quota: %w", err)
	}
	if n == 0 {
		return ErrNoSpaceAvailable
	}
	return nil
}

// DeleteObject removes all copies of an object and decrements their quotas.
// Returns all deleted copies, or ErrObjectNotFound if the object doesn't exist.
func (s *Store) DeleteObject(ctx context.Context, key string) ([]DeletedCopy, error) {
	return withTxVal(s, ctx, func(qtx *db.Queries) ([]DeletedCopy, error) {
		// --- Get all copies ---
		existing, err := qtx.GetExistingCopiesForUpdate(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("failed to get object locations: %w", err)
		}

		if len(existing) == 0 {
			return nil, ErrObjectNotFound
		}

		// --- Delete all location records ---
		if err := qtx.DeleteObjectCopies(ctx, key); err != nil {
			return nil, fmt.Errorf("failed to delete object locations: %w", err)
		}

		// --- Decrement quota for each backend ---
		copies := make([]DeletedCopy, len(existing))
		for i, ec := range existing {
			copies[i] = DeletedCopy{
				BackendName: ec.BackendName,
				SizeBytes:   ec.SizeBytes,
			}
			if err := qtx.DecrementQuota(ctx, db.DecrementQuotaParams{
				Amount:      ec.SizeBytes,
				BackendName: ec.BackendName,
			}); err != nil {
				return nil, fmt.Errorf("failed to decrement quota for %s: %w", ec.BackendName, err)
			}
		}

		return copies, nil
	})
}

// ListObjectsByBackend returns objects stored on a specific backend, ordered by
// size ascending (smallest first). Used by the rebalancer to find movable objects.
func (s *Store) ListObjectsByBackend(ctx context.Context, backendName string, limit int) ([]ObjectLocation, error) {
	rows, err := s.queries.ListObjectsByBackend(ctx, db.ListObjectsByBackendParams{
		BackendName: backendName,
		Limit:       int32(limit), //nolint:gosec // G115: limit is a small caller-controlled batch size
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list objects by backend: %w", err)
	}
	return toSlimObjectLocations(rows), nil
}

// MoveObjectLocation atomically moves a copy of an object from one backend to
// another. Uses SELECT FOR UPDATE to prevent races. Returns (0, nil) if the
// source copy is gone or the target already has a copy.
func (s *Store) MoveObjectLocation(ctx context.Context, key, fromBackend, toBackend string) (int64, error) {
	return withTxVal(s, ctx, func(qtx *db.Queries) (int64, error) {
		targetHasCopy, err := qtx.CheckObjectExistsOnBackend(ctx, db.CheckObjectExistsOnBackendParams{
			ObjectKey:   key,
			BackendName: toBackend,
		})
		if err != nil {
			return 0, fmt.Errorf("failed to check target: %w", err)
		}
		if targetHasCopy {
			return 0, nil
		}
		locked, ok, err := lockSourceCopy(ctx, qtx, key, fromBackend)
		if err != nil || !ok {
			return 0, err
		}
		if err := moveCopyBetweenBackends(ctx, qtx, key, fromBackend, toBackend, locked); err != nil {
			return 0, err
		}
		return locked.SizeBytes, nil
	})
}

// lockSourceCopy takes a row-level lock on the source copy. Returns
// ok=false (with nil error) when the source copy is already gone — a
// benign race the caller treats as "nothing to move."
func lockSourceCopy(ctx context.Context, qtx *db.Queries, key, fromBackend string) (db.LockObjectOnBackendRow, bool, error) {
	locked, err := qtx.LockObjectOnBackend(ctx, db.LockObjectOnBackendParams{
		ObjectKey:   key,
		BackendName: fromBackend,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.LockObjectOnBackendRow{}, false, nil
	}
	if err != nil {
		return db.LockObjectOnBackendRow{}, false, fmt.Errorf("failed to lock object: %w", err)
	}
	return locked, true, nil
}

// moveCopyBetweenBackends performs the delete-source + insert-destination
// + quota-swap sequence after the source row has been locked. Preserves
// encryption and integrity metadata on the new row.
func moveCopyBetweenBackends(ctx context.Context, qtx *db.Queries, key, fromBackend, toBackend string, src db.LockObjectOnBackendRow) error {
	if err := qtx.DeleteObjectFromBackend(ctx, db.DeleteObjectFromBackendParams{
		ObjectKey:   key,
		BackendName: fromBackend,
	}); err != nil {
		return fmt.Errorf("failed to delete source location: %w", err)
	}
	if err := qtx.InsertObjectLocation(ctx, db.InsertObjectLocationParams{
		ObjectKey:     key,
		BackendName:   toBackend,
		SizeBytes:     src.SizeBytes,
		Encrypted:     src.Encrypted,
		EncryptionKey: src.EncryptionKey,
		KeyID:         src.KeyID,
		PlaintextSize: src.PlaintextSize,
		ContentHash:   src.ContentHash,
	}); err != nil {
		return fmt.Errorf("failed to insert destination location: %w", err)
	}
	if err := qtx.DecrementQuota(ctx, db.DecrementQuotaParams{
		Amount:      src.SizeBytes,
		BackendName: fromBackend,
	}); err != nil {
		return fmt.Errorf("failed to decrement source quota: %w", err)
	}
	return incrementBackendQuota(ctx, qtx, toBackend, src.SizeBytes)
}

// ListObjectsResult holds the result of a list objects query.
type ListObjectsResult struct {
	Objects               []ObjectLocation
	IsTruncated           bool
	NextContinuationToken string
}

// ListObjects returns objects matching the given prefix, sorted by key.
// Supports pagination via startAfter and maxKeys. Returns one extra row to
// detect truncation.
func (s *Store) ListObjects(ctx context.Context, prefix, startAfter string, maxKeys int) (*ListObjectsResult, error) {
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

	result := &ListObjectsResult{}
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
func (s *Store) ListExpiredObjects(ctx context.Context, prefix string, cutoff time.Time, limit int) ([]ObjectLocation, error) {
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

// DirEntry holds aggregate stats for one immediate child of a directory prefix.
type DirEntry struct {
	Name      string `json:"name"`      // absolute path (e.g. "bucket/photos/")
	IsDir     bool   `json:"isDir"`     // true for directories
	FileCount int64  `json:"fileCount"` // number of files (recursive for dirs)
	TotalSize int64  `json:"totalSize"` // total bytes (recursive for dirs)
	Backend   string `json:"backend"`   // backend name (files only)
	CreatedAt string `json:"createdAt"` // formatted timestamp (files only)
}

// DirectoryListResult holds the response for a lazy-loaded directory listing.
type DirectoryListResult struct {
	Entries    []DirEntry `json:"entries"`
	HasMore    bool       `json:"hasMore"`
	NextCursor string     `json:"nextCursor"`
}

// ListDirectoryChildren returns the immediate children of a directory prefix
// with aggregate stats for subdirectories. Files include backend and creation
// time. Prefix must end with "/" (or be "" for root).
func (s *Store) ListDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*DirectoryListResult, error) {
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
		Backend   string
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
			Backend:   row.BackendName,
			CreatedAt: row.CreatedAt.Time.Format("2006-01-02 15:04"),
		}
	}

	result := &DirectoryListResult{
		Entries: make([]DirEntry, 0, len(stats)),
	}

	for _, s := range stats {
		entry := DirEntry{
			Name:      prefix + s.Name,
			IsDir:     s.IsDir,
			FileCount: s.FileCount,
			TotalSize: s.TotalSize,
		}
		if !s.IsDir {
			if detail, ok := fileLookup[s.Name]; ok {
				entry.Backend = detail.Backend
				entry.CreatedAt = detail.CreatedAt
			} else {
				// File is outside the current page — skip it.
				continue
			}
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

// ImportObject records a pre-existing object in the database without overwriting.
// Returns true if the object was imported, false if it already existed for this
// backend. Used by the sync subcommand to bring existing bucket objects under
// proxy management.
func (s *Store) ImportObject(ctx context.Context, key, backend string, size int64) (bool, error) {
	return withTxVal(s, ctx, func(qtx *db.Queries) (bool, error) {
		inserted, err := qtx.InsertObjectLocationIfNotExists(ctx, db.InsertObjectLocationIfNotExistsParams{
			ObjectKey:   key,
			BackendName: backend,
			SizeBytes:   size,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("failed to import object %s: %w", key, err)
		}

		if !inserted {
			return false, nil
		}

		n, err := qtx.IncrementQuota(ctx, db.IncrementQuotaParams{
			Amount:      size,
			BackendName: backend,
		})
		if err != nil {
			return false, fmt.Errorf("failed to increment quota for %s: %w", backend, err)
		}
		if n == 0 {
			return false, ErrNoSpaceAvailable
		}

		return true, nil
	})
}
