// -------------------------------------------------------------------------------
// OverReplicationCleaner - Background Service Constructor
//
// Author: Alex Freidah
//
// Wraps *OverReplicationCleaner in a lifecycle.Runner backed by the
// shared advisory-locked ticker primitive (#925).
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle/tickrunner"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// DefaultOverReplicationTick is the over-replication cleaner's
// per-tick cadence when the config does not specify one.
const DefaultOverReplicationTick = 5 * time.Minute

// NewOverReplicationService constructs the over-replication cleanup service.
func NewOverReplicationService(manager tickrunner.QuotaMetricsRefresher, overRep *OverReplicationCleaner, locker tickrunner.AdvisoryLocker) lifecycle.Runner {
	interval := DefaultOverReplicationTick
	if rcfg := overRep.Config(); rcfg != nil && rcfg.WorkerInterval > 0 {
		interval = rcfg.WorkerInterval
	}
	const slug = "over_replication_cleanup"
	log := tickrunner.ComponentLogger(slug)
	return tickrunner.New(tickrunner.Config{
		Locker:   locker,
		Interval: interval,
		LockID:   core.LockOverReplication,
		Name:     slug,
		Log:      log,
		ShouldRun: func() bool {
			rcfg := overRep.Config()
			return rcfg != nil && rcfg.Factor > 1
		},
		Work: func(ctx context.Context) error {
			rcfg := overRep.Config()
			if rcfg == nil {
				return nil
			}
			removed, err := overRep.Clean(ctx, *rcfg)
			return tickrunner.HandlePassResult(ctx, log, manager, removed, err, "copies_removed")
		},
	})
}
