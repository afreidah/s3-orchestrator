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
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
	"github.com/afreidah/s3-orchestrator/internal/util/workerpool"
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
// configured replication factor. Embeds *backendCore for access to shared
// infrastructure.
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
func (c *OverReplicationCleaner) Clean(ctx context.Context, cfg config.ReplicationConfig, observer progress.Observer) (int, error) {
	return runOpsCycle(ctx, "OverReplicationClean", "over_replication_clean", func(ctx context.Context) (int, error) {
		return c.clean(ctx, cfg, observer)
	})
}

// cleanOutcome captures everything the post-cycle reporter needs so the
// body of clean stays focused on the cleanup work itself.
type cleanOutcome struct {
	copiesRemoved  int
	objectsChecked int
	err            error
}

// clean is the body of Clean after observe.Run sets up the span. All
// per-cycle telemetry and audit emission lives in reportCleanCycle so
// the work below reads as straight business logic.
func (c *OverReplicationCleaner) clean(ctx context.Context, cfg config.ReplicationConfig, observer progress.Observer) (int, error) {
	if cfg.Factor <= 1 {
		return 0, nil
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
		return 0, out.err
	}
	if len(locations) == 0 {
		return 0, nil
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

	telemetry.OverReplicationPending.Set(float64(len(tasks)))
	var removed atomic.Int64
	workerpool.Run(ctx, cfg.Concurrency, tasks, func(ctx context.Context, task cleanupTask) {
		defer telemetry.OverReplicationPending.Dec()
		WithAdmission(ctx, c.ops, WorkerNameOverReplication, func() {
			progress.Track(observer, task.key, func() string {
				n := c.cleanObject(ctx, task.key, task.copies, task.excess, cfg.Factor, quotaStats)
				removed.Add(int64(n))
				return progress.StatusOK
			})
		})
	})

	out.copiesRemoved = int(removed.Load())
	return out.copiesRemoved, nil
}

// reportCleanCycle emits every per-cycle metric and the completion
// audit log in one place: error counter or success counter + duration
// histogram, the remove-total counter, and the complete-audit line.
// Called via defer from clean so every exit path reports consistently.
func (c *OverReplicationCleaner) reportCleanCycle(ctx context.Context, start time.Time, out *cleanOutcome) {
	duration := time.Since(start)
	if out.err != nil {
		telemetry.OverReplicationRunsTotal.WithLabelValues("error").Inc()
		return
	}
	telemetry.OverReplicationRunsTotal.WithLabelValues("success").Inc()
	telemetry.OverReplicationDuration.Observe(duration.Seconds())
	if out.objectsChecked == 0 {
		telemetry.OverReplicationPending.Set(0)
		return
	}
	telemetry.OverReplicationRemovedTotal.Add(float64(out.copiesRemoved))
	audit.Log(ctx, "over_replication.complete",
		slog.Int("copies_removed", out.copiesRemoved),
		slog.Int("objects_checked", out.objectsChecked),
		slog.Duration("duration", duration),
	)
}

// CountPending returns the number of objects exceeding the replication factor.
func (c *OverReplicationCleaner) CountPending(ctx context.Context, factor int) (int64, error) {
	return c.store.CountOverReplicatedObjects(ctx, factor)
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
// Among healthy backends, the most utilized backend gets the lowest score,
// making its copy the first candidate for removal -- freeing space where it
// is scarcest.
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

// cleanObject removes excess copies of a single object. Scores all copies,
// sorts ascending, and removes the lowest-scoring copies until the count
// reaches the target factor. factor is forwarded to RemoveExcessCopy so
// the per-victim tx can re-read the copy set under lock and skip a
// removal that races with a concurrent client delete. Returns the
// number of copies removed.
func (c *OverReplicationCleaner) cleanObject(ctx context.Context, key string, copies []core.ObjectLocation, excess, factor int, stats map[string]core.QuotaStat) int {
	// Score each copy
	scored := make([]scoredCopy, len(copies))
	for i := range copies {
		scored[i] = scoredCopy{loc: copies[i], score: c.ScoreCopy(&copies[i], stats)}
	}

	// Sort ascending: lowest score = first to remove
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score < scored[j].score
	})

	removed := 0
	for i := 0; i < excess && i < len(scored); i++ {
		if ctx.Err() != nil {
			break
		}
		victim := scored[i].loc

		// Remove from metadata first so the replicator never sees a ghost
		// copy. If the DB remove succeeds but the backend delete fails, the
		// cleanup queue handles the orphan. removed=false is the benign
		// race outcome: a parallel client delete or earlier tick already
		// absorbed the excess, so this victim no longer needs touching.
		didRemove, err := c.store.RemoveExcessCopy(ctx, key, victim.BackendName, factor)
		if err != nil {
			c.log.WarnContext(ctx, "failed to remove metadata",
				"key", key, "backend", victim.BackendName, "error", err)
			telemetry.OverReplicationErrorsTotal.Inc()
			continue
		}
		if !didRemove {
			continue
		}

		be, err := c.ops.GetBackend(victim.BackendName)
		if err != nil {
			c.log.WarnContext(ctx, "backend not found",
				"key", key, "backend", victim.BackendName)
			telemetry.OverReplicationErrorsTotal.Inc()
			continue
		}

		c.placement.DeleteOrEnqueue(ctx, be, victim.BackendName, key, "over_replication", victim.SizeBytes)

		audit.Log(ctx, "over_replication.remove",
			slog.String("key", key),
			slog.String("backend", victim.BackendName),
			slog.Int64("size", victim.SizeBytes),
			slog.Float64("score", scored[i].score),
		)

		removed++
	}

	return removed
}
