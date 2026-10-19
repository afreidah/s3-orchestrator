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
// instead. It selects the columns selectClaimableRows does, so both scan
// through scanCleanupItem.
func (s *Store) GetPendingCleanups(ctx context.Context, limit int) ([]core.CleanupItem, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, backend_name, object_key, storage_key, reason, attempts, size_bytes,
		        claimed_at, claimed_by, 0 AS reclaimed
		 FROM cleanup_queue
		 WHERE next_retry <= ? AND attempts < ?
		 ORDER BY next_retry ASC, created_at ASC
		 LIMIT ?`,
		now(), core.MaxCleanupAttempts, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get pending cleanups: %w", err)
	}
	return collectRows(rows, "cleanup items", scanCleanupItem)
}

// scanCleanupItem reads one cleanup row selected by GetPendingCleanups or
// selectClaimableRows. claimed_at is parsed back from its text form, since SQLite
// has no native timestamp type.
func scanCleanupItem(rows *sql.Rows) (core.CleanupItem, error) {
	var (
		item      core.CleanupItem
		claimedAt sql.NullString
		claimedBy sql.NullString
	)
	if err := rows.Scan(&item.ID, &item.BackendName, &item.ObjectKey, &item.StorageKey, &item.Reason,
		&item.Attempts, &item.SizeBytes, &claimedAt, &claimedBy, &item.Reclaimed); err != nil {
		return core.CleanupItem{}, fmt.Errorf("failed to scan cleanup item: %w", err)
	}
	if at := parseNullableTime(claimedAt); at != nil {
		item.Claim = &core.CleanupClaim{At: *at, By: nullStringValue(claimedBy)}
	}
	return item, nil
}

// ClaimPendingCleanups atomically claims a batch of pending rows for the
// calling instance, using the same eligibility rules as Postgres (next_retry
// due, attempts below the ceiling, claim NULL or older than graceCutoff), and
// returns each with the claim it stamped. SQLite serializes writers, so the
// claim needs no row locking.
func (s *Store) ClaimPendingCleanups(ctx context.Context, limit int, instanceID string, graceCutoff time.Time) ([]core.CleanupItem, error) {
	cutoff := formatTime(graceCutoff)

	var items []core.CleanupItem
	err := cbWithTx(ctx, s.rawDB, s.cb, func(tx *sql.Tx) error {
		selected, err := selectClaimableRows(ctx, tx, now(), cutoff, limit)
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
		        claimed_at, claimed_by, (claimed_at IS NOT NULL) AS reclaimed
		 FROM cleanup_queue
		 WHERE next_retry <= ?
		   AND attempts < ?
		   AND (claimed_at IS NULL OR claimed_at < ?)
		 ORDER BY next_retry ASC, created_at ASC
		 LIMIT ?`,
		now, core.MaxCleanupAttempts, cutoff, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("select cleanup candidates: %w", err)
	}
	return collectRows(rows, "cleanup candidates", scanCleanupItem)
}

// stampClaims marks each row in items as claimed by instanceID and records the
// claim on the item. The caller wraps both the SELECT and these UPDATEs in one
// transaction so the claim is atomic against concurrent workers.
func stampClaims(ctx context.Context, tx *sql.Tx, items []core.CleanupItem, instanceID string) error {
	claimedAt := time.Now()
	stamp := formatTime(claimedAt)
	for i := range items {
		if _, err := tx.ExecContext(ctx,
			`UPDATE cleanup_queue
			 SET claimed_at = ?, claimed_by = ?
			 WHERE id = ?`,
			stamp, instanceID, items[i].ID,
		); err != nil {
			return fmt.Errorf("stamp cleanup claim: %w", err)
		}
		items[i].Claim = &core.CleanupClaim{At: claimedAt, By: instanceID}
	}
	return nil
}

// -------------------------------------------------------------------------
// SETTLING
// -------------------------------------------------------------------------

// deleteClaimedCleanup removes one cleanup row while the claim the settler
// read still holds it, for both a completed row and one moved to the DLQ.
const deleteClaimedCleanup = `DELETE FROM cleanup_queue WHERE id = ? AND claimed_at = ? AND COALESCE(claimed_by, '') = ?`

// claimArgs returns the arguments of the claim condition every settle write
// ends with, the same one deleteClaimedCleanup spells out, so it acts on a row
// only while the claim the settler read still holds it. An item with no claim
// matches no row, since every row a settle reaches was claimed when it was
// read.
func claimArgs(item *core.CleanupItem) []any {
	if item.Claim == nil {
		return []any{item.ID, "", ""}
	}
	return []any{item.ID, formatTime(item.Claim.At), item.Claim.By}
}

// CompleteCleanupItems retires the rows whose bytes are gone, each only while
// it is still held under its claim, in one transaction, and returns the ids
// it retired.
func (s *Store) CompleteCleanupItems(ctx context.Context, items []core.CleanupItem) ([]int64, error) {
	if len(items) == 0 {
		return nil, nil
	}
	var retired []int64
	err := cbWithTx(ctx, s.rawDB, s.cb, func(tx *sql.Tx) error {
		a := &sqliteTxAdapter{q: tx}
		for i := range items {
			done, err := a.execChanged(ctx, "complete cleanup item", deleteClaimedCleanup, claimArgs(&items[i])...)
			if err != nil {
				return err
			}
			if done {
				retired = append(retired, items[i].ID)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return retired, nil
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
// worker tick. Reports false when the item's claim no longer holds the row.
func (s *Store) RetryCleanupItem(ctx context.Context, item *core.CleanupItem, backoff time.Duration, lastError string) (bool, error) {
	nextRetry := formatTime(time.Now().Add(backoff))
	args := append([]any{nextRetry, lastError}, claimArgs(item)...)
	return s.execChanged(ctx, "update cleanup retry",
		`UPDATE cleanup_queue
		 SET attempts = attempts + 1,
		     next_retry = ?,
		     last_error = ?,
		     claimed_at = NULL,
		     claimed_by = NULL
		 WHERE id = ? AND claimed_at = ? AND COALESCE(claimed_by, '') = ?`,
		args...,
	)
}

// MoveCleanupToDLQ graduates an exhausted row to cleanup_dlq, matching the
// claim the settler read, and reports false when that claim no longer holds
// the row. original_id keeps the queue row's id and first_enqueued_at its
// created_at, so the DLQ entry remembers how long the cleanup was outstanding.
func (s *Store) MoveCleanupToDLQ(ctx context.Context, item *core.CleanupItem, lastError string) (bool, error) {
	match := claimArgs(item)
	n, err := s.moveRows(ctx, "move cleanup row to DLQ",
		`INSERT INTO cleanup_dlq (
			original_id, backend_name, object_key, storage_key, reason, size_bytes,
			attempts, first_enqueued_at, last_error
		 )
		 SELECT id, backend_name, object_key, storage_key, reason, size_bytes,
		        attempts, created_at, ?
		 FROM cleanup_queue
		 WHERE id = ? AND claimed_at = ? AND COALESCE(claimed_by, '') = ?`,
		append([]any{lastError}, match...),
		deleteClaimedCleanup,
		match,
	)
	return n > 0, err
}

// moveRows copies rows from one table to another and deletes them from the
// first in one transaction, for the queue and dead-letter moves. SQLite
// serializes writers, so the rows the insert read are the ones the delete
// removes. Returns how many rows moved; a move that copied nothing deletes
// nothing.
func (s *Store) moveRows(ctx context.Context, what, insertSQL string, insertArgs []any, deleteSQL string, deleteArgs []any) (int64, error) {
	var n int64
	err := cbWithTx(ctx, s.rawDB, s.cb, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, insertSQL, insertArgs...)
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		if n, err = res.RowsAffected(); err != nil || n == 0 {
			return err
		}
		if _, err := tx.ExecContext(ctx, deleteSQL, deleteArgs...); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// -------------------------------------------------------------------------
// DEPTHS AND DLQ
// -------------------------------------------------------------------------

// CleanupQueueDepth returns the number of items still pending in the queue,
// those below the attempt ceiling.
func (s *Store) CleanupQueueDepth(ctx context.Context) (int64, error) {
	return s.countRows(ctx, "pending cleanups",
		`SELECT COUNT(*) FROM cleanup_queue WHERE attempts < ?`, core.MaxCleanupAttempts)
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
	return s.moveRows(ctx, "requeue cleanup DLQ",
		`INSERT INTO cleanup_queue (backend_name, object_key, storage_key, reason, size_bytes, created_at, next_retry)
		 SELECT backend_name, object_key, storage_key, reason, size_bytes, ?, ?
		 FROM cleanup_dlq
		 WHERE (? = '' OR backend_name = ?)`,
		[]any{now, now, backend, backend},
		`DELETE FROM cleanup_dlq WHERE (? = '' OR backend_name = ?)`,
		[]any{backend, backend},
	)
}
