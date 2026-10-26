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

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
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
	DeleteObjectLocation(ctx context.Context, loc *core.ObjectLocation, keepAtLeast int, reason string) ([]core.CleanupItem, error)
}

// reasonOverReplication labels the cleanup of an excess copy's bytes.
const reasonOverReplication = "over_replication"

// OverReplicationCleaner removes excess copies of objects that exceed the
// configured replication factor. Its replication config is reloadable through
// the embedded SetConfig and Config.
type OverReplicationCleaner struct {
	syncutil.Reloadable[config.ReplicationConfig]
	log       *slog.Logger
	ops       Ops
	placement Placement
	store     OverReplicationCleanerStore
}

// OverReplicationCleanerDeps groups the cleaner's constructor dependencies:
// fleet operations, write-path placement, and the metadata store.
type OverReplicationCleanerDeps struct {
	Ops       Ops
	Placement Placement
	Store     OverReplicationCleanerStore
}

// NewOverReplicationCleaner creates a cleaner from its dependencies, all of
// which are required.
func NewOverReplicationCleaner(deps OverReplicationCleanerDeps) *OverReplicationCleaner {
	must.NotNil("Ops", deps.Ops)
	must.NotNil("Placement", deps.Placement)
	must.NotNil("Store", deps.Store)
	return &OverReplicationCleaner{
		ops:       deps.Ops,
		placement: deps.Placement,
		store:     deps.Store,
		log:       workerLogger(audit.WorkloadOverReplication),
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Clean finds over-replicated objects and removes excess copies to reach the
// target replication factor. Returns the number of copies removed. observer,
// when non-nil, receives a start and end step per object cleaned.
func (c *OverReplicationCleaner) Clean(ctx context.Context, cfg config.ReplicationConfig, observer progress.Observer) (OverReplicationSummary, error) {
	return runOpsCycle(ctx, audit.WorkloadOverReplication, func(ctx context.Context) (OverReplicationSummary, error) {
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

// AuditAttrs adds the copies removed to the tally's attributes.
func (s OverReplicationSummary) AuditAttrs() []slog.Attr {
	return append(s.Summary.AuditAttrs(), slog.Int("copies_removed", s.CopiesRemoved))
}

// clean is the body of Clean after observe.Run sets up the span.
func (c *OverReplicationCleaner) clean(ctx context.Context, cfg config.ReplicationConfig, observer progress.Observer) (OverReplicationSummary, error) {
	if cfg.Factor <= 1 {
		return OverReplicationSummary{}, nil
	}

	audit.Log(ctx, "over_replication.start",
		slog.Int("factor", cfg.Factor),
		slog.Int("batch_size", cfg.BatchSize),
	)

	locations, err := c.store.GetOverReplicatedObjects(ctx, cfg.Factor, cfg.BatchSize)
	if err != nil {
		return OverReplicationSummary{}, fmt.Errorf("failed to query over-replicated objects: %w", err)
	}
	sets := planCopySets(locations, overBy(cfg.Factor))
	if len(sets) == 0 {
		return OverReplicationSummary{}, nil
	}

	// Pre-fetch quota stats for copy scoring (utilization ratio).
	quotaStats, qErr := c.store.GetQuotaStats(ctx)
	if qErr != nil {
		c.log.WarnContext(ctx, "failed to get quota stats, scoring without utilization",
			"error", qErr)
	}

	// The pending gauge is the fleet snapshot's ledger count, which every
	// instance serves; this pass's sets are one batch of it.
	pass := copySetPass{log: c.log, concurrency: cfg.Concurrency, observer: observer}
	sum, removed := pass.run(ctx, c.ops, sets, func(ctx context.Context, set *copySet) (int, batch.ItemResult) {
		n, failures := c.cleanObject(ctx, set.key, set.copies, set.off, cfg.Factor, quotaStats)
		return n, cleanupItemResult(n, failures)
	})

	telemetry.OverReplicationRemovedTotal.Add(float64(removed))
	return OverReplicationSummary{Summary: sum, CopiesRemoved: removed}, nil
}

// cleanupItemResult folds one object's surplus-copy removals into the batch
// tally. An object whose removals all failed is a failed item; one that gave
// up nothing without erroring was a benign race (a parallel delete or an
// earlier tick already absorbed the excess) and is skipped, not failed.
func cleanupItemResult(removed, failures int) batch.ItemResult {
	switch {
	case removed > 0:
		return batch.Succeeded()
	case failures > 0:
		return batch.Failed()
	default:
		return batch.ItemResult{Outcome: batch.ItemSkipped, Status: progress.StatusOK}
	}
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
//   - backend refusing writes, such as a draining one: 0 (always remove first)
//   - backend refusing reads: 1 (remove next)
//   - healthy backend: 2 + (1 - utilization_ratio), range [2..3]
//
// Among healthy backends the most utilized scores lowest, so its copy is
// removed first.
func (c *OverReplicationCleaner) ScoreCopy(loc *core.ObjectLocation, stats map[string]core.QuotaStat) float64 {
	if !c.ops.BackendFlags(loc.BackendName).AcceptedBy(core.AcceptForWrite) {
		return 0
	}

	if _, ok := c.ops.Backends()[loc.BackendName]; !ok {
		return 0
	}
	if !c.ops.ServedCalls(loc.BackendName).Has(core.RequiredForRead) {
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
// down to factor. DeleteObjectLocation re-reads the copy set under lock with
// factor, so a removal racing a client delete or overwrite is skipped. It
// returns copies removed and removals failed.
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

		// The row goes and its bytes are queued in one transaction, so the
		// replicator never sees a ghost copy. A concurrent delete or earlier
		// tick may have absorbed the excess, or a client overwrite replaced the
		// victim; either is skipped.
		dropped, err := c.store.DeleteObjectLocation(ctx, &victim, factor, reasonOverReplication)
		switch {
		case errors.Is(err, core.ErrCopyNotExcess), errors.Is(err, core.ErrCopyChanged):
			continue
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
		c.placement.Settle(ctx, dropped)

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
