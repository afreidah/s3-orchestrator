// -------------------------------------------------------------------------------
// Worker Services - The Background Service of Each Worker
//
// Author: Alex Freidah
//
// Every worker runs one pass per tick under its own advisory lock, named for
// its workload like everything else it reports. The pass reports itself
// through the cycle envelope, so a service only says when it ticks, whether a
// tick runs, and which pass it runs. Intervals are read once here: the config
// fills their defaults, and changing one takes a restart.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle/tickrunner"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
)

// The fixed ticks of the workers no config sets an interval for.
const (
	cleanupQueueTick = time.Minute
	drainTick        = 10 * time.Second
)

// passWork adapts a pass to a tick's work. The pass reports itself, so the
// tick needs only its error.
func passWork[T any](pass func(ctx context.Context) (T, error)) func(context.Context) error {
	return func(ctx context.Context) error {
		_, err := pass(ctx)
		return err
	}
}

// NewReplicatorService constructs the replication service, which runs while
// the replication factor is above one. It also runs a pass as it starts, so a
// restart does not leave objects under-replicated for a whole interval.
func NewReplicatorService(rep *Replicator, locker core.AdvisoryLocker) lifecycle.Runner {
	return tickrunner.New(tickrunner.Config{
		Locker:       locker,
		LockID:       core.LockReplicator,
		Name:         string(audit.WorkloadReplication),
		Interval:     rep.Config().WorkerInterval,
		ShouldRun:    func() bool { return rep.Config().Factor > 1 },
		RunAtStartup: true,
		Work: passWork(func(ctx context.Context) (ReplicationSummary, error) {
			return rep.Replicate(ctx, *rep.Config(), nil)
		}),
	})
}

// NewOverReplicationService constructs the over-replication cleanup service,
// which runs while the replication factor is above one.
func NewOverReplicationService(c *OverReplicationCleaner, locker core.AdvisoryLocker) lifecycle.Runner {
	return tickrunner.New(tickrunner.Config{
		Locker:    locker,
		LockID:    core.LockOverReplication,
		Name:      string(audit.WorkloadOverReplication),
		Interval:  c.Config().WorkerInterval,
		ShouldRun: func() bool { return c.Config().Factor > 1 },
		Work: passWork(func(ctx context.Context) (OverReplicationSummary, error) {
			return c.Clean(ctx, *c.Config(), nil)
		}),
	})
}

// NewRebalancerService constructs the rebalancer service, which runs while
// rebalancing is enabled.
func NewRebalancerService(r *Rebalancer, locker core.AdvisoryLocker) lifecycle.Runner {
	return tickrunner.New(tickrunner.Config{
		Locker:    locker,
		LockID:    core.LockRebalancer,
		Name:      string(audit.WorkloadRebalance),
		Interval:  r.Config().Interval,
		ShouldRun: func() bool { return r.Config().Enabled },
		Work: passWork(func(ctx context.Context) (RebalanceSummary, error) {
			return r.Rebalance(ctx, *r.Config(), nil)
		}),
	})
}

// NewDrainerService constructs the drain service. The tick only decides how
// soon a new or resumed drain is picked up: a pass works each drain until it
// finishes or stalls.
func NewDrainerService(d *Drainer, locker core.AdvisoryLocker) lifecycle.Runner {
	return tickrunner.New(tickrunner.Config{
		Locker:   locker,
		LockID:   core.LockDrain,
		Name:     string(audit.WorkloadDrain),
		Interval: drainTick,
		Work: passWork(func(ctx context.Context) (DrainSummary, error) {
			return d.Drain(ctx, nil)
		}),
	})
}

// NewScrubberService constructs the integrity scrubber service, which runs
// while integrity is enabled. A zero scrubber interval turns the sweep off,
// and the caller registers no service for it.
func NewScrubberService(s *Scrubber, locker core.AdvisoryLocker) lifecycle.Runner {
	return tickrunner.New(tickrunner.Config{
		Locker:    locker,
		LockID:    core.LockScrubber,
		Name:      string(audit.WorkloadScrub),
		Interval:  s.Config().ScrubberInterval,
		ShouldRun: func() bool { return s.Config().IsEnabled() },
		Work: passWork(func(ctx context.Context) (batch.Summary, error) {
			return s.Scrub(ctx, s.Config().ScrubberBatchSize, "", nil)
		}),
	})
}

// NewCleanupQueueService constructs the cleanup queue service.
func NewCleanupQueueService(w *CleanupWorker, locker core.AdvisoryLocker) lifecycle.Runner {
	return tickrunner.New(tickrunner.Config{
		Locker:   locker,
		LockID:   core.LockCleanupQueue,
		Name:     string(audit.WorkloadCleanup),
		Interval: cleanupQueueTick,
		Work:     passWork(w.ProcessCleanupQueue),
	})
}

// NewPendingReaperService constructs the pending reaper service, which
// resolves abandoned PUT intents every tick.
func NewPendingReaperService(r *PendingReaper, locker core.AdvisoryLocker, tick time.Duration) lifecycle.Runner {
	return tickrunner.New(tickrunner.Config{
		Locker:   locker,
		LockID:   core.LockPendingReaper,
		Name:     string(audit.WorkloadPendingReaper),
		Interval: tick,
		Work:     passWork(r.ProcessPendingQueue),
	})
}

// NewReconcileService constructs the scheduled reconcile service.
func NewReconcileService(r *Reconciler, locker core.AdvisoryLocker, interval time.Duration) lifecycle.Runner {
	return tickrunner.New(tickrunner.Config{
		Locker:   locker,
		LockID:   core.LockReconcile,
		Name:     string(audit.WorkloadReconcile),
		Interval: interval,
		Work:     passWork(r.Run),
	})
}
