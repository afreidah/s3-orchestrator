// -------------------------------------------------------------------------------
// SQLite Replication - The Replication Queue
//
// Author: Alex Freidah
//
// Implements the replication queue for the SQLite backend: the rebuild that
// queues every key the ledger holds off the factor, and the drain's read,
// settle, retry and count. A key's count is its managed copies plus its live
// intents, copies on an excluded backend not counting toward a short key; the
// Postgres queries carry the reasoning.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// queueUpsert makes a key already queued due now on its new side and bumps its
// version, so a drain that read an older version cannot settle it away.
const queueUpsert = `
	ON CONFLICT(object_key) DO UPDATE
	SET direction = excluded.direction, next_retry = excluded.next_retry,
	    attempts = 0, version = replication_queue.version + 1`

// -------------------------------------------------------------------------
// QUEUE WRITES
// -------------------------------------------------------------------------

// QueueReplication queues keys the transaction knows the side of, in one
// statement.
func (a *sqliteTxAdapter) QueueReplication(ctx context.Context, keys []string, directions []core.ReplicationDirection) error {
	sides := make([]int64, len(directions))
	for i, d := range directions {
		sides[i] = int64(d)
	}
	return a.exec(ctx, "queue replication",
		`INSERT INTO replication_queue (object_key, direction, next_retry)
		 SELECT k.value, d.value, ?3
		 FROM json_each(?1) k JOIN json_each(?2) d ON d.key = k.key
		 WHERE true`+queueUpsert,
		jsonList(keys), jsonList(sides), now())
}

// QueueOffFactor counts the keys and queues those off factor, in one
// statement. A key with no copy and no intent is gone and needs nothing.
func (a *sqliteTxAdapter) QueueOffFactor(ctx context.Context, factor int, keys ...string) error {
	return a.exec(ctx, "queue off-factor keys",
		`INSERT INTO replication_queue (object_key, direction, next_retry)
		 SELECT object_key, CASE WHEN live < ?1 THEN -1 ELSE 1 END, ?3
		 FROM (
		     SELECT k.value AS object_key,
		         (SELECT COUNT(*) FROM object_locations ol WHERE ol.object_key = k.value AND ol.managed)
		       + (SELECT COUNT(*) FROM pending_objects p WHERE p.object_key = k.value) AS live
		     FROM json_each(?2) k
		 )
		 WHERE live > 0 AND live <> ?1`+queueUpsert,
		factor, jsonList(keys), now())
}

// -------------------------------------------------------------------------
// REBUILD
// -------------------------------------------------------------------------

// RebuildReplicationQueue queues every key the ledger holds off factor in one
// grouped pass and reports how many it queued.
func (s *Store) RebuildReplicationQueue(ctx context.Context, factor int, excluded []string) (int64, error) {
	return withTxVal(ctx, s, func(a *sqliteTxAdapter) (int64, error) {
		return a.execRows(ctx, "rebuild replication queue",
			`INSERT INTO replication_queue (object_key, direction, next_retry)
			 SELECT object_key, CASE WHEN live < ?1 THEN -1 ELSE 1 END, ?3
			 FROM (
			     SELECT ol.object_key,
			            SUM(ol.backend_name NOT IN (SELECT value FROM json_each(?2))) + COALESCE(i.copies, 0) AS live,
			            COUNT(*) AS held
			     FROM object_locations ol
			     LEFT JOIN (
			         SELECT object_key, COUNT(*) AS copies FROM pending_objects GROUP BY object_key
			     ) i ON i.object_key = ol.object_key
			     WHERE ol.managed
			     GROUP BY ol.object_key, i.copies
			 )
			 WHERE live < ?1 OR held > ?1`+queueUpsert,
			factor, jsonList(excluded), now())
	})
}

// -------------------------------------------------------------------------
// DRAIN
// -------------------------------------------------------------------------

// ListReplicationQueue reads a side's next due keys, each with its count and
// the version a settle or retry must match, in idx_replication_queue_due
// order.
func (s *Store) ListReplicationQueue(ctx context.Context, direction core.ReplicationDirection, limit int, excluded []string) ([]core.QueuedReplication, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT rq.object_key, rq.direction, rq.attempts, rq.version,
		     (SELECT COUNT(*) FROM object_locations ol
		      WHERE ol.object_key = rq.object_key AND ol.managed AND ol.backend_name NOT IN (SELECT value FROM json_each(?4)))
		   + (SELECT COUNT(*) FROM pending_objects p WHERE p.object_key = rq.object_key),
		     (SELECT COUNT(*) FROM object_locations ol WHERE ol.object_key = rq.object_key AND ol.managed)
		 FROM replication_queue rq
		 WHERE rq.direction = ?1 AND rq.next_retry <= ?2
		 ORDER BY rq.direction, rq.next_retry
		 LIMIT ?3`,
		int64(direction), now(), limit, jsonList(excluded))
	if err != nil {
		return nil, fmt.Errorf("failed to read replication queue: %w", err)
	}
	return collectRows(rows, "replication queue", func(rows *sql.Rows) (core.QueuedReplication, error) {
		var q core.QueuedReplication
		err := rows.Scan(&q.Key, &q.Direction, &q.Attempts, &q.Version, &q.Live, &q.Held)
		return q, err
	})
}

// SettleReplicationQueue drops a key the drain brought back to the factor,
// unless a write queued it again since the drain read it.
func (s *Store) SettleReplicationQueue(ctx context.Context, q *core.QueuedReplication) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM replication_queue WHERE object_key = ? AND version = ?`, q.Key, q.Version)
	if err != nil {
		return fmt.Errorf("failed to settle replication queue key: %w", err)
	}
	return nil
}

// RetryReplicationQueue holds a key the drain left off factor until
// nextRetry, unless a write queued it again since the drain read it.
func (s *Store) RetryReplicationQueue(ctx context.Context, q *core.QueuedReplication, nextRetry time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE replication_queue SET attempts = attempts + 1, next_retry = ?
		 WHERE object_key = ? AND version = ?`,
		formatTime(nextRetry), q.Key, q.Version)
	if err != nil {
		return fmt.Errorf("failed to retry replication queue key: %w", err)
	}
	return nil
}

// CountReplicationQueue counts the queued keys on each side of the factor.
func (s *Store) CountReplicationQueue(ctx context.Context) (core.ReplicationBacklog, error) {
	var b core.ReplicationBacklog
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(direction < 0), 0), COALESCE(SUM(direction > 0), 0) FROM replication_queue`,
	).Scan(&b.Under, &b.Over)
	if err != nil {
		return core.ReplicationBacklog{}, fmt.Errorf("failed to count replication queue: %w", err)
	}
	return b, nil
}
