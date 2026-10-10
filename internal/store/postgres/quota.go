// -------------------------------------------------------------------------------
// Quota Operations
//
// Author: Alex Freidah
//
// Implements the Postgres engine bindings for backend quotas: per-backend
// limits, the striped byte counters, orphan bytes, and the usage listing the
// quota tracker refreshes its baselines from. Admission itself is tested
// against the backend_capacity view inside the statements that claim space.
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

// LedgerStats returns every per-backend ledger figure from one grouped pass
// over object_locations.
func (s *Store) LedgerStats(ctx context.Context) (core.LedgerStats, error) {
	rows, err := s.queries.LedgerStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query ledger stats: %w", err)
	}
	out := make(core.LedgerStats, len(rows))
	for i := range rows {
		r := &rows[i]
		out[r.BackendName] = core.LedgerStat{
			Objects:    r.Objects,
			Unhashed:   r.Unhashed,
			Plaintext:  r.Plaintext,
			Unreadable: r.Unreadable,
			Compressed: core.CompressionStat{
				Objects:      r.CompressedObjects,
				LogicalBytes: r.CompressedLogicalBytes,
				StoredBytes:  r.CompressedStoredBytes,
			},
			Verifiable:    r.Verifiable,
			NeverVerified: r.NeverVerified,
			OldestTouched: r.OldestTouched.Time,
		}
	}
	return out, nil
}

// GetActiveMultipartCounts returns the number of in-progress multipart uploads
// per backend.
func (s *Store) GetActiveMultipartCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.queries.GetActiveMultipartCountsByBackend(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query multipart counts: %w", err)
	}
	return totalsByBackend(rows, func(r db.GetActiveMultipartCountsByBackendRow) (string, int64) {
		return r.BackendName, r.UploadCount
	}), nil
}
