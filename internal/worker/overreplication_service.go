// -------------------------------------------------------------------------------
// OverReplicationCleaner - Background Service Constructor
//
// Author: Alex Freidah
//
// Wraps *OverReplicationCleaner in the shared pass service.
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

// DefaultOverReplicationTick is the over-replication cleaner's
// per-tick cadence when the config does not specify one.
const DefaultOverReplicationTick = 5 * time.Minute

// NewOverReplicationService constructs the over-replication cleanup service,
// which runs while the replication factor is above one.
func NewOverReplicationService(manager tickrunner.QuotaMetricsRefresher, overRep *OverReplicationCleaner, locker tickrunner.AdvisoryLocker) lifecycle.Runner {
	return passService(&passSpec[config.ReplicationConfig]{
		Workload:   audit.WorkloadOverReplication,
		Lock:       core.LockOverReplication,
		Config:     overRep.Config,
		Interval:   DefaultOverReplicationTick,
		Configured: func(cfg *config.ReplicationConfig) time.Duration { return cfg.WorkerInterval },
		Enabled:    func(cfg *config.ReplicationConfig) bool { return cfg.Factor > 1 },
		Pass: func(ctx context.Context, cfg *config.ReplicationConfig) (int, error) {
			sum, err := overRep.Clean(ctx, *cfg, nil)
			return sum.CopiesRemoved, err
		},
		CountKey: "copies_removed",
	}, manager, locker)
}
