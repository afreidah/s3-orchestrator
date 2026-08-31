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

// GetObjectTags returns an object's tag set ordered by key. An object with no
// tags yields an empty slice, not an error: an untagged object has an empty
// TagSet rather than a missing one, and the caller answers 200 either way.
//
// Ordered in SQL rather than in Go so both engines hand back the same sequence
// without the caller having to re-sort what it was given.
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

// CountObjectTags returns how many tags an object carries. An object with no
// tags counts zero rather than erroring, for the same reason GetObjectTags
// returns an empty set: the read path asks this of every object it serves,
// and most of them are untagged.
func (s *Store) CountObjectTags(ctx context.Context, key string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM object_tags WHERE object_key = ?`,
		key,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("failed to count object tags: %w", err)
	}
	return n, nil
}
