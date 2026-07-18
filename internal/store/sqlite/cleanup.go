// -------------------------------------------------------------------------------
// SQLite Cleanup Queue - Background Retry Worker Operations
//
// Author: Alex Freidah
//
// Implements cleanup queue operations for the SQLite backend: enqueue failed
// deletions, fetch pending items with backoff-aware scheduling, and mark items
// as completed or retried.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// EnqueueCleanup adds a failed cleanup operation to the retry queue.
func (s *Store) EnqueueCleanup(ctx context.Context, backendName, objectKey, reason string, sizeBytes int64) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO cleanup_queue (backend_name, object_key, reason, size_bytes, created_at, next_retry)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		backendName, objectKey, reason, sizeBytes, now, now,
	)
	if err != nil {
		return fmt.Errorf("failed to enqueue cleanup: %w", err)
	}
	return nil
}

// GetPendingCleanups returns a read-only snapshot of pending cleanup rows
// for the admin endpoint. The cleanup worker uses ClaimPendingCleanups
// instead. claimed_at is parsed back from RFC3339Nano text since SQLite has
// no native timestamp type.
func (s *Store) GetPendingCleanups(ctx context.Context, limit int) ([]core.CleanupItem, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, backend_name, object_key, reason, attempts, size_bytes,
		        claimed_at, claimed_by
		 FROM cleanup_queue
		 WHERE next_retry <= ? AND attempts < 10
		 ORDER BY created_at ASC
		 LIMIT ?`,
		now, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get pending cleanups: %w", err)
	}
	defer rows.Close()

	var items []core.CleanupItem
	for rows.Next() {
		var item core.CleanupItem
		var claimedAt sql.NullString
		var claimedBy sql.NullString
		if err := rows.Scan(&item.ID, &item.BackendName, &item.ObjectKey, &item.Reason, &item.Attempts, &item.SizeBytes, &claimedAt, &claimedBy); err != nil {
			return nil, fmt.Errorf("failed to scan cleanup item: %w", err)
		}
		item.ClaimedAt = parseNullableTime(claimedAt)
		if claimedBy.Valid {
			cb := claimedBy.String
			item.ClaimedBy = &cb
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ClaimPendingCleanups atomically claims a batch of pending rows for the
// calling instance. SQLite serialises writes intrinsically (one writer at a
// time per connection), so a single UPDATE...WHERE id IN (SELECT...) is
// race-free against itself; the same eligibility predicates as the postgres
// path apply (next_retry due, attempts < 10, claim NULL or older than
// graceCutoff). The reclaimed flag is computed at SELECT time.
func (s *Store) ClaimPendingCleanups(ctx context.Context, limit int, instanceID string, graceCutoff time.Time) ([]core.CleanupItem, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	cutoff := graceCutoff.UTC().Format(time.RFC3339Nano)

	var items []core.CleanupItem
	err := cbWithTx(ctx, s.rawDB, s.cb, func(tx *sql.Tx) error {
		selected, err := selectClaimableRows(ctx, tx, now, cutoff, limit)
		if err != nil || len(selected) == 0 {
			items = selected
			return err
		}
		if err := stampClaims(ctx, tx, selected, instanceID); err != nil {
			return err
		}
		items = selected
		return nil
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}

// selectClaimableRows fetches the next batch of cleanup_queue rows
// that satisfy ClaimPendingCleanups' eligibility predicates.
func selectClaimableRows(ctx context.Context, tx *sql.Tx, now, cutoff string, limit int) ([]core.CleanupItem, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, backend_name, object_key, reason, attempts, size_bytes,
		        (claimed_at IS NOT NULL) AS reclaimed
		 FROM cleanup_queue
		 WHERE next_retry <= ?
		   AND attempts < 10
		   AND (claimed_at IS NULL OR claimed_at < ?)
		 ORDER BY created_at ASC
		 LIMIT ?`,
		now, cutoff, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("select cleanup candidates: %w", err)
	}
	defer rows.Close()

	var items []core.CleanupItem
	for rows.Next() {
		var item core.CleanupItem
		if err := rows.Scan(&item.ID, &item.BackendName, &item.ObjectKey, &item.Reason, &item.Attempts, &item.SizeBytes, &item.Reclaimed); err != nil {
			return nil, fmt.Errorf("scan cleanup candidate: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cleanup candidates: %w", err)
	}
	return items, nil
}

// stampClaims marks each row in items as claimed by instanceID. The
// caller wraps both the SELECT and these UPDATEs in one transaction so
// the claim is atomic against concurrent workers.
func stampClaims(ctx context.Context, tx *sql.Tx, items []core.CleanupItem, instanceID string) error {
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	for i := range items {
		if _, err := tx.ExecContext(ctx,
			`UPDATE cleanup_queue
			 SET claimed_at = ?, claimed_by = ?
			 WHERE id = ?`,
			stamp, instanceID, items[i].ID,
		); err != nil {
			return fmt.Errorf("stamp cleanup claim: %w", err)
		}
	}
	return nil
}

// CompleteCleanupItem atomically deletes a successfully-processed row and
// decrements the backing backend's orphan_bytes by the row's size. The two
// statements run inside a single transaction so a worker crash between them
// cannot leave the counter inconsistent. Idempotent against re-claim retries:
// if the row was already deleted the update affects zero rows.
func (s *Store) CompleteCleanupItem(ctx context.Context, id int64) error {
	return cbWithTx(ctx, s.rawDB, s.cb, func(tx *sql.Tx) error {
		var backend string
		var size int64
		row := tx.QueryRowContext(ctx,
			`DELETE FROM cleanup_queue
			 WHERE id = ?
			 RETURNING backend_name, size_bytes`,
			id,
		)
		if err := row.Scan(&backend, &size); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("delete cleanup row: %w", err)
		}
		if size > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE backend_quotas
				 SET orphan_bytes = MAX(0, orphan_bytes - ?)
				 WHERE backend_name = ?`,
				size, backend,
			); err != nil {
				return fmt.Errorf("decrement orphan bytes: %w", err)
			}
		}
		return nil
	})
}

// RetryCleanupItem increments the attempt counter, schedules the next retry,
// and clears the claim so the row is immediately re-eligible for the next
// worker tick.
func (s *Store) RetryCleanupItem(ctx context.Context, id int64, backoff time.Duration, lastError string) error {
	nextRetry := time.Now().Add(backoff).UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx,
		`UPDATE cleanup_queue
		 SET attempts = attempts + 1,
		     next_retry = ?,
		     last_error = ?,
		     claimed_at = NULL,
		     claimed_by = NULL
		 WHERE id = ?`,
		nextRetry, lastError, id,
	)
	if err != nil {
		return fmt.Errorf("failed to update cleanup retry: %w", err)
	}
	return nil
}

// parseNullableTime returns a *time.Time parsed from a SQLite string column,
// or nil when the column was NULL or unparseable.
func parseNullableTime(s sql.NullString) *time.Time {
	if !s.Valid {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s.String)
	if err != nil {
		return nil
	}
	return &t
}

// SweepStaleCleanupQueueRows delegates to core.SweepStaleCleanupQueueRows.
func (s *Store) SweepStaleCleanupQueueRows(ctx context.Context, key, backend string) (int64, error) {
	return core.SweepStaleCleanupQueueRows(ctx, s, key, backend)
}

// CleanupQueueDepth returns the number of items still pending in the queue
// (fewer than 10 attempts).
func (s *Store) CleanupQueueDepth(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM cleanup_queue WHERE attempts < 10`,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count pending cleanups: %w", err)
	}
	return count, nil
}

// CleanupDLQDepth returns the number of rows currently in cleanup_dlq.
// Surfaces the count of unrecoverable orphans so the dashboard and the
// cleanup_dlq_depth gauge can flag operator-visible work.
func (s *Store) CleanupDLQDepth(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM cleanup_dlq`,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count cleanup DLQ rows: %w", err)
	}
	return count, nil
}

// MoveCleanupToDLQ atomically graduates an exhausted cleanup_queue row
// to the dead-letter table. Delegates to core.MoveCleanupToDLQ so both
// engines share the move semantics - notably that orphan_bytes is left
// untouched because the backend object is still on disk.
func (s *Store) MoveCleanupToDLQ(ctx context.Context, id int64, lastError string) (bool, error) {
	return core.MoveCleanupToDLQ(ctx, s, id, lastError)
}

// ListCleanupDLQ returns dead-lettered cleanup rows for operator inspection,
// newest graduation first. An empty backend lists every backend.
func (s *Store) ListCleanupDLQ(ctx context.Context, backend string, limit int) ([]core.CleanupDLQItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT backend_name, object_key, reason, size_bytes, attempts,
		       first_enqueued_at, moved_at, last_error
		FROM cleanup_dlq
		WHERE (? = '' OR backend_name = ?)
		ORDER BY moved_at DESC
		LIMIT ?`, backend, backend, limit)
	if err != nil {
		return nil, fmt.Errorf("list cleanup dlq: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []core.CleanupDLQItem
	for rows.Next() {
		var (
			it              core.CleanupDLQItem
			firstEnq, moved string
			lastErr         sql.NullString
		)
		if err := rows.Scan(&it.BackendName, &it.ObjectKey, &it.Reason, &it.SizeBytes,
			&it.Attempts, &firstEnq, &moved, &lastErr); err != nil {
			return nil, fmt.Errorf("scan cleanup dlq row: %w", err)
		}
		it.FirstEnqueued, _ = time.Parse(time.RFC3339Nano, firstEnq)
		it.MovedAt, _ = time.Parse(time.RFC3339Nano, moved)
		it.LastError = lastErr.String
		out = append(out, it)
	}
	return out, rows.Err()
}

// RequeueCleanupDLQ moves dead-lettered rows back into cleanup_queue inside a
// single transaction (SQLite serialises writes, so the insert+delete pair is
// atomic). cleanup_queue column defaults reset created_at/next_retry to now
// and attempts to 0. Returns the number of rows requeued.
func (s *Store) RequeueCleanupDLQ(ctx context.Context, backend string) (int64, error) {
	// Set created_at/next_retry explicitly in RFC3339Nano to match
	// EnqueueCleanup and GetPendingCleanups' comparison format; the column
	// default's millisecond form sorts inconsistently (a trailing 'Z' outranks
	// digits), which would delay eligibility of the requeued rows.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var n int64
	err := cbWithTx(ctx, s.rawDB, s.cb, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO cleanup_queue (backend_name, object_key, reason, size_bytes, created_at, next_retry)
			SELECT backend_name, object_key, reason, size_bytes, ?, ?
			FROM cleanup_dlq
			WHERE (? = '' OR backend_name = ?)`, now, now, backend, backend)
		if err != nil {
			return fmt.Errorf("requeue insert: %w", err)
		}
		n, _ = res.RowsAffected()
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM cleanup_dlq WHERE (? = '' OR backend_name = ?)`, backend, backend); err != nil {
			return fmt.Errorf("requeue delete: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}
