// -------------------------------------------------------------------------------
// Replicator - Background Service Constructor
//
// Author: Alex Freidah
//
// Wraps *Replicator in a lifecycle.Runner backed by the shared
// advisory-locked ticker primitive. Replication is the one service
// that registers a Startup hook so the first pass fires immediately on
// boot rather than waiting one tick.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle/tickrunner"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// DefaultReplicatorTick is the replicator's per-tick cadence when the
// config does not specify one.
const DefaultReplicatorTick = 5 * time.Minute

// NewReplicatorService constructs the replication background service.
func NewReplicatorService(manager tickrunner.QuotaMetricsRefresher, replicator *Replicator, locker tickrunner.AdvisoryLocker) lifecycle.Runner {
	const slug = "replication"
	log := tickrunner.ComponentLogger(slug)
	replicateWork := func(ctx context.Context) error {
		rcfg := replicator.Config()
		if rcfg == nil {
			return nil
		}
		created, err := replicator.Replicate(ctx, *rcfg, nil)
		return tickrunner.HandlePassResult(ctx, log, manager, created, err, "copies_created")
	}

	interval := DefaultReplicatorTick
	if rcfg := replicator.Config(); rcfg != nil && rcfg.WorkerInterval > 0 {
		interval = rcfg.WorkerInterval
	}
	return tickrunner.New(tickrunner.Config{
		Locker:   locker,
		Interval: interval,
		LockID:   core.LockReplicator,
		Name:     slug,
		Log:      log,
		ShouldRun: func() bool {
			rcfg := replicator.Config()
			return rcfg != nil && rcfg.Factor > 1
		},
		Startup: replicateWork,
		Work:    replicateWork,
	})
}
