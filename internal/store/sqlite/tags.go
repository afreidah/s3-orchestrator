// -------------------------------------------------------------------------------
// SQLite Store - Object Tags
//
// Author: Alex Freidah
//
// The read side of object_tags. The writes are transactional and promoted from
// core.TxOps, so this engine contributes only the query that reads a set back.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// GetObjectTags returns an object's tag set ordered by key. An untagged object
// yields an empty slice, not an error, because S3 answers 200 with an empty
// TagSet.
func (s *Store) GetObjectTags(ctx context.Context, key string) ([]core.Tag, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tag_key, tag_value
		 FROM object_tags
		 WHERE object_key = ?
		 ORDER BY tag_key`,
		key,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get object tags: %w", err)
	}
	tags, err := collectRows(rows, "object tags", func(rows *sql.Rows) (core.Tag, error) {
		var t core.Tag
		if err := rows.Scan(&t.Key, &t.Value); err != nil {
			return core.Tag{}, fmt.Errorf("failed to scan object tag: %w", err)
		}
		return t, nil
	})
	if err != nil {
		return nil, err
	}
	// An untagged object has an empty set, not a nil one: the caller renders
	// this straight into a TagSet and a nil would encode as absent.
	if tags == nil {
		tags = []core.Tag{}
	}
	return tags, nil
}
