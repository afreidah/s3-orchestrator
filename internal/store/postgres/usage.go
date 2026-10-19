// -------------------------------------------------------------------------------
// Usage Tracking Operations
//
// Author: Alex Freidah
//
// Implements the Postgres engine bindings for backend_usage and
// backend_request_usage - the monthly per-backend request and byte totals and
// per-pool request counts used by the usage flusher, the limit checks and the
// dashboard. Each batch is added with INSERT ON CONFLICT DO UPDATE column-level
// ADDs, so several flushers converge on the same rows without losing deltas.
// Rows are keyed by backend and period, where period is YYYY-MM.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// ApplyUsageBatch adds a batch's totals and pools to its period's rows in one
// transaction. A batch with an ID records it first, and one already recorded
// is left alone; see core.UsageFlusher.
func (s *Store) ApplyUsageBatch(ctx context.Context, b *core.UsageBatch) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		if fresh, err := recordUsageFlush(ctx, q, b.ID); err != nil || !fresh {
			return err
		}
		if err := q.FlushUsageDeltas(ctx, db.FlushUsageDeltasParams{
			BackendName:  b.Backend,
			Period:       b.Period,
			ApiRequests:  b.APIRequests,
			EgressBytes:  b.EgressBytes,
			IngressBytes: b.IngressBytes,
		}); err != nil {
			return err
		}
		for pool, requests := range b.Pools {
			if requests == 0 {
				continue
			}
			if err := q.FlushPoolDelta(ctx, db.FlushPoolDeltaParams{
				BackendName: b.Backend,
				Period:      b.Period,
				Pool:        pool,
				Requests:    requests,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// recordUsageFlush records a shared batch's id and prunes old ones, reporting
// whether the batch is new and should be applied. A batch with no id is
// always new.
func recordUsageFlush(ctx context.Context, q *db.Queries, id string) (bool, error) {
	if id == "" {
		return true, nil
	}
	fresh, err := q.RecordUsageFlush(ctx, id)
	if err != nil || fresh == 0 {
		return false, err
	}
	return true, q.PruneUsageFlushes(ctx)
}

// GetUsageForPeriod returns every backend's totals and pool counts for the
// given period, keyed by backend name, from one read of the totals joined to
// their pool rows.
func (s *Store) GetUsageForPeriod(ctx context.Context, period string) (map[string]core.Usage, error) {
	rows, err := s.queries.GetUsageForPeriod(ctx, period)
	if err != nil {
		return nil, err
	}

	usage := make(map[string]core.Usage, len(rows))
	for _, row := range rows {
		u := usage[row.BackendName]
		u.UsageStat = core.UsageStat{
			APIRequests:  row.ApiRequests,
			EgressBytes:  row.EgressBytes,
			IngressBytes: row.IngressBytes,
		}
		if row.Pool != nil && row.Requests != nil {
			if u.Pools == nil {
				u.Pools = make(core.PoolUsage)
			}
			u.Pools[*row.Pool] = *row.Requests
		}
		usage[row.BackendName] = u
	}
	return usage, nil
}
