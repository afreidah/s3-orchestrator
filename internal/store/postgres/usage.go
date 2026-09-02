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

// FlushPoolDeltas adds one backend's accumulated per-pool request counts to
// their persistent rows.
//
// One statement per pool rather than one multi-row insert: a backend has a
// handful of pools, and writing them individually keeps a single unparseable
// pool name from failing the whole flush and sending every other pool's delta
// back into the counter.
func (s *Store) FlushPoolDeltas(ctx context.Context, backendName, period string, deltas core.PoolUsage) error {
	for pool, requests := range deltas {
		if requests == 0 {
			continue
		}
		err := s.queries.FlushPoolDelta(ctx, db.FlushPoolDeltaParams{
			BackendName: backendName,
			Period:      period,
			Pool:        pool,
			Requests:    requests,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// GetPoolUsageForPeriod returns every backend's per-pool request counts for
// the given period, keyed by backend name.
func (s *Store) GetPoolUsageForPeriod(ctx context.Context, period string) (map[string]core.PoolUsage, error) {
	rows, err := s.queries.GetPoolUsageForPeriod(ctx, period)
	if err != nil {
		return nil, err
	}

	usage := make(map[string]core.PoolUsage)
	for _, row := range rows {
		if usage[row.BackendName] == nil {
			usage[row.BackendName] = make(core.PoolUsage)
		}
		usage[row.BackendName][row.Pool] = row.Requests
	}
	return usage, nil
}
