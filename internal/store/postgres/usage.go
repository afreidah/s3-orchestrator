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
// Rows are keyed by backend, period (YYYY-MM) and workload.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// ApplyUsageBatch adds each workload's totals and pools in a batch to its
// period's rows in one transaction. A batch with an ID records it first, and
// one already recorded is left alone; see core.UsageFlusher.
func (s *Store) ApplyUsageBatch(ctx context.Context, b *core.UsageBatch) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		if fresh, err := recordUsageFlush(ctx, q, b.ID); err != nil || !fresh {
			return err
		}
		for workload, u := range b.Workloads {
			if err := applyWorkloadUsage(ctx, q, b, workload, &u); err != nil {
				return err
			}
		}
		return nil
	})
}

// applyWorkloadUsage adds one workload's totals and pools to its rows, inside
// the batch's transaction.
func applyWorkloadUsage(ctx context.Context, q *db.Queries, b *core.UsageBatch, workload string, u *core.Usage) error {
	if err := q.FlushUsageDeltas(ctx, db.FlushUsageDeltasParams{
		BackendName:  b.Backend,
		Period:       b.Period,
		Workload:     workload,
		ApiRequests:  u.APIRequests,
		EgressBytes:  u.EgressBytes,
		IngressBytes: u.IngressBytes,
	}); err != nil {
		return err
	}
	for pool, requests := range u.Pools {
		if requests == 0 {
			continue
		}
		if err := q.FlushPoolDelta(ctx, db.FlushPoolDeltaParams{
			BackendName: b.Backend,
			Period:      b.Period,
			Workload:    workload,
			Pool:        pool,
			Requests:    requests,
		}); err != nil {
			return err
		}
	}
	return nil
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

// GetUsageForPeriod returns every backend's totals and pool counts by workload
// for the given period, keyed by backend name, from one read of the totals
// joined to their pool rows.
func (s *Store) GetUsageForPeriod(ctx context.Context, period string) (map[string]core.WorkloadUsage, error) {
	rows, err := s.queries.GetUsageForPeriod(ctx, period)
	if err != nil {
		return nil, err
	}

	usage := make(map[string]core.WorkloadUsage)
	for _, row := range rows {
		if usage[row.BackendName] == nil {
			usage[row.BackendName] = make(core.WorkloadUsage)
		}
		u := usage[row.BackendName][row.Workload]
		u.UsageStat = core.UsageStat{
			APIRequests:  row.ApiRequests,
			EgressBytes:  row.EgressBytes,
			IngressBytes: row.IngressBytes,
		}
		if row.Pool != nil && row.Requests != nil {
			u.AddPool(*row.Pool, *row.Requests)
		}
		usage[row.BackendName][row.Workload] = u
	}
	return usage, nil
}
