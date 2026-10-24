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
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// DIRECTORY LISTING
// -------------------------------------------------------------------------

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
	files, err := queryDirectFiles(ctx, s, prefix, escapedPrefix, startAfter, maxKeys)
	if err != nil {
		return nil, err
	}
	return core.BuildDirectoryListing(prefix, stats, files, maxKeys), nil
}

// queryDirectoryStats returns aggregate stats for each immediate child
// under prefix. file_count counts distinct object_keys (logical files);
// total_size sums every replica row so directory totals reflect physical
// storage, matching the Storage Summary semantics.
func queryDirectoryStats(ctx context.Context, s *Store, prefix, escapedPrefix string) ([]core.DirStat, error) {
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
	return collectRows(rows, "directory stats", func(rows *sql.Rows) (core.DirStat, error) {
		var (
			ds       core.DirStat
			isDirInt int
		)
		if err := rows.Scan(&ds.Name, &isDirInt, &ds.FileCount, &ds.TotalSize); err != nil {
			return core.DirStat{}, fmt.Errorf("scan directory stat: %w", err)
		}
		ds.IsDir = isDirInt == 1
		return ds, nil
	})
}

// queryDirectFiles pages through the direct-child files (entries without a '/'
// after prefix) in key order. Backends are aggregated into a comma-separated
// string per object_key and returned sorted. One extra row is fetched to
// detect truncation.
func queryDirectFiles(ctx context.Context, s *Store, prefix, escapedPrefix, startAfter string, maxKeys int) ([]core.DirFile, error) {
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
		return nil, fmt.Errorf("list direct children: %w", err)
	}
	return collectRows(rows, "file details", func(rows *sql.Rows) (core.DirFile, error) {
		var (
			f           core.DirFile
			backendList string
			createdAt   string
		)
		if err := rows.Scan(&f.ObjectKey, &backendList, &f.SizeBytes, &createdAt); err != nil {
			return core.DirFile{}, fmt.Errorf("scan file detail: %w", err)
		}
		f.Backends = splitAndSort(backendList)
		var err error
		if f.CreatedAt, err = parseTime(createdAt); err != nil {
			return core.DirFile{}, fmt.Errorf(errInvalidTimestamp, createdAt, err)
		}
		return f, nil
	})
}

// splitAndSort turns a comma-separated GROUP_CONCAT result into a sorted
// slice of backend names. Empty input yields nil.
func splitAndSort(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	slices.Sort(parts)
	return parts
}
