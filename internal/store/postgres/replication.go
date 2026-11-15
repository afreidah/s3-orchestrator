// -------------------------------------------------------------------------------
// Replication Operations
//
// Author: Alex Freidah
//
// Implements the Postgres engine bindings for the replication-related
// queries: the copy lookups and the replication queue's rebuild, read,
// settle, retry and count. The rebuild and the read take the backends the
// drain is skipping, such as circuit-broken ones.
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
// COPY LOOKUP
// -------------------------------------------------------------------------

// GetAllObjectLocations returns all copies of an object, ordered by created_at
// ascending (oldest/primary first), each carrying the key's tag count. Used
// for read failover.
func (s *Store) GetAllObjectLocations(ctx context.Context, key string) ([]core.ObjectLocation, error) {
	rows, err := s.queries.GetAllObjectLocations(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("failed to get object locations: %w", err)
	}

	if len(rows) == 0 {
		return nil, core.ErrObjectNotFound
	}

	locs := make([]core.ObjectLocation, len(rows))
	for i := range rows {
		locs[i] = toObjectLocation(&rows[i].ObjectLocation)
		locs[i].TagCount = int(rows[i].TagCount)
	}
	return locs, nil
}

// GetObjectBackendsForKeys returns a map from each supplied object_key to
// the set of backend names that hold a copy. Empty input yields an empty
// map; keys with no copies are absent from the result. Used by the
// rebalancer planner to fold the per-key existence check into a single
// query per batch instead of N+1.
func (s *Store) GetObjectBackendsForKeys(ctx context.Context, keys []string) (map[string][]string, error) {
	if len(keys) == 0 {
		return map[string][]string{}, nil
	}
	rows, err := s.queries.GetObjectBackendsForKeys(ctx, keys)
	if err != nil {
		return nil, fmt.Errorf("failed to get object backends for keys: %w", err)
	}
	out := make(map[string][]string, len(keys))
	for _, r := range rows {
		out[r.ObjectKey] = append(out[r.ObjectKey], r.BackendName)
	}
	return out, nil
}

// -------------------------------------------------------------------------
// REPLICATION QUEUE
// -------------------------------------------------------------------------

// RebuildReplicationQueue queues every key the ledger holds off factor in one
// grouped pass and reports how many it queued.
func (s *Store) RebuildReplicationQueue(ctx context.Context, factor int, excluded []string) (int64, error) {
	n, err := s.queries.RebuildReplicationQueue(ctx, db.RebuildReplicationQueueParams{
		Excluded: excludedList(excluded),
		Factor:   int64(factor),
	})
	if err != nil {
		return 0, fmt.Errorf("failed to rebuild replication queue: %w", err)
	}
	return n, nil
}

// ListReplicationQueue reads a side's next due keys, each with its count and
// the version a settle or retry must match.
func (s *Store) ListReplicationQueue(ctx context.Context, direction core.ReplicationDirection, limit int, excluded []string) ([]core.QueuedReplication, error) {
	rows, err := s.queries.ListReplicationQueue(ctx, db.ListReplicationQueueParams{
		Excluded:  excludedList(excluded),
		Direction: int16(direction),
		RowLimit:  rowLimit(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to read replication queue: %w", err)
	}
	return mapSlice(rows, func(r *db.ListReplicationQueueRow) core.QueuedReplication {
		return core.QueuedReplication{
			Key:       r.ObjectKey,
			Direction: core.ReplicationDirection(r.Direction),
			Attempts:  r.Attempts,
			Version:   r.Version,
			Live:      int(r.Live),
			Held:      int(r.Held),
		}
	}), nil
}

// SettleReplicationQueue drops a key the drain brought back to the factor,
// unless a write queued it again since the drain read it.
func (s *Store) SettleReplicationQueue(ctx context.Context, q *core.QueuedReplication) error {
	if err := s.queries.SettleReplicationQueue(ctx, db.SettleReplicationQueueParams{
		ObjectKey: q.Key,
		Version:   q.Version,
	}); err != nil {
		return fmt.Errorf("failed to settle replication queue key: %w", err)
	}
	return nil
}

// RetryReplicationQueue holds a key the drain left off factor until
// nextRetry, unless a write queued it again since the drain read it.
func (s *Store) RetryReplicationQueue(ctx context.Context, q *core.QueuedReplication, nextRetry time.Time) error {
	if err := s.queries.RetryReplicationQueue(ctx, db.RetryReplicationQueueParams{
		ObjectKey: q.Key,
		NextRetry: pgTimestamptz(nextRetry),
		Version:   q.Version,
	}); err != nil {
		return fmt.Errorf("failed to retry replication queue key: %w", err)
	}
	return nil
}

// CountReplicationQueue counts the queued keys on each side of the factor.
func (s *Store) CountReplicationQueue(ctx context.Context) (core.ReplicationBacklog, error) {
	row, err := s.queries.CountReplicationQueue(ctx)
	if err != nil {
		return core.ReplicationBacklog{}, fmt.Errorf("failed to count replication queue: %w", err)
	}
	return core.ReplicationBacklog{Under: row.UnderReplicated, Over: row.OverReplicated}, nil
}

// excludedList binds an empty list for nil, since a nil slice binds as NULL
// and != ALL(NULL) would match no row.
func excludedList(excluded []string) []string {
	if excluded == nil {
		return []string{}
	}
	return excluded
}
