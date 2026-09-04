// -------------------------------------------------------------------------------
// Quota Operations
//
// Author: Alex Freidah
//
// Implements the Postgres engine bindings for the backend_quotas table:
// per-backend bytes_used, bytes_limit, and orphan_bytes tracking. Carries
// the read-side eligibility queries the write path uses for backend
// routing (GetBackendWithSpace, GetLeastUtilizedBackend) and the per-tx
// increment / decrement primitives core/ uses to keep quota in lockstep
// with object_locations changes. Increment is guarded so the UPDATE
// touches zero rows when the limit would be exceeded, surfacing as
// ErrNoSpaceAvailable.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// -------------------------------------------------------------------------
// CONFIGURATION
// -------------------------------------------------------------------------

// SyncQuotaLimits ensures the backend_quotas table has entries for all configured
// backends with their quota limits. Creates new entries or updates existing limits.
// All updates happen in a single transaction for atomicity.
func (s *Store) SyncQuotaLimits(ctx context.Context, backends []config.BackendConfig) error {
	return s.withTx(ctx, func(qtx *db.Queries) error {
		for i := range backends {
			err := qtx.UpsertQuotaLimit(ctx, db.UpsertQuotaLimitParams{
				BackendName: backends[i].Name,
				BytesLimit:  backends[i].QuotaBytes,
			})
			if err != nil {
				return fmt.Errorf("failed to sync quota for backend %s: %w", backends[i].Name, err)
			}
		}
		return nil
	})
}

// -------------------------------------------------------------------------
// STATISTICS
// -------------------------------------------------------------------------

// GetQuotaStats returns quota statistics for all backends.
func (s *Store) GetQuotaStats(ctx context.Context) (map[string]core.QuotaStat, error) {
	rows, err := s.queries.GetAllQuotaStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query quota stats: %w", err)
	}

	stats := make(map[string]core.QuotaStat, len(rows))
	for _, row := range rows {
		stats[row.BackendName] = core.QuotaStat{
			BackendName: row.BackendName,
			BytesUsed:   row.BytesUsed,
			BytesLimit:  row.BytesLimit,
			OrphanBytes: row.OrphanBytes,
			UpdatedAt:   row.UpdatedAt.Time,
		}
	}

	return stats, nil
}

// ListBackendQuotaUsage returns each backend's ceiling and the byte totals a
// write is judged against, for the quota tracker's baseline refresh.
func (s *Store) ListBackendQuotaUsage(ctx context.Context) ([]core.BackendQuotaUsage, error) {
	rows, err := s.queries.ListBackendQuotaUsage(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query backend quota usage: %w", err)
	}

	usage := make([]core.BackendQuotaUsage, 0, len(rows))
	for _, row := range rows {
		usage = append(usage, core.BackendQuotaUsage{
			BackendName:   row.BackendName,
			BytesLimit:    row.BytesLimit,
			BytesUsed:     row.BytesUsed,
			OrphanBytes:   row.OrphanBytes,
			InflightBytes: row.InflightBytes,
		})
	}

	return usage, nil
}

// GetObjectCounts returns the number of objects stored on each backend.
func (s *Store) GetObjectCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.queries.GetObjectCountsByBackend(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query object counts: %w", err)
	}

	counts := make(map[string]int64, len(rows))
	for _, row := range rows {
		counts[row.BackendName] = row.ObjectCount
	}
	return counts, nil
}

// GetUnverifiedObjectCounts returns the number of objects per backend whose
// content_hash column is NULL (objects predating integrity verification
// or otherwise not yet checksummed). Drives the dashboard's "needs
// backfill" column.
func (s *Store) GetUnverifiedObjectCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.queries.GetUnverifiedObjectCountsByBackend(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query unverified object counts: %w", err)
	}
	counts := make(map[string]int64, len(rows))
	for _, row := range rows {
		counts[row.BackendName] = row.ObjectCount
	}
	return counts, nil
}

// GetActiveMultipartCounts returns the number of in-progress multipart uploads
// per backend.
func (s *Store) GetActiveMultipartCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.queries.GetActiveMultipartCountsByBackend(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query multipart counts: %w", err)
	}

	counts := make(map[string]int64, len(rows))
	for _, row := range rows {
		counts[row.BackendName] = row.UploadCount
	}
	return counts, nil
}
