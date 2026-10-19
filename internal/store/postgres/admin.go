// -------------------------------------------------------------------------------
// Admin and Backend Lifecycle Operations
//
// Author: Alex Freidah
//
// Implements the Postgres engine bindings for backend-lifecycle ops:
// adding a backend to backend_quotas at startup (UpsertQuotaLimit),
// listing backends, and deleting all metadata for a removed backend
// (DeleteBackendData). Translates between sqlc-generated row types
// and the canonical core domain types so business logic in core/
// stays engine-agnostic.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"
	"fmt"
)

// -------------------------------------------------------------------------
// BACKEND LIFECYCLE
// -------------------------------------------------------------------------

// BackendObjectStats returns the object count and total bytes stored on a backend.
func (s *Store) BackendObjectStats(ctx context.Context, backendName string) (int64, int64, error) {
	row, err := s.queries.BackendObjectStats(ctx, backendName)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get backend object stats: %w", err)
	}
	return row.ObjectCount, row.TotalBytes, nil
}

// DeleteBackendData removes all database records for a backend in FK-safe order.
// Runs in a single transaction.
func (s *Store) DeleteBackendData(ctx context.Context, backendName string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	qtx := s.queries.WithTx(tx)
	steps := []struct {
		what string
		run  func(context.Context, string) error
	}{
		{"cleanup queue", qtx.DeleteCleanupQueueByBackend},
		{"cleanup DLQ", qtx.DeleteCleanupDLQByBackend},
		{"pending objects", qtx.DeletePendingObjectsByBackend},
		{"multipart uploads", qtx.DeleteMultipartUploadsByBackend},
		{"object locations", qtx.DeleteObjectLocationsByBackend},
		{"usage records", qtx.DeleteUsageByBackend},
		{"pool usage records", qtx.DeletePoolUsageByBackend},
		{"quota", qtx.DeleteQuota},
	}
	for _, step := range steps {
		if err := step.run(ctx, backendName); err != nil {
			return fmt.Errorf("failed to delete %s: %w", step.what, err)
		}
	}

	return tx.Commit(ctx)
}
