// -------------------------------------------------------------------------------
// Usage Tracking Operations
//
// Author: Alex Freidah
//
// Implements the Postgres engine bindings for backend_usage and
// backend_request_usage - the per-backend request and byte totals and per-pool
// request counts used by the usage flusher, the limit checks and the
// dashboard. Each batch is added with INSERT ON CONFLICT DO UPDATE column-level
// ADDs, so several flushers converge on the same rows without losing deltas.
// Rows are keyed by backend, hourly bucket (UTC) and workload.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// ApplyUsageBatch adds each workload's totals and pools in a batch to its
// buckets' rows in one transaction, then drops the backend's buckets older
// than core.UsageRetention. A batch with an ID records it first, and one
// already recorded is left alone; see core.UsageFlusher.
func (s *Store) ApplyUsageBatch(ctx context.Context, b *core.UsageBatch) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		if fresh, err := recordUsageFlush(ctx, q, b.ID); err != nil || !fresh {
			return err
		}
		if err := b.Each(func(bucket time.Time, workload string, u *core.Usage) error {
			return applyWorkloadUsage(ctx, q, b.Backend, pgTimestamptz(bucket), workload, u)
		}); err != nil {
			return err
		}
		return pruneUsageBuckets(ctx, q, b.Backend)
	})
}

// applyWorkloadUsage adds one workload's totals and pools to its bucket's
// rows, inside the batch's transaction.
func applyWorkloadUsage(ctx context.Context, q *db.Queries, backend string, bucket pgtype.Timestamptz, workload string, u *core.Usage) error {
	if err := q.FlushUsageDeltas(ctx, db.FlushUsageDeltasParams{
		BackendName:  backend,
		BucketStart:  bucket,
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
			BackendName: backend,
			BucketStart: bucket,
			Workload:    workload,
			Pool:        pool,
			Requests:    requests,
		}); err != nil {
			return err
		}
	}
	return nil
}

// pruneUsageBuckets drops one backend's buckets older than the retention
// window, inside the flush's transaction.
func pruneUsageBuckets(ctx context.Context, q *db.Queries, backend string) error {
	cutoff := pgTimestamptz(time.Now().Add(-core.UsageRetention))
	if err := q.PruneUsageBuckets(ctx, db.PruneUsageBucketsParams{BackendName: backend, Cutoff: cutoff}); err != nil {
		return err
	}
	return q.PrunePoolUsageBuckets(ctx, db.PrunePoolUsageBucketsParams{BackendName: backend, Cutoff: cutoff})
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

// GetUsageSince sums each backend's buckets from its own start onward, by
// workload, from one read of the totals joined to their pool sums; see
// core.UsageReader.
func (s *Store) GetUsageSince(ctx context.Context, starts map[string]time.Time) (map[string]core.WorkloadUsage, error) {
	params := db.GetUsageSinceParams{
		Backends: make([]string, 0, len(starts)),
		Starts:   make([]pgtype.Timestamptz, 0, len(starts)),
	}
	for backend, start := range starts {
		params.Backends = append(params.Backends, backend)
		params.Starts = append(params.Starts, pgTimestamptz(start))
	}
	rows, err := s.queries.GetUsageSince(ctx, params)
	if err != nil {
		return nil, err
	}

	usage := make(map[string]core.WorkloadUsage)
	for _, row := range rows {
		stat := core.UsageStat{APIRequests: row.ApiRequests, EgressBytes: row.EgressBytes, IngressBytes: row.IngressBytes}
		core.AddUsageRow(usage, row.BackendName, row.Workload, stat, row.Pool, row.Requests)
	}
	return usage, nil
}
