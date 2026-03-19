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

package storage

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/audit"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/workerpool"
)

// -------------------------------------------------------------------------
// OVER-REPLICATION CLEANER TYPE
// -------------------------------------------------------------------------

// OverReplicationCleaner removes excess copies of objects that exceed the
// configured replication factor. Embeds *backendCore for access to shared
// infrastructure.
type OverReplicationCleaner struct {
	*backendCore
	cfg atomic.Pointer[config.ReplicationConfig]
}

// NewOverReplicationCleaner creates a cleaner that shares the given core.
func NewOverReplicationCleaner(core *backendCore) *OverReplicationCleaner {
	return &OverReplicationCleaner{backendCore: core}
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
// target replication factor. Returns the number of copies removed.
func (c *OverReplicationCleaner) Clean(ctx context.Context, cfg config.ReplicationConfig) (int, error) {
	start := time.Now()
	ctx = audit.WithRequestID(ctx, audit.NewID())
	ctx, span := telemetry.StartSpan(ctx, "OverReplicationClean",
		telemetry.AttrOperation.String("over_replication_clean"),
	)
	defer span.End()

	if cfg.Factor <= 1 {
		return 0, nil
	}

	audit.Log(ctx, "over_replication.start",
		slog.Int("factor", cfg.Factor),
		slog.Int("batch_size", cfg.BatchSize),
	)

	// --- Find over-replicated objects ---
	locations, err := c.store.GetOverReplicatedObjects(ctx, cfg.Factor, cfg.BatchSize)
	if err != nil {
		telemetry.OverReplicationRunsTotal.WithLabelValues("error").Inc()
		return 0, fmt.Errorf("failed to query over-replicated objects: %w", err)
	}

	if len(locations) == 0 {
		telemetry.OverReplicationPending.Set(0)
		telemetry.OverReplicationRunsTotal.WithLabelValues("success").Inc()
		telemetry.OverReplicationDuration.Observe(time.Since(start).Seconds())
		return 0, nil
	}

	// Pre-fetch quota stats for copy scoring (utilization ratio).
	quotaStats, qErr := c.store.GetQuotaStats(ctx)
	if qErr != nil {
		slog.WarnContext(ctx, "Over-replication: failed to get quota stats, scoring without utilization",
			"error", qErr)
	}

	// --- Group locations by object key ---
	grouped := groupByKey(locations)

	type cleanupTask struct {
		key    string
		copies []ObjectLocation
		excess int
	}
	var tasks []cleanupTask
	for key, copies := range grouped {
		excess := len(copies) - cfg.Factor
		if excess > 0 {
			tasks = append(tasks, cleanupTask{key: key, copies: copies, excess: excess})
		}
	}

	// --- Remove excess copies concurrently ---
	var removed atomic.Int64
	workerpool.Run(ctx, cfg.Concurrency, tasks, func(ctx context.Context, task cleanupTask) {
		n := c.cleanObject(ctx, task.key, task.copies, task.excess, quotaStats)
		removed.Add(int64(n))
	})

	copiesRemoved := int(removed.Load())
	telemetry.OverReplicationRemovedTotal.Add(float64(copiesRemoved))
	telemetry.OverReplicationRunsTotal.WithLabelValues("success").Inc()
	telemetry.OverReplicationDuration.Observe(time.Since(start).Seconds())

	audit.Log(ctx, "over_replication.complete",
		slog.Int("copies_removed", copiesRemoved),
		slog.Int("objects_checked", len(grouped)),
		slog.Duration("duration", time.Since(start)),
	)

	return copiesRemoved, nil
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
	loc   ObjectLocation
	score float64
}

// scoreCopy assigns a retention score to a copy based on its backend's state:
//   - draining backend: 0 (always remove first)
//   - circuit-broken backend: 1 (remove next)
//   - healthy backend: 2 + (1 - utilization_ratio), range [2..3]
//
// Among healthy backends, the most utilized backend gets the lowest score,
// making its copy the first candidate for removal -- freeing space where it
// is scarcest.
func (c *OverReplicationCleaner) scoreCopy(loc *ObjectLocation, stats map[string]QuotaStat) float64 {
	if c.IsDraining(loc.BackendName) {
		return 0
	}

	backend, ok := c.backends[loc.BackendName]
	if !ok {
		return 0
	}

	if cbb, ok := backend.(*CircuitBreakerBackend); ok && !cbb.IsHealthy() {
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
// reaches the target factor. Returns the number of copies removed.
func (c *OverReplicationCleaner) cleanObject(ctx context.Context, key string, copies []ObjectLocation, excess int, stats map[string]QuotaStat) int {
	// Score each copy
	scored := make([]scoredCopy, len(copies))
	for i := range copies {
		scored[i] = scoredCopy{loc: copies[i], score: c.scoreCopy(&copies[i], stats)}
	}

	// Sort ascending: lowest score = first to remove
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score < scored[j].score
	})

	removed := 0
	for i := 0; i < excess && i < len(scored); i++ {
		victim := scored[i].loc

		// Delete from backend
		backend, err := c.getBackend(victim.BackendName)
		if err != nil {
			slog.WarnContext(ctx, "Over-replication: backend not found",
				"key", key, "backend", victim.BackendName)
			telemetry.OverReplicationErrorsTotal.Inc()
			continue
		}

		c.deleteOrEnqueue(ctx, backend, victim.BackendName, key, "over_replication", victim.SizeBytes)

		// Remove from metadata
		if err := c.store.RemoveExcessCopy(ctx, key, victim.BackendName, victim.SizeBytes); err != nil {
			slog.WarnContext(ctx, "Over-replication: failed to remove metadata",
				"key", key, "backend", victim.BackendName, "error", err)
			telemetry.OverReplicationErrorsTotal.Inc()
			continue
		}

		c.usage.Record(victim.BackendName, 1, 0, 0) // Delete API call

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
