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

// GetPendingCleanups returns cleanup items ready for retry.
func (s *Store) GetPendingCleanups(ctx context.Context, limit int) ([]core.CleanupItem, error) {
	rows, err := s.queries.GetPendingCleanups(ctx, int32(limit)) //nolint:gosec // G115: limit is a small caller-controlled batch size
	if err != nil {
		return nil, fmt.Errorf("failed to get pending cleanups: %w", err)
	}

	return mapSlice(rows, cleanupItemFromRow), nil
}

// cleanupItemFromRow converts a sqlc GetPendingCleanups row to the
// core.CleanupItem shape consumed by the cleanup worker.
func cleanupItemFromRow(r *db.GetPendingCleanupsRow) core.CleanupItem {
	return core.CleanupItem{
		ID:          r.ID,
		BackendName: r.BackendName,
		ObjectKey:   r.ObjectKey,
		Reason:      r.Reason,
		Attempts:    r.Attempts,
		SizeBytes:   r.SizeBytes,
	}
}

// CompleteCleanupItem removes a successfully processed item from the queue.
func (s *Store) CompleteCleanupItem(ctx context.Context, id int64) error {
	err := s.queries.DeleteCleanupItem(ctx, id)
	if err != nil {
		return fmt.Errorf("failed to complete cleanup item: %w", err)
	}
	return nil
}

// RetryCleanupItem increments the attempt counter and schedules the next retry.
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
