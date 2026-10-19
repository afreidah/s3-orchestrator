// -------------------------------------------------------------------------------
// Cleanup Queue Store - Database Operations
//
// Author: Alex Freidah
//
// Provides MetadataStore methods for the cleanup queue: claim queued rows with
// backoff-aware scheduling, retire them once their bytes are gone, and update
// attempt counts for a retry. Rows are queued by the transactions that stop
// recording bytes, through the tx adapter.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// -------------------------------------------------------------------------
// QUEUE LIFECYCLE
// -------------------------------------------------------------------------

// GetPendingCleanups returns a read-only snapshot of pending cleanup rows.
// Used by the admin endpoint to render the queue. The cleanup worker uses
// ClaimPendingCleanups instead, which atomically stamps claim columns.
func (s *Store) GetPendingCleanups(ctx context.Context, limit int) ([]core.CleanupItem, error) {
	rows, err := s.queries.GetPendingCleanups(ctx, db.GetPendingCleanupsParams{
		MaxAttempts: core.MaxCleanupAttempts,
		RowLimit:    int32(limit), //nolint:gosec // G115: limit is a small caller-controlled batch size
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get pending cleanups: %w", err)
	}
	return mapSlice(rows, cleanupItemFromRow), nil
}

// cleanupItemFromRow converts a cleanup queue row to the core.CleanupItem shape.
// ClaimPendingCleanups selects the same columns, so its rows convert here too.
func cleanupItemFromRow(r *db.GetPendingCleanupsRow) core.CleanupItem {
	item := core.CleanupItem{
		ID:          r.ID,
		BackendName: r.BackendName,
		ObjectKey:   r.ObjectKey,
		StorageKey:  r.StorageKey,
		Reason:      r.Reason,
		Attempts:    r.Attempts,
		SizeBytes:   r.SizeBytes,
		Reclaimed:   r.Reclaimed,
	}
	if r.ClaimedAt.Valid {
		item.Claim = &core.CleanupClaim{At: r.ClaimedAt.Time, By: derefStr(r.ClaimedBy)}
	}
	return item
}

// ClaimPendingCleanups atomically reserves a batch of cleanup rows for the
// calling instance using FOR UPDATE SKIP LOCKED, returning each with the claim
// it stamped. See the SQL definition for the eligibility rules; this method is
// the only path the cleanup worker should use to fetch pending rows.
func (s *Store) ClaimPendingCleanups(ctx context.Context, limit int, instanceID string, graceCutoff time.Time) ([]core.CleanupItem, error) {
	rows, err := s.queries.ClaimPendingCleanups(ctx, db.ClaimPendingCleanupsParams{
		MaxAttempts: core.MaxCleanupAttempts,
		GraceCutoff: pgtype.Timestamptz{Time: graceCutoff, Valid: true},
		RowLimit:    int32(limit), //nolint:gosec // G115: limit is a small caller-controlled batch size
		ClaimedBy:   instanceID,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to claim pending cleanups: %w", err)
	}
	return mapSlice(rows, func(r *db.ClaimPendingCleanupsRow) core.CleanupItem {
		return cleanupItemFromRow((*db.GetPendingCleanupsRow)(r))
	}), nil
}

// CompleteCleanupItems retires the rows whose bytes are gone in one statement,
// each only while it is still held under its claim, and returns the ids it
// retired.
func (s *Store) CompleteCleanupItems(ctx context.Context, items []core.CleanupItem) ([]int64, error) {
	if len(items) == 0 {
		return nil, nil
	}
	params := db.CompleteCleanupItemsParams{
		Ids:        make([]int64, len(items)),
		ClaimedAts: make([]pgtype.Timestamptz, len(items)),
		ClaimedBys: make([]string, len(items)),
	}
	for i := range items {
		claim := claimOf(&items[i])
		params.Ids[i] = items[i].ID
		params.ClaimedAts[i] = pgtype.Timestamptz{Time: claim.At, Valid: true}
		params.ClaimedBys[i] = claim.By
	}
	retired, err := s.queries.CompleteCleanupItems(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("complete cleanup items: %w", err)
	}
	return retired, nil
}

// RetryCleanupItem increments the attempt counter, schedules the next retry,
// and clears the claim so the row is immediately re-eligible for the next
// worker tick. Reports false when the item's claim no longer holds the row.
func (s *Store) RetryCleanupItem(ctx context.Context, item *core.CleanupItem, backoff time.Duration, lastError string) (bool, error) {
	claim := claimOf(item)
	n, err := s.queries.UpdateCleanupRetry(ctx, db.UpdateCleanupRetryParams{
		Backoff:   durationToInterval(backoff),
		LastError: &lastError,
		ID:        item.ID,
		ClaimedAt: pgtype.Timestamptz{Time: claim.At, Valid: true},
		ClaimedBy: claim.By,
	})
	return changed(n, err, "update cleanup retry")
}

// MoveCleanupToDLQ graduates an exhausted row to cleanup_dlq in one statement.
// Reports false when the item's claim no longer holds the row.
func (s *Store) MoveCleanupToDLQ(ctx context.Context, item *core.CleanupItem, lastError string) (bool, error) {
	claim := claimOf(item)
	n, err := s.queries.MoveCleanupToDLQ(ctx, db.MoveCleanupToDLQParams{
		LastError: lastError,
		ID:        item.ID,
		ClaimedAt: pgtype.Timestamptz{Time: claim.At, Valid: true},
		ClaimedBy: claim.By,
	})
	return changed(n, err, "move cleanup row to DLQ")
}

// claimOf is the claim a settle write matches. An item with no claim matches
// no row, since every row a settle reaches was claimed when it was read.
func claimOf(item *core.CleanupItem) core.CleanupClaim {
	if item.Claim == nil {
		return core.CleanupClaim{}
	}
	return *item.Claim
}

// CleanupQueueDepth returns the number of items still pending in the queue.
func (s *Store) CleanupQueueDepth(ctx context.Context) (int64, error) {
	count, err := s.queries.CountPendingCleanups(ctx, core.MaxCleanupAttempts)
	if err != nil {
		return 0, fmt.Errorf("failed to count pending cleanups: %w", err)
	}
	return count, nil
}

// -------------------------------------------------------------------------
// DEAD-LETTER OPS
// -------------------------------------------------------------------------

// CleanupDLQDepth returns the number of rows currently in cleanup_dlq.
// Surfaces the count of unrecoverable orphans so the dashboard and the
// cleanup_dlq_depth gauge can flag operator-visible work.
func (s *Store) CleanupDLQDepth(ctx context.Context) (int64, error) {
	count, err := s.queries.CountCleanupDLQ(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to count cleanup DLQ rows: %w", err)
	}
	return count, nil
}

// -------------------------------------------------------------------------
// SWEEPS
// -------------------------------------------------------------------------

// SweepStaleCleanupQueueRows drops the queued cleanups for a path reconcile
// found missing from the backend, and returns how many went.
func (s *Store) SweepStaleCleanupQueueRows(ctx context.Context, storageKey, backend string) (int64, error) {
	n, err := s.queries.DeleteCleanupQueueByKey(ctx, db.DeleteCleanupQueueByKeyParams{
		StorageKey:  storageKey,
		BackendName: backend,
	})
	if err != nil {
		return 0, fmt.Errorf("sweep stale cleanup rows: %w", err)
	}
	return n, nil
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// durationToInterval converts a Go time.Duration to a pgtype.Interval.
func durationToInterval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{
		Microseconds: d.Microseconds(),
		Valid:        true,
	}
}

// ListCleanupDLQ returns dead-lettered cleanup rows for operator inspection,
// newest graduation first. An empty backend lists every backend.
func (s *Store) ListCleanupDLQ(ctx context.Context, backend string, limit int) ([]core.CleanupDLQItem, error) {
	// Convert only inside an in-range guard so the int->int32 narrowing is
	// provably overflow-free; the admin handler already caps limit, this is
	// defence in depth.
	rowLimit := int32(math.MaxInt32)
	if limit >= 0 && limit <= math.MaxInt32 {
		rowLimit = int32(limit)
	}
	rows, err := s.queries.ListCleanupDLQ(ctx, db.ListCleanupDLQParams{
		Backend:  backend,
		RowLimit: rowLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("list cleanup dlq: %w", err)
	}
	return mapSlice(rows, cleanupDLQItemFromRow), nil
}

// cleanupDLQItemFromRow maps a sqlc DLQ row onto the core domain type,
// unwrapping the nullable last_error and timestamptz columns.
func cleanupDLQItemFromRow(r *db.ListCleanupDLQRow) core.CleanupDLQItem {
	return core.CleanupDLQItem{
		BackendName:   r.BackendName,
		ObjectKey:     r.ObjectKey,
		StorageKey:    r.StorageKey,
		Reason:        r.Reason,
		SizeBytes:     r.SizeBytes,
		Attempts:      r.Attempts,
		FirstEnqueued: r.FirstEnqueuedAt.Time,
		MovedAt:       r.MovedAt.Time,
		LastError:     derefStr(r.LastError),
	}
}

// RequeueCleanupDLQ moves dead-lettered rows back into cleanup_queue via the
// writable-CTE query so the move is atomic. Returns the number of rows
// requeued.
func (s *Store) RequeueCleanupDLQ(ctx context.Context, backend string) (int64, error) {
	n, err := s.queries.RequeueCleanupDLQ(ctx, backend)
	if err != nil {
		return 0, fmt.Errorf("requeue cleanup dlq: %w", err)
	}
	return n, nil
}
