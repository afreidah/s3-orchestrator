// -------------------------------------------------------------------------------
// Replicator - Background Service Constructor
//
// Author: Alex Freidah
//
// Wraps *Replicator in the shared pass service. Replication is the one service
// that also runs a pass at startup, so the first pass fires immediately on boot
// rather than waiting one tick.
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

// DefaultReplicatorTick is the replicator's per-tick cadence when the
// config does not specify one.
const DefaultReplicatorTick = 5 * time.Minute

// NewReplicatorService constructs the replication background service, which
// runs while the replication factor is above one.
func NewReplicatorService(manager tickrunner.QuotaMetricsRefresher, replicator *Replicator, locker tickrunner.AdvisoryLocker) lifecycle.Runner {
	return passService(&passSpec[config.ReplicationConfig]{
		Workload:   audit.WorkloadReplication,
		Lock:       core.LockReplicator,
		Config:     replicator.Config,
		Interval:   DefaultReplicatorTick,
		Configured: func(cfg *config.ReplicationConfig) time.Duration { return cfg.WorkerInterval },
		Enabled:    func(cfg *config.ReplicationConfig) bool { return cfg.Factor > 1 },
		Pass: func(ctx context.Context, cfg *config.ReplicationConfig) (int, error) {
			sum, err := replicator.Replicate(ctx, *cfg, nil)
			return sum.CopiesCreated, err
		},
		CountKey:     "copies_created",
		RunAtStartup: true,
	}, manager, locker)
}
