// -------------------------------------------------------------------------------
// Rebalancer - Background Service Constructor
//
// Author: Alex Freidah
//
// Wraps *Rebalancer in a lifecycle.Runner backed by the shared
// advisory-locked ticker primitive. Uses HandlePassResult so the
// success-with-work logging + quota-metrics refresh stays identical
// to replication and over-replication.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle/tickrunner"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// DefaultRebalanceInterval is the rebalancer's per-tick cadence when
// the config does not specify one.
const DefaultRebalanceInterval = 6 * time.Hour

// NewRebalancerService constructs the rebalancer background service.
func NewRebalancerService(manager tickrunner.QuotaMetricsRefresher, rebalancer *Rebalancer, locker tickrunner.AdvisoryLocker) lifecycle.Runner {
	interval := DefaultRebalanceInterval
	if rcfg := rebalancer.Config(); rcfg != nil && rcfg.Interval > 0 {
		interval = rcfg.Interval
	}
	const slug = "rebalance"
	log := tickrunner.ComponentLogger(slug)
	return tickrunner.New(tickrunner.Config{
		Locker:   locker,
		Interval: interval,
		LockID:   core.LockRebalancer,
		Name:     slug,
		Log:      log,
		ShouldRun: func() bool {
			rcfg := rebalancer.Config()
			return rcfg != nil && rcfg.Enabled
		},
		Work: func(ctx context.Context) error {
			rcfg := rebalancer.Config()
			if rcfg == nil {
				return nil
			}
			sum, err := rebalancer.Rebalance(ctx, *rcfg)
			return tickrunner.HandlePassResult(ctx, log, manager, sum.Succeeded, err, "objects_moved")
		},
	})
}
