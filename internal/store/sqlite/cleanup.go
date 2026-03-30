// Package sqlite implements cleanup queue operations for the SQLite backend:
// enqueue failed deletions, fetch pending items with backoff-aware scheduling,
// and mark items as completed or retried.
package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store"
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

// GetPendingCleanups returns cleanup items ready for retry (next_retry in the
// past and fewer than 10 attempts).
func (s *Store) GetPendingCleanups(ctx context.Context, limit int) ([]store.CleanupItem, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, backend_name, object_key, reason, attempts, size_bytes
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

	var items []store.CleanupItem
	for rows.Next() {
		var item store.CleanupItem
		if err := rows.Scan(&item.ID, &item.BackendName, &item.ObjectKey, &item.Reason, &item.Attempts, &item.SizeBytes); err != nil {
			return nil, fmt.Errorf("failed to scan cleanup item: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// CompleteCleanupItem removes a successfully processed item from the queue.
func (s *Store) CompleteCleanupItem(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM cleanup_queue WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("failed to complete cleanup item: %w", err)
	}
	return nil
}

// RetryCleanupItem increments the attempt counter and schedules the next retry
// at now + backoff.
func (s *Store) RetryCleanupItem(ctx context.Context, id int64, backoff time.Duration, lastError string) error {
	nextRetry := time.Now().Add(backoff).UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx,
		`UPDATE cleanup_queue
		 SET attempts = attempts + 1,
		     next_retry = ?,
		     last_error = ?
		 WHERE id = ?`,
		nextRetry, lastError, id,
	)
	if err != nil {
		return fmt.Errorf("failed to update cleanup retry: %w", err)
	}
	return nil
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
