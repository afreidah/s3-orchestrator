// -------------------------------------------------------------------------------
// Postgres Store - Object Tags
//
// Author: Alex Freidah
//
// The read side of object_tags. The writes are transactional and promoted from
// core.TxOps, so this engine contributes only the query that reads a set back.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// GetObjectTags returns an object's tag set ordered by key. An object with no
// tags yields an empty slice, not an error: an untagged object has an empty
// TagSet rather than a missing one, and the caller answers 200 either way.
func (s *Store) GetObjectTags(ctx context.Context, key string) ([]core.Tag, error) {
	rows, err := s.queries.GetObjectTags(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("failed to get object tags: %w", err)
	}
	return mapSlice(rows, tagFromRow), nil
}

// tagFromRow converts a sqlc GetObjectTags row into the canonical core.Tag.
func tagFromRow(r *db.GetObjectTagsRow) core.Tag {
	return core.Tag{Key: r.TagKey, Value: r.TagValue}
}
