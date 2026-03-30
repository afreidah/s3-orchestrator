// -------------------------------------------------------------------------------
// SQLite Directory Listing - Lazy-Loaded File Browser Queries
//
// Author: Alex Freidah
//
// Implements the directory tree listing for the web UI dashboard. Aggregates
// immediate children of a prefix into directories and files with counts and
// sizes. Uses INSTR/SUBSTR instead of PostgreSQL's position/substring.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/store"
)

// -------------------------------------------------------------------------
// DIRECTORY LISTING
// -------------------------------------------------------------------------

// ListDirectoryChildren returns immediate children (directories and files)
// under prefix, with aggregate stats for directories and detail for files.
// Pagination uses startAfter as the cursor for file-level detail.
func (s *Store) ListDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*store.DirectoryListResult, error) {
	if maxKeys <= 0 {
		maxKeys = 200
	}

	escapedPrefix := likeEscaper.Replace(prefix)

	// Aggregate stats for immediate children. Deduplicates replicated
	// objects via a MIN(rowid) subquery before grouping by relative name.
	statsQuery := `
		SELECT
			CASE WHEN INSTR(SUBSTR(object_key, LENGTH(?) + 1), '/') > 0
				THEN SUBSTR(SUBSTR(object_key, LENGTH(?) + 1), 1, INSTR(SUBSTR(object_key, LENGTH(?) + 1), '/'))
				ELSE SUBSTR(object_key, LENGTH(?) + 1)
			END AS name,
			CASE WHEN INSTR(SUBSTR(object_key, LENGTH(?) + 1), '/') > 0
				THEN 1 ELSE 0
			END AS is_dir,
			COUNT(*) AS file_count,
			COALESCE(SUM(size_bytes), 0) AS total_size
		FROM (
			SELECT o.object_key, o.size_bytes
			FROM object_locations o
			INNER JOIN (
				SELECT object_key, MIN(rowid) AS min_id
				FROM object_locations
				WHERE object_key LIKE ? || '%' ESCAPE '\'
				  AND LENGTH(object_key) > LENGTH(?)
				GROUP BY object_key
			) sub ON o.rowid = sub.min_id
		) deduped
		GROUP BY name, is_dir
		ORDER BY is_dir DESC, name ASC`

	statsRows, err := s.db.QueryContext(ctx, statsQuery,
		prefix, prefix, prefix, prefix, prefix,
		escapedPrefix, prefix,
	)
	if err != nil {
		return nil, fmt.Errorf("directory stats: %w", err)
	}
	defer statsRows.Close()

	type dirStat struct {
		Name      string
		IsDir     bool
		FileCount int64
		TotalSize int64
	}
	var stats []dirStat
	for statsRows.Next() {
		var ds dirStat
		var isDirInt int
		if err := statsRows.Scan(&ds.Name, &isDirInt, &ds.FileCount, &ds.TotalSize); err != nil {
			return nil, fmt.Errorf("scan directory stat: %w", err)
		}
		ds.IsDir = isDirInt == 1
		stats = append(stats, ds)
	}
	if err := statsRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate directory stats: %w", err)
	}

	// Paginated file detail for direct children (non-directory entries).
	fileQuery := `
		SELECT o.object_key, o.backend_name, o.created_at
		FROM object_locations o
		INNER JOIN (
			SELECT object_key, MIN(rowid) AS min_id
			FROM object_locations
			WHERE object_key LIKE ? || '%' ESCAPE '\'
			  AND LENGTH(object_key) > LENGTH(?)
			  AND INSTR(SUBSTR(object_key, LENGTH(?) + 1), '/') = 0
			  AND object_key > ?
			GROUP BY object_key
		) sub ON o.rowid = sub.min_id
		ORDER BY o.object_key
		LIMIT ?`

	fileRows, err := s.db.QueryContext(ctx, fileQuery,
		escapedPrefix, prefix, prefix, startAfter, maxKeys+1,
	)
	if err != nil {
		return nil, fmt.Errorf("list direct children: %w", err)
	}
	defer fileRows.Close()

	type fileDetail struct {
		Backend   string
		CreatedAt string
	}
	fileLookup := make(map[string]fileDetail)
	var fileKeys []string
	for fileRows.Next() {
		var objectKey, backend, createdAt string
		if err := fileRows.Scan(&objectKey, &backend, &createdAt); err != nil {
			return nil, fmt.Errorf("scan file detail: %w", err)
		}
		relName := objectKey[len(prefix):]
		fileLookup[relName] = fileDetail{Backend: backend, CreatedAt: formatTimestamp(createdAt)}
		fileKeys = append(fileKeys, objectKey)
	}
	if err := fileRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate file details: %w", err)
	}

	hasMore := len(fileKeys) > maxKeys
	if hasMore {
		fileKeys = fileKeys[:maxKeys]
		// Remove the extra entry from lookup
		extraRel := fileKeys[maxKeys-1][len(prefix):]
		_ = extraRel // lookup is keyed by relName, trim handled below
	}

	result := &store.DirectoryListResult{
		Entries: make([]store.DirEntry, 0, len(stats)),
	}

	for _, ds := range stats {
		entry := store.DirEntry{
			Name:      prefix + ds.Name,
			IsDir:     ds.IsDir,
			FileCount: ds.FileCount,
			TotalSize: ds.TotalSize,
		}
		if !ds.IsDir {
			if detail, ok := fileLookup[ds.Name]; ok {
				entry.Backend = detail.Backend
				entry.CreatedAt = detail.CreatedAt
			} else {
				// File outside current page — skip.
				continue
			}
		}
		result.Entries = append(result.Entries, entry)
	}

	if hasMore && len(fileKeys) > 0 {
		result.HasMore = true
		result.NextCursor = fileKeys[len(fileKeys)-1]
	}

	return result, nil
}

// formatTimestamp converts an RFC3339 timestamp to a short display format.
func formatTimestamp(ts string) string {
	t, err := parseTime(ts)
	if err != nil {
		return ts
	}
	return t.Format("2006-01-02 15:04")
}
