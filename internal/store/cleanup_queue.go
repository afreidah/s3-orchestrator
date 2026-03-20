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

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/afreidah/s3-orchestrator/internal/store/sqlc"
)

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
func (s *Store) GetPendingCleanups(ctx context.Context, limit int) ([]CleanupItem, error) {
	rows, err := s.queries.GetPendingCleanups(ctx, int32(limit))
	if err != nil {
		return nil, fmt.Errorf("failed to get pending cleanups: %w", err)
	}

	items := make([]CleanupItem, len(rows))
	for i, row := range rows {
		items[i] = CleanupItem{
			ID:          row.ID,
			BackendName: row.BackendName,
			ObjectKey:   row.ObjectKey,
			Reason:      row.Reason,
			Attempts:    row.Attempts,
			SizeBytes:   row.SizeBytes,
		}
	}
	return items, nil
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

// durationToInterval converts a Go time.Duration to a pgtype.Interval.
func durationToInterval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{
		Microseconds: d.Microseconds(),
		Valid:        true,
	}
}
