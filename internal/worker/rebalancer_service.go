// -------------------------------------------------------------------------------
// Rebalancer - Background Service Constructor
//
// Author: Alex Freidah
//
// Wraps *Rebalancer in the shared pass service.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle/tickrunner"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// DefaultRebalanceInterval is the rebalancer's per-tick cadence when
// the config does not specify one.
const DefaultRebalanceInterval = 6 * time.Hour

// NewRebalancerService constructs the rebalancer background service, which
// runs while rebalancing is enabled.
func NewRebalancerService(manager tickrunner.QuotaMetricsRefresher, rebalancer *Rebalancer, locker tickrunner.AdvisoryLocker) lifecycle.Runner {
	return passService(&passSpec[config.RebalanceConfig]{
		Workload:   audit.WorkloadRebalance,
		Lock:       core.LockRebalancer,
		Config:     rebalancer.Config,
		Interval:   DefaultRebalanceInterval,
		Configured: func(cfg *config.RebalanceConfig) time.Duration { return cfg.Interval },
		Enabled:    func(cfg *config.RebalanceConfig) bool { return cfg.Enabled },
		Pass: func(ctx context.Context, cfg *config.RebalanceConfig) (int, error) {
			sum, err := rebalancer.Rebalance(ctx, *cfg, nil)
			return sum.Succeeded, err
		},
		CountKey: "objects_moved",
	}, manager, locker)
}
