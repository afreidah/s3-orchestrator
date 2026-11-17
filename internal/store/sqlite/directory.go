// -------------------------------------------------------------------------------
// SQLite Directory Listing - File Browser Folder Totals
//
// Author: Alex Freidah
//
// Totals the folders directly beneath a directory prefix for the web UI's file
// browser, over the key range one listing page covers. Uses INSTR/SUBSTR
// instead of PostgreSQL's position/substring.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// DirectoryStats totals each folder directly beneath prefix whose keys fall in
// [from, end). file_count counts distinct object_keys (logical files);
// total_size sums every replica row so folder totals reflect physical storage,
// matching the Storage Summary semantics.
func (s *Store) DirectoryStats(ctx context.Context, prefix, from, end string) ([]core.DirStat, error) {
	const statsQuery = `
		SELECT
			SUBSTR(SUBSTR(object_key, LENGTH(?) + 1), 1, INSTR(SUBSTR(object_key, LENGTH(?) + 1), '/')) AS name,
			COUNT(DISTINCT object_key) AS file_count,
			COALESCE(SUM(size_bytes), 0) AS total_size
		FROM object_locations
		WHERE object_key >= ? AND object_key < ?
		  AND INSTR(SUBSTR(object_key, LENGTH(?) + 1), '/') > 0
		GROUP BY name
		ORDER BY name`

	rows, err := s.db.QueryContext(ctx, statsQuery, prefix, prefix, from, end, prefix)
	if err != nil {
		return nil, fmt.Errorf("directory stats: %w", err)
	}
	return collectRows(rows, "directory stats", func(rows *sql.Rows) (core.DirStat, error) {
		var ds core.DirStat
		if err := rows.Scan(&ds.Name, &ds.FileCount, &ds.TotalSize); err != nil {
			return core.DirStat{}, fmt.Errorf("scan directory stat: %w", err)
		}
		return ds, nil
	})
}
