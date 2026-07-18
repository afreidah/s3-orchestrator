// -------------------------------------------------------------------------------
// Cleanup Queue Store - Database Operations
//
// Author: Alex Freidah
//
// Provides MetadataStore methods for the cleanup retry queue: enqueue failed
// operations, fetch pending items with backoff-aware scheduling, and update
// attempt counts or mark items as completed. Also manages orphan_bytes tracking
// on backend_quotas for bytes pending physical deletion.
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

// EnqueueCleanup adds a failed cleanup operation to the retry queue.
func (s *Store) EnqueueCleanup(ctx context.Context, backendName, objectKey, reason string, sizeBytes int64) error {
	err := s.queries.EnqueueCleanup(ctx, db.EnqueueCleanupParams{
		BackendName: backendName,
		ObjectKey:   objectKey,
		Reason:      reason,
		SizeBytes:   sizeBytes,
	})
	if err != nil {
		return fmt.Errorf("failed to enqueue cleanup: %w", err)
	}
	return nil
}

// GetPendingCleanups returns a read-only snapshot of pending cleanup rows.
// Used by the admin endpoint to render the queue. The cleanup worker uses
// ClaimPendingCleanups instead, which atomically stamps claim columns.
func (s *Store) GetPendingCleanups(ctx context.Context, limit int) ([]core.CleanupItem, error) {
	rows, err := s.queries.GetPendingCleanups(ctx, int32(limit)) //nolint:gosec // G115: limit is a small caller-controlled batch size
	if err != nil {
		return nil, fmt.Errorf("failed to get pending cleanups: %w", err)
	}

	return mapSlice(rows, cleanupItemFromRow), nil
}

// cleanupItemFromRow converts a sqlc GetPendingCleanups row to the
// core.CleanupItem shape consumed by the admin endpoint.
func cleanupItemFromRow(r *db.GetPendingCleanupsRow) core.CleanupItem {
	return core.CleanupItem{
		ID:          r.ID,
		BackendName: r.BackendName,
		ObjectKey:   r.ObjectKey,
		Reason:      r.Reason,
		Attempts:    r.Attempts,
		SizeBytes:   r.SizeBytes,
		ClaimedAt:   timestamptzPtr(r.ClaimedAt),
		ClaimedBy:   r.ClaimedBy,
	}
}

// claimedItemFromRow converts a sqlc ClaimPendingCleanups row to the
// core.CleanupItem shape, carrying the reclaimed flag the worker uses to
// drive the stale-claim recovery metric.
func claimedItemFromRow(r *db.ClaimPendingCleanupsRow) core.CleanupItem {
	return core.CleanupItem{
		ID:          r.ID,
		BackendName: r.BackendName,
		ObjectKey:   r.ObjectKey,
		Reason:      r.Reason,
		Attempts:    r.Attempts,
		SizeBytes:   r.SizeBytes,
		Reclaimed:   r.Reclaimed,
	}
}

// ClaimPendingCleanups atomically reserves a batch of cleanup rows for the
// calling instance using FOR UPDATE SKIP LOCKED. See the SQL definition for
// the eligibility rules; this method is the only path the cleanup worker
// should use to fetch pending rows.
func (s *Store) ClaimPendingCleanups(ctx context.Context, limit int, instanceID string, graceCutoff time.Time) ([]core.CleanupItem, error) {
	rows, err := s.queries.ClaimPendingCleanups(ctx, db.ClaimPendingCleanupsParams{
		Limit:       int32(limit), //nolint:gosec // G115: limit is a small caller-controlled batch size
		GraceCutoff: pgtype.Timestamptz{Time: graceCutoff, Valid: true},
		ClaimedBy:   instanceID,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to claim pending cleanups: %w", err)
	}
	return mapSlice(rows, claimedItemFromRow), nil
}

// CompleteCleanupItem atomically deletes a successfully-processed row and
// decrements the backing backend's orphan_bytes by the row's size. The
// underlying SQL is a single CTE so a worker crash between the delete and
// the decrement cannot leave the counter inconsistent; idempotent against
// re-claim retries because the CTE is empty when the row is already gone.
func (s *Store) CompleteCleanupItem(ctx context.Context, id int64) error {
	if err := s.queries.CompleteCleanupItem(ctx, id); err != nil {
		return fmt.Errorf("failed to complete cleanup item: %w", err)
	}
	return nil
}

// RetryCleanupItem increments the attempt counter, schedules the next retry,
// and clears the claim so the row is immediately re-eligible for the next
// worker tick.
func (s *Store) RetryCleanupItem(ctx context.Context, id int64, backoff time.Duration, lastError string) error {
	err := s.queries.UpdateCleanupRetry(ctx, db.UpdateCleanupRetryParams{
		Backoff:   durationToInterval(backoff),
		LastError: &lastError,
		ID:        id,
	})
	if err != nil {
		return fmt.Errorf("failed to update cleanup retry: %w", err)
	}
	return nil
}

// timestamptzPtr returns a *time.Time for a sqlc-emitted pgtype.Timestamptz,
// or nil when the column was NULL.
func timestamptzPtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}

// CleanupQueueDepth returns the number of items still pending in the queue.
func (s *Store) CleanupQueueDepth(ctx context.Context) (int64, error) {
	count, err := s.queries.CountPendingCleanups(ctx)
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

// MoveCleanupToDLQ atomically graduates an exhausted cleanup_queue row
// to the dead-letter table. Delegates to core.MoveCleanupToDLQ so both
// engines share the move semantics - notably that orphan_bytes is left
// untouched because the backend object is still on disk.
func (s *Store) MoveCleanupToDLQ(ctx context.Context, id int64, lastError string) (bool, error) {
	return core.MoveCleanupToDLQ(ctx, s, id, lastError)
}

// -------------------------------------------------------------------------
// ORPHAN BYTES AND SWEEPS
// -------------------------------------------------------------------------

// IncrementOrphanBytes adds bytes to the orphan_bytes counter for a backend.
// Called when a physical delete fails and is enqueued for retry.
func (s *Store) IncrementOrphanBytes(ctx context.Context, backendName string, amount int64) error {
	err := s.queries.IncrementOrphanBytes(ctx, db.IncrementOrphanBytesParams{
		Amount:      amount,
		BackendName: backendName,
	})
	if err != nil {
		return fmt.Errorf("failed to increment orphan bytes: %w", err)
	}
	return nil
}

// DecrementOrphanBytes subtracts bytes from the orphan_bytes counter for a
// backend. Called when a cleanup queue item is successfully processed or
// exhausted (written off).
func (s *Store) DecrementOrphanBytes(ctx context.Context, backendName string, amount int64) error {
	err := s.queries.DecrementOrphanBytes(ctx, db.DecrementOrphanBytesParams{
		Amount:      amount,
		BackendName: backendName,
	})
	if err != nil {
		return fmt.Errorf("failed to decrement orphan bytes: %w", err)
	}
	return nil
}

// SweepStaleCleanupQueueRows removes every cleanup_queue row matching
// the (object_key, backend_name) pair and decrements the backend's
// orphan_bytes counter by the sum of their size_bytes. Delegates to
// core.SweepStaleCleanupQueueRows.
func (s *Store) SweepStaleCleanupQueueRows(ctx context.Context, key, backend string) (int64, error) {
	return core.SweepStaleCleanupQueueRows(ctx, s, key, backend)
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
