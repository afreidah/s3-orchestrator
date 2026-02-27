// -------------------------------------------------------------------------------
// Cleanup Queue Store - Database Operations
//
// Author: Alex Freidah
//
// Provides MetadataStore methods for the cleanup retry queue: enqueue failed
// operations, fetch pending items with backoff-aware scheduling, and update
// attempt counts or mark items as completed.
// -------------------------------------------------------------------------------

package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/afreidah/s3-orchestrator/internal/storage/sqlc"
)

// EnqueueCleanup adds a failed cleanup operation to the retry queue.
func (s *Store) EnqueueCleanup(ctx context.Context, backendName, objectKey, reason string) error {
	err := s.queries.EnqueueCleanup(ctx, db.EnqueueCleanupParams{
		BackendName: backendName,
		ObjectKey:   objectKey,
		Reason:      reason,
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

// durationToInterval converts a Go time.Duration to a pgtype.Interval.
func durationToInterval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{
		Microseconds: d.Microseconds(),
		Valid:        true,
	}
}
