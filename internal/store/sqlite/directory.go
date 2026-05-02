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
	"sort"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// DIRECTORY LISTING
// -------------------------------------------------------------------------

// dirStat is one aggregated row from the directory stats query: either a
// sub-directory roll-up or a single file entry directly beneath the prefix.
type dirStat struct {
	Name      string
	IsDir     bool
	FileCount int64
	TotalSize int64
}

// fileDetail carries the per-file backend set, logical size, and display
// timestamp used to enrich direct-child file entries in ListDirectoryChildren.
type fileDetail struct {
	Backends  []string
	SizeBytes int64
	CreatedAt string
}

// ListDirectoryChildren returns immediate children (directories and files)
// under prefix, with aggregate stats for directories and detail for files.
// Pagination uses startAfter as the cursor for file-level detail.
func (s *Store) ListDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*core.DirectoryListResult, error) {
	if maxKeys <= 0 {
		maxKeys = 200
	}
	escapedPrefix := likeEscaper.Replace(prefix)

	stats, err := queryDirectoryStats(ctx, s, prefix, escapedPrefix)
	if err != nil {
		return nil, err
	}
	fileLookup, fileKeys, err := queryDirectFileDetails(ctx, s, prefix, escapedPrefix, startAfter, maxKeys)
	if err != nil {
		return nil, err
	}

	hasMore := len(fileKeys) > maxKeys
	if hasMore {
		fileKeys = fileKeys[:maxKeys]
	}

	result := &core.DirectoryListResult{
		Entries: buildDirectoryEntries(prefix, stats, fileLookup),
	}
	if hasMore && len(fileKeys) > 0 {
		result.HasMore = true
		result.NextCursor = fileKeys[len(fileKeys)-1]
	}
	return result, nil
}

// queryDirectoryStats returns aggregate stats for each immediate child
// under prefix. file_count counts distinct object_keys (logical files);
// total_size sums every replica row so directory totals reflect physical
// storage, matching the Storage Summary semantics.
func queryDirectoryStats(ctx context.Context, s *Store, prefix, escapedPrefix string) ([]dirStat, error) {
	const statsQuery = `
		SELECT
			CASE WHEN INSTR(SUBSTR(object_key, LENGTH(?) + 1), '/') > 0
				THEN SUBSTR(SUBSTR(object_key, LENGTH(?) + 1), 1, INSTR(SUBSTR(object_key, LENGTH(?) + 1), '/'))
				ELSE SUBSTR(object_key, LENGTH(?) + 1)
			END AS name,
			CASE WHEN INSTR(SUBSTR(object_key, LENGTH(?) + 1), '/') > 0
				THEN 1 ELSE 0
			END AS is_dir,
			COUNT(DISTINCT object_key) AS file_count,
			COALESCE(SUM(size_bytes), 0) AS total_size
		FROM object_locations
		WHERE object_key LIKE ? || '%' ESCAPE '\'
		  AND LENGTH(object_key) > LENGTH(?)
		GROUP BY name, is_dir
		ORDER BY is_dir DESC, name ASC`

	rows, err := s.db.QueryContext(ctx, statsQuery,
		prefix, prefix, prefix, prefix, prefix,
		escapedPrefix, prefix,
	)
	if err != nil {
		return nil, fmt.Errorf("directory stats: %w", err)
	}
	defer rows.Close()

	var stats []dirStat
	for rows.Next() {
		var ds dirStat
		var isDirInt int
		if err := rows.Scan(&ds.Name, &isDirInt, &ds.FileCount, &ds.TotalSize); err != nil {
			return nil, fmt.Errorf("scan directory stat: %w", err)
		}
		ds.IsDir = isDirInt == 1
		stats = append(stats, ds)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate directory stats: %w", err)
	}
	return stats, nil
}

// queryDirectFileDetails pages through the direct-child files (entries
// without a '/' after prefix), returning a rel-name -> backends/size/ctime
// lookup plus the ordered object-key list used for the "has more" cursor.
// Backends are aggregated into a sorted comma-separated string per
// object_key. One extra row is fetched to detect truncation.
func queryDirectFileDetails(ctx context.Context, s *Store, prefix, escapedPrefix, startAfter string, maxKeys int) (map[string]fileDetail, []string, error) {
	const fileQuery = `
		SELECT object_key,
		       GROUP_CONCAT(backend_name, ',') AS backends,
		       MAX(size_bytes) AS size_bytes,
		       MIN(created_at) AS created_at
		FROM object_locations
		WHERE object_key LIKE ? || '%' ESCAPE '\'
		  AND LENGTH(object_key) > LENGTH(?)
		  AND INSTR(SUBSTR(object_key, LENGTH(?) + 1), '/') = 0
		  AND object_key > ?
		GROUP BY object_key
		ORDER BY object_key
		LIMIT ?`

	rows, err := s.db.QueryContext(ctx, fileQuery,
		escapedPrefix, prefix, prefix, startAfter, maxKeys+1,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("list direct children: %w", err)
	}
	defer rows.Close()

	lookup := make(map[string]fileDetail)
	var keys []string
	for rows.Next() {
		var objectKey, backendList, createdAt string
		var sizeBytes int64
		if err := rows.Scan(&objectKey, &backendList, &sizeBytes, &createdAt); err != nil {
			return nil, nil, fmt.Errorf("scan file detail: %w", err)
		}
		relName := objectKey[len(prefix):]
		lookup[relName] = fileDetail{
			Backends:  splitAndSort(backendList),
			SizeBytes: sizeBytes,
			CreatedAt: formatTimestamp(createdAt),
		}
		keys = append(keys, objectKey)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate file details: %w", err)
	}
	return lookup, keys, nil
}

// buildDirectoryEntries merges stats roll-ups with per-file details,
// skipping files that fell outside the current page (no fileLookup entry).
// File rows report the logical size from fileDetail rather than the
// replica-summed total_size returned by the stats query.
func buildDirectoryEntries(prefix string, stats []dirStat, fileLookup map[string]fileDetail) []core.DirEntry {
	entries := make([]core.DirEntry, 0, len(stats))
	for _, ds := range stats {
		entry := core.DirEntry{
			Name:      prefix + ds.Name,
			IsDir:     ds.IsDir,
			FileCount: ds.FileCount,
			TotalSize: ds.TotalSize,
		}
		if !ds.IsDir {
			detail, ok := fileLookup[ds.Name]
			if !ok {
				continue
			}
			entry.Backends = detail.Backends
			entry.CreatedAt = detail.CreatedAt
			entry.TotalSize = detail.SizeBytes
		}
		entries = append(entries, entry)
	}
	return entries
}

// splitAndSort turns a comma-separated GROUP_CONCAT result into a sorted
// slice of backend names. Empty input yields nil.
func splitAndSort(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	sort.Strings(parts)
	return parts
}

// formatTimestamp converts an RFC3339 timestamp to a short display format.
func formatTimestamp(ts string) string {
	t, err := parseTime(ts)
	if err != nil {
		return ts
	}
	return t.Format("2006-01-02 15:04")
}
