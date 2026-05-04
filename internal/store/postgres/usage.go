// -------------------------------------------------------------------------------
// Usage Tracking Operations
//
// Author: Alex Freidah
//
// Implements the Postgres engine bindings for backend_usage - the
// monthly per-backend (api_requests, egress_bytes, ingress_bytes)
// counters used by the usage flusher and the dashboard. FlushUsageDeltas
// uses INSERT ON CONFLICT DO UPDATE with a column-level ADD so multiple
// flushers can converge on the same row without losing deltas. Each row
// is keyed by (backend_name, period) where period is YYYY-MM.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// FlushUsageDeltas atomically adds accumulated usage deltas to the persistent
// usage row. Creates the row if it doesn't exist for this (backend, period).
func (s *Store) FlushUsageDeltas(ctx context.Context, backendName, period string, apiRequests, egressBytes, ingressBytes int64) error {
	return s.queries.FlushUsageDeltas(ctx, db.FlushUsageDeltasParams{
		BackendName:  backendName,
		Period:       period,
		ApiRequests:  apiRequests,
		EgressBytes:  egressBytes,
		IngressBytes: ingressBytes,
	})
}

// GetUsageForPeriod returns usage statistics for all backends in the given period.
func (s *Store) GetUsageForPeriod(ctx context.Context, period string) (map[string]core.UsageStat, error) {
	rows, err := s.queries.GetUsageForPeriod(ctx, period)
	if err != nil {
		return nil, err
	}

	stats := make(map[string]core.UsageStat, len(rows))
	for _, row := range rows {
		stats[row.BackendName] = core.UsageStat{
			APIRequests:  row.ApiRequests,
			EgressBytes:  row.EgressBytes,
			IngressBytes: row.IngressBytes,
		}
	}
	return stats, nil
}
