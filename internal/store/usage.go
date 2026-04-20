// -------------------------------------------------------------------------------
// Usage Tracking Operations
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package store

import (
	"context"

	db "github.com/afreidah/s3-orchestrator/internal/store/sqlc"
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
func (s *Store) GetUsageForPeriod(ctx context.Context, period string) (map[string]UsageStat, error) {
	rows, err := s.queries.GetUsageForPeriod(ctx, period)
	if err != nil {
		return nil, err
	}

	stats := make(map[string]UsageStat, len(rows))
	for _, row := range rows {
		stats[row.BackendName] = UsageStat{
			APIRequests:  row.ApiRequests,
			EgressBytes:  row.EgressBytes,
			IngressBytes: row.IngressBytes,
		}
	}
	return stats, nil
}
