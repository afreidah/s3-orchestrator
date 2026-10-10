// -------------------------------------------------------------------------------
// Over-Replication Cleaner - Background Excess Copy Removal
//
// Author: Alex Freidah
//
// Removes surplus copies of objects that exceed the configured replication
// factor. Over-replication occurs when a backend recovers after the replicator
// has already created replacement copies elsewhere. Each object's copies are
// scored by backend health and utilization; the lowest-scoring copies are
// removed until the object reaches the target factor. Uses FOR UPDATE locking
// per object to prevent races with concurrent replicator/rebalancer activity.
// -------------------------------------------------------------------------------

package worker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// -------------------------------------------------------------------------
// OVER-REPLICATION CLEANER TYPE
// -------------------------------------------------------------------------

// OverReplicationCleanerStore is the narrow persistence surface the
// over-replication cleaner needs: over-replicated discovery + excess copy
// removal + per-backend quota stats for the deletion target. Declared
// locally so the worker does not pull in the full MetadataStore.
type OverReplicationCleanerStore interface {
	core.ReplicationStore
	core.QuotaStore
}

// OverReplicationCleaner removes excess copies of objects that exceed the
// configured replication factor.
type OverReplicationCleaner struct {
	log       *slog.Logger
	ops       Ops
	placement Placement
	store     OverReplicationCleanerStore
	cfg       syncutil.AtomicConfig[config.ReplicationConfig]
}

// NewOverReplicationCleaner creates a cleaner with fleet operations, write-path
// placement, and a metadata store.
func NewOverReplicationCleaner(ops Ops, placement Placement, store OverReplicationCleanerStore) *OverReplicationCleaner {
	must.NotNil("ops", ops)
	must.NotNil("placement", placement)
	must.NotNil("store", store)
	return &OverReplicationCleaner{ops: ops, placement: placement, store: store, log: slog.Default().With(logfmt.Component("over_replication"))}
}

// SetConfig atomically stores the replication configuration.
func (c *OverReplicationCleaner) SetConfig(cfg *config.ReplicationConfig) {
	c.cfg.Store(cfg)
}

// Config returns the current replication configuration.
func (c *OverReplicationCleaner) Config() *config.ReplicationConfig {
	return c.cfg.Load()
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Clean finds over-replicated objects and removes excess copies to reach the
// target replication factor. Returns the number of copies removed. observer,
// when non-nil, receives a start and end step per object cleaned.
func (c *OverReplicationCleaner) Clean(ctx context.Context, cfg config.ReplicationConfig, observer progress.Observer) (OverReplicationSummary, error) {
	return runOpsCycle(ctx, "OverReplicationClean", "over_replication_clean", func(ctx context.Context) (OverReplicationSummary, error) {
		return c.clean(ctx, cfg, observer)
	})
}

// OverReplicationSummary is the outcome of one cleanup cycle: the per-item
// tally every worker reports, plus the copies those items removed. One
// over-replicated object can carry several surplus copies, so CopiesRemoved is
// not the number of items that succeeded.
type OverReplicationSummary struct {
	batch.Summary
	CopiesRemoved int
}

// cleanOutcome captures everything the post-cycle reporter needs so the
// body of clean stays focused on the cleanup work itself.
type cleanOutcome struct {
	summary        OverReplicationSummary
	objectsChecked int
	err            error
}

// clean is the body of Clean after observe.Run sets up the span. All
// per-cycle telemetry and audit emission lives in reportCleanCycle so
// the work below reads as straight business logic.
func (c *OverReplicationCleaner) clean(ctx context.Context, cfg config.ReplicationConfig, observer progress.Observer) (OverReplicationSummary, error) {
	if cfg.Factor <= 1 {
		return OverReplicationSummary{}, nil
	}

	start := time.Now()
	var out cleanOutcome
	defer func() { c.reportCleanCycle(ctx, start, &out) }()

	audit.Log(ctx, "over_replication.start",
		slog.Int("factor", cfg.Factor),
		slog.Int("batch_size", cfg.BatchSize),
	)

	locations, err := c.store.GetOverReplicatedObjects(ctx, cfg.Factor, cfg.BatchSize)
	if err != nil {
		out.err = fmt.Errorf("failed to query over-replicated objects: %w", err)
		return OverReplicationSummary{}, out.err
	}
	if len(locations) == 0 {
		return OverReplicationSummary{}, nil
	}

	// Pre-fetch quota stats for copy scoring (utilization ratio).
	quotaStats, qErr := c.store.GetQuotaStats(ctx)
	if qErr != nil {
		c.log.WarnContext(ctx, "failed to get quota stats, scoring without utilization",
			"error", qErr)
	}

	grouped := core.GroupByKey(locations)
	out.objectsChecked = len(grouped)

	type cleanupTask struct {
		key    string
		copies []core.ObjectLocation
		excess int
	}
	var tasks []cleanupTask
	for key, copies := range grouped {
		excess := len(copies) - cfg.Factor
		if excess > 0 {
			tasks = append(tasks, cleanupTask{key: key, copies: copies, excess: excess})
		}
	}

	// The pending gauge is the fleet snapshot's ledger count, which every
	// instance serves; this pass's task list is one batch of it.
	var removed atomic.Int64
	runner := batch.Runner[cleanupTask]{
		Name:        "over_replication",
		Log:         c.log,
		Concurrency: cfg.Concurrency,
		Observer:    observer,
		Key:         func(t cleanupTask) string { return t.key },
	}
	sum := runner.Run(ctx, tasks, func(ctx context.Context, task cleanupTask) batch.ItemResult {
		var res batch.ItemResult // zero value (batch.ItemSkipped) when admission blocks the work
		WithAdmission(ctx, c.ops, WorkerNameOverReplication, func() {
			n, failures := c.cleanObject(ctx, task.key, task.copies, task.excess, cfg.Factor, quotaStats)
			removed.Add(int64(n))
			res = cleanupItemResult(n, failures)
		})
		return res
	})

	out.summary = OverReplicationSummary{Summary: sum, CopiesRemoved: int(removed.Load())}
	return out.summary, nil
}

// cleanupItemResult folds one object's surplus-copy removals into the batch
// tally. An object whose removals all failed is a failed item; one that gave
// up nothing without erroring was a benign race (a parallel delete or an
// earlier tick already absorbed the excess) and is skipped, not failed.
func cleanupItemResult(removed, failures int) batch.ItemResult {
	switch {
	case removed > 0:
		return batch.ItemResult{Outcome: batch.ItemSucceeded, Status: progress.StatusOK}
	case failures > 0:
		return batch.ItemResult{Outcome: batch.ItemFailed, Status: progress.StatusFailed}
	default:
		return batch.ItemResult{Outcome: batch.ItemSkipped, Status: progress.StatusOK}
	}
}

// reportCleanCycle emits every per-cycle metric and the completion
// audit log in one place: error counter or the tallied outcome + duration
// histogram, the remove-total counter, and the complete-audit line.
// Called via defer from clean so every exit path reports consistently.
func (c *OverReplicationCleaner) reportCleanCycle(ctx context.Context, start time.Time, out *cleanOutcome) {
	duration := time.Since(start)
	if out.err != nil {
		telemetry.OverReplicationRunsTotal.WithLabelValues(batch.OutcomeError).Inc()
		return
	}
	telemetry.OverReplicationRunsTotal.WithLabelValues(out.summary.Outcome()).Inc()
	telemetry.OverReplicationDuration.Observe(duration.Seconds())
	if out.objectsChecked == 0 {
		return
	}
	telemetry.OverReplicationRemovedTotal.Add(float64(out.summary.CopiesRemoved))
	audit.Log(ctx, "over_replication.complete",
		slog.Int("copies_removed", out.summary.CopiesRemoved),
		slog.Int("objects_checked", out.objectsChecked),
		slog.Int("objects_failed", out.summary.Failed),
		slog.Duration("duration", duration),
	)
}

// CountPending returns the number of objects exceeding the replication factor.
func (c *OverReplicationCleaner) CountPending(ctx context.Context, factor int) (int64, error) {
	backlog, err := c.store.CountReplicationBacklog(ctx, factor)
	return backlog.Over, err
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// scoredCopy pairs a copy location with a health/utilization score.
// Higher scores indicate copies that should be kept.
type scoredCopy struct {
	loc   core.ObjectLocation
	score float64
}

// ScoreCopy assigns a retention score to a copy based on its backend's state:
//   - draining backend: 0 (always remove first)
//   - circuit-broken backend: 1 (remove next)
//   - healthy backend: 2 + (1 - utilization_ratio), range [2..3]
//
// Among healthy backends the most utilized scores lowest, so its copy is
// removed first.
func (c *OverReplicationCleaner) ScoreCopy(loc *core.ObjectLocation, stats map[string]core.QuotaStat) float64 {
	if c.ops.IsDraining(loc.BackendName) {
		return 0
	}

	be, ok := c.ops.Backends()[loc.BackendName]
	if !ok {
		return 0
	}

	if cbb, ok := be.(*backend.CircuitBreakerBackend); ok && !cbb.IsHealthy() {
		return 1
	}

	// Healthy backend -- score by storage utilization
	if stat, ok := stats[loc.BackendName]; ok && stat.BytesLimit > 0 {
		utilization := float64(stat.BytesUsed) / float64(stat.BytesLimit)
		return 2 + (1 - utilization)
	}
	return 2.5 // no quota data: assume mid-range utilization
}

// cleanObject removes the lowest-scoring copies of one object until it is
// down to factor. RemoveExcessCopy re-reads the copy set under lock with
// factor, so a removal racing a client delete is skipped. It returns copies
// removed and removals failed.
func (c *OverReplicationCleaner) cleanObject(ctx context.Context, key string, copies []core.ObjectLocation, excess, factor int, stats map[string]core.QuotaStat) (removed, failures int) {
	// Score each copy
	scored := make([]scoredCopy, len(copies))
	for i := range copies {
		scored[i] = scoredCopy{loc: copies[i], score: c.ScoreCopy(&copies[i], stats)}
	}

	// Sort ascending: lowest score = first to remove
	slices.SortFunc(scored, func(a, b scoredCopy) int {
		return cmp.Compare(a.score, b.score)
	})

	for i := 0; i < excess && i < len(scored); i++ {
		if ctx.Err() != nil {
			break
		}
		victim := scored[i].loc

		// Remove metadata first so the replicator never sees a ghost copy; a
		// failed backend delete goes to the cleanup queue. dropped=false means
		// a concurrent delete or earlier tick already absorbed the excess.
		dropped, err := c.store.RemoveExcessCopy(ctx, key, victim.BackendName, factor)
		switch {
		case errors.Is(err, core.ErrCopyHoldsOnlyDEK):
			// This victim is the only copy that can still decrypt the object,
			// so keep it over-replicated. A mixed copy set means a row lost its
			// metadata and needs repair, hence the warning.
			c.log.WarnContext(ctx, "skipping over-replication removal: victim holds the only usable encryption key",
				"key", key, "backend", victim.BackendName)
			telemetry.OverReplicationKeyPreservedTotal.Inc()
			audit.Log(ctx, "over_replication.key_preserved",
				slog.String("key", key),
				slog.String("backend", victim.BackendName),
			)
			continue
		case err != nil:
			c.log.WarnContext(ctx, "failed to remove metadata",
				"key", key, "backend", victim.BackendName, "error", err)
			telemetry.OverReplicationErrorsTotal.Inc()
			failures++
			continue
		}
		if !dropped.Removed {
			continue
		}
		be, err := c.ops.GetBackend(victim.BackendName)
		if err != nil {
			c.log.WarnContext(ctx, "backend not found",
				"key", key, "backend", victim.BackendName)
			telemetry.OverReplicationErrorsTotal.Inc()
			failures++
			continue
		}

		// The path comes from the transaction that dropped the row, not from
		// the scan: a copy the cleaner selected minutes ago may have been
		// replaced by a newer write since, and deleting the scan's path would
		// then take that write's bytes.
		c.placement.DeleteOrEnqueue(ctx, be, &core.CleanupRequest{
			BackendName: victim.BackendName,
			ObjectKey:   key,
			StorageKey:  core.StoragePath(key, dropped.StorageKey),
			Reason:      "over_replication",
			SizeBytes:   dropped.SizeBytes,
		})

		audit.Log(ctx, "over_replication.remove",
			slog.String("key", key),
			slog.String("backend", victim.BackendName),
			slog.Int64("size", victim.SizeBytes),
			slog.Float64("score", scored[i].score),
		)

		removed++
	}

	return removed, failures
}
