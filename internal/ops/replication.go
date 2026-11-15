// -------------------------------------------------------------------------------
// Ops - Replication Operations
//
// Author: Alex Freidah
//
// The two halves of keeping copy counts at the configured factor: a cycle that
// creates the copies under-replicated objects are missing, and a cycle that
// removes the surplus copies over-replicated objects carry. Both run under the
// replicator's lock, so one never overlaps the replication service's drain, and
// decline when replication is meaningless at the running factor.
// -------------------------------------------------------------------------------

package ops

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// maxCleanBatchSize caps a caller-supplied surplus-cleanup batch so one
// request cannot schedule an unbounded pass.
const maxCleanBatchSize = 10000

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// SurplusCount reports how many objects currently hold more copies than the
// running factor calls for.
type SurplusCount struct {
	Factor  int
	Pending int64
}

// QueueRebuild reports a rebuild of the replication queue: the factor it
// counted against and how many keys it queued.
type QueueRebuild struct {
	Factor int
	Queued int64
}

// ReplicationDeps holds the collaborators Replication requires.
type ReplicationDeps struct {
	Replicator Replicator
	OverRep    OverReplicationCleaner
	Backlog    BacklogCounter
	Locker     core.AdvisoryLocker
	Config     *config.Live
}

// Replication serves the copy-count operations shared by the admin API and
// the web UI. The workers report their own passes, so it only resolves the
// running config and passes the request on.
type Replication struct {
	d ReplicationDeps
}

// NewReplication is the explicit-deps constructor.
func NewReplication(d ReplicationDeps) *Replication {
	must.NotNil("d.Replicator", d.Replicator)
	must.NotNil("d.OverRep", d.OverRep)
	must.NotNil("d.Backlog", d.Backlog)
	must.NotNil("d.Locker", d.Locker)
	must.NotNil("d.Config", d.Config)
	return &Replication{d: d}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Replicate runs one replication cycle and reports its tally and the copies it
// created. observer, when non-nil, receives a start and end step per object
// replicated.
func (r *Replication) Replicate(ctx context.Context, observer progress.Observer) (worker.ReplicationSummary, error) {
	runCfg, err := r.runConfig()
	if err != nil {
		return worker.ReplicationSummary{}, err
	}
	return underLock(ctx, r.d.Locker, core.LockReplicator, ErrReplicationInProgress, func(lockCtx context.Context) (worker.ReplicationSummary, error) {
		return r.d.Replicator.Replicate(lockCtx, runCfg, observer)
	})
}

// CountSurplus reports the queued over-replication backlog at the running
// factor.
func (r *Replication) CountSurplus(ctx context.Context) (SurplusCount, error) {
	runCfg, err := r.runConfig()
	if err != nil {
		return SurplusCount{}, err
	}

	backlog, err := r.d.Backlog.CountReplicationQueue(ctx)
	if err != nil {
		return SurplusCount{}, err
	}
	return SurplusCount{Factor: runCfg.Factor, Pending: backlog.Over}, nil
}

// RebuildQueue rebuilds the replication queue from the ledger at the running
// factor, for an operator who suspects it drifted, and reports how many keys
// it queued.
func (r *Replication) RebuildQueue(ctx context.Context) (QueueRebuild, error) {
	runCfg, err := r.runConfig()
	if err != nil {
		return QueueRebuild{}, err
	}
	return underLock(ctx, r.d.Locker, core.LockReplicator, ErrReplicationInProgress, func(lockCtx context.Context) (QueueRebuild, error) {
		queued, err := r.d.Replicator.RebuildQueue(lockCtx, runCfg)
		return QueueRebuild{Factor: runCfg.Factor, Queued: queued}, err
	})
}

// CleanExcess removes copies beyond the configured factor. batchSize <= 0 uses
// the resolved config; a larger request is capped. observer, when non-nil,
// receives an end step per copy removed.
func (r *Replication) CleanExcess(ctx context.Context, batchSize int, observer progress.Observer) (worker.OverReplicationSummary, error) {
	runCfg, err := r.runConfig()
	if err != nil {
		return worker.OverReplicationSummary{}, err
	}
	if batchSize > 0 {
		runCfg.BatchSize = min(batchSize, maxCleanBatchSize)
	}
	return underLock(ctx, r.d.Locker, core.LockReplicator, ErrReplicationInProgress, func(lockCtx context.Context) (worker.OverReplicationSummary, error) {
		return r.d.OverRep.Clean(lockCtx, runCfg, observer)
	})
}

// runConfig returns the running config for one manual cycle, whose defaults
// the config loader filled. Reports ErrReplicationDisabled when the factor
// leaves nothing to replicate.
func (r *Replication) runConfig() (config.ReplicationConfig, error) {
	runCfg := r.d.Config.Load().Replication
	if runCfg.Factor <= 1 {
		return config.ReplicationConfig{}, ErrReplicationDisabled
	}
	return runCfg, nil
}
