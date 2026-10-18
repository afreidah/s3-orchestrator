// -------------------------------------------------------------------------------
// SQLite Cleanup Queue - Background Retry Worker Operations
//
// Author: Alex Freidah
//
// Implements cleanup queue operations for the SQLite backend: claim queued rows
// with backoff-aware scheduling, retire them once their bytes are gone, and
// schedule retries. Rows are queued by the transactions that stop recording
// bytes, through the tx adapter.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// QUEUE
// -------------------------------------------------------------------------

// GetPendingCleanups returns a read-only snapshot of pending cleanup rows
// for the admin endpoint. The cleanup worker uses ClaimPendingCleanups
// instead. claimed_at is parsed back from RFC3339Nano text since SQLite has
// no native timestamp type.
func (s *Store) GetPendingCleanups(ctx context.Context, limit int) ([]core.CleanupItem, error) {
	now := now()
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, backend_name, object_key, storage_key, reason, attempts, size_bytes,
		        claimed_at, claimed_by
		 FROM cleanup_queue
		 WHERE next_retry <= ? AND attempts < 10
		 ORDER BY next_retry ASC, created_at ASC
		 LIMIT ?`,
		now, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get pending cleanups: %w", err)
	}
	return collectRows(rows, "cleanup items", func(rows *sql.Rows) (core.CleanupItem, error) {
		var (
			item      core.CleanupItem
			claimedAt sql.NullString
			claimedBy sql.NullString
		)
		if err := rows.Scan(&item.ID, &item.BackendName, &item.ObjectKey, &item.StorageKey, &item.Reason, &item.Attempts, &item.SizeBytes, &claimedAt, &claimedBy); err != nil {
			return core.CleanupItem{}, fmt.Errorf("failed to scan cleanup item: %w", err)
		}
		item.ClaimedAt = parseNullableTime(claimedAt)
		if claimedBy.Valid {
			cb := claimedBy.String
			item.ClaimedBy = &cb
		}
		return item, nil
	})
}

// ClaimPendingCleanups atomically claims a batch of pending rows for the
// calling instance, using the same eligibility rules as Postgres (next_retry
// due, attempts < 10, claim NULL or older than graceCutoff). SQLite serializes
// writers, so the claim needs no row locking.
func (s *Store) ClaimPendingCleanups(ctx context.Context, limit int, instanceID string, graceCutoff time.Time) ([]core.CleanupItem, error) {
	now := now()
	cutoff := formatTime(graceCutoff)

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
// that satisfy ClaimPendingCleanups' eligibility predicates, in
// idx_cleanup_queue_claim order so the scan stops at the limit.
func selectClaimableRows(ctx context.Context, tx *sql.Tx, now, cutoff string, limit int) ([]core.CleanupItem, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, backend_name, object_key, storage_key, reason, attempts, size_bytes,
		        (claimed_at IS NOT NULL) AS reclaimed
		 FROM cleanup_queue
		 WHERE next_retry <= ?
		   AND attempts < 10
		   AND (claimed_at IS NULL OR claimed_at < ?)
		 ORDER BY next_retry ASC, created_at ASC
		 LIMIT ?`,
		now, cutoff, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("select cleanup candidates: %w", err)
	}
	return collectRows(rows, "cleanup candidates", func(rows *sql.Rows) (core.CleanupItem, error) {
		var item core.CleanupItem
		if err := rows.Scan(&item.ID, &item.BackendName, &item.ObjectKey, &item.StorageKey, &item.Reason, &item.Attempts, &item.SizeBytes, &item.Reclaimed); err != nil {
			return core.CleanupItem{}, fmt.Errorf("scan cleanup candidate: %w", err)
		}
		return item, nil
	})
}

// stampClaims marks each row in items as claimed by instanceID. The
// caller wraps both the SELECT and these UPDATEs in one transaction so
// the claim is atomic against concurrent workers.
func stampClaims(ctx context.Context, tx *sql.Tx, items []core.CleanupItem, instanceID string) error {
	stamp := now()
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

// -------------------------------------------------------------------------
// COMPLETION AND RETRY
// -------------------------------------------------------------------------

// CompleteCleanupItem retires a row whose bytes are gone. A row already
// retired is a no-op.
func (s *Store) CompleteCleanupItem(ctx context.Context, id int64) error {
	return s.direct().DeleteCleanupItem(ctx, id)
}

// SweepStaleCleanupQueueRows drops the queued cleanups for a path reconcile
// found missing from the backend, and returns how many went.
func (s *Store) SweepStaleCleanupQueueRows(ctx context.Context, storageKey, backend string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM cleanup_queue WHERE storage_key = ? AND backend_name = ?`,
		storageKey, backend,
	)
	if err != nil {
		return 0, fmt.Errorf("sweep stale cleanup rows: %w", err)
	}
	return res.RowsAffected()
}

// RetryCleanupItem increments the attempt counter, schedules the next retry,
// and clears the claim so the row is immediately re-eligible for the next
// worker tick.
func (s *Store) RetryCleanupItem(ctx context.Context, id int64, backoff time.Duration, lastError string) error {
	nextRetry := formatTime(time.Now().Add(backoff))
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

// -------------------------------------------------------------------------
// DEPTHS AND DLQ
// -------------------------------------------------------------------------

// CleanupQueueDepth returns the number of items still pending in the queue
// (fewer than 10 attempts).
func (s *Store) CleanupQueueDepth(ctx context.Context) (int64, error) {
	return s.countRows(ctx, "pending cleanups",
		`SELECT COUNT(*) FROM cleanup_queue WHERE attempts < 10`)
}

// CleanupDLQDepth returns the number of rows currently in cleanup_dlq.
// Surfaces the count of unrecoverable orphans so the dashboard and the
// cleanup_dlq_depth gauge can flag operator-visible work.
func (s *Store) CleanupDLQDepth(ctx context.Context) (int64, error) {
	return s.countRows(ctx, "cleanup DLQ rows", `SELECT COUNT(*) FROM cleanup_dlq`)
}

// ListCleanupDLQ returns dead-lettered cleanup rows for operator inspection,
// newest graduation first. An empty backend lists every backend.
func (s *Store) ListCleanupDLQ(ctx context.Context, backend string, limit int) ([]core.CleanupDLQItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT backend_name, object_key, storage_key, reason, size_bytes, attempts,
		       first_enqueued_at, moved_at, last_error
		FROM cleanup_dlq
		WHERE (? = '' OR backend_name = ?)
		ORDER BY moved_at DESC
		LIMIT ?`, backend, backend, limit)
	if err != nil {
		return nil, fmt.Errorf("list cleanup dlq: %w", err)
	}
	return collectRows(rows, "cleanup dlq rows", func(rows *sql.Rows) (core.CleanupDLQItem, error) {
		var (
			it              core.CleanupDLQItem
			firstEnq, moved string
			lastErr         sql.NullString
		)
		if err := rows.Scan(&it.BackendName, &it.ObjectKey, &it.StorageKey, &it.Reason, &it.SizeBytes,
			&it.Attempts, &firstEnq, &moved, &lastErr); err != nil {
			return core.CleanupDLQItem{}, fmt.Errorf("scan cleanup dlq row: %w", err)
		}
		it.FirstEnqueued, _ = time.Parse(time.RFC3339Nano, firstEnq)
		it.MovedAt, _ = time.Parse(time.RFC3339Nano, moved)
		it.LastError = lastErr.String
		return it, nil
	})
}

// RequeueCleanupDLQ moves dead-lettered rows back into cleanup_queue inside a
// single transaction (SQLite serialises writes, so the insert+delete pair is
// atomic). cleanup_queue column defaults reset created_at/next_retry to now
// and attempts to 0. Returns the number of rows requeued.
func (s *Store) RequeueCleanupDLQ(ctx context.Context, backend string) (int64, error) {
	// Set created_at/next_retry explicitly in RFC3339Nano to match
	// InsertCleanup and GetPendingCleanups' comparison format; the column
	// default's millisecond form sorts inconsistently (a trailing 'Z' outranks
	// digits), which would delay eligibility of the requeued rows.
	now := now()
	var n int64
	err := cbWithTx(ctx, s.rawDB, s.cb, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO cleanup_queue (backend_name, object_key, storage_key, reason, size_bytes, created_at, next_retry)
			SELECT backend_name, object_key, storage_key, reason, size_bytes, ?, ?
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
