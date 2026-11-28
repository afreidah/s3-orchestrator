// -------------------------------------------------------------------------------
// Ops - Replication Operations
//
// Author: Alex Freidah
//
// The two halves of keeping copy counts at each key's factor: a cycle that
// creates the copies under-replicated objects are missing, and a cycle that
// removes the surplus copies over-replicated objects carry. Both run under the
// replicator's lock, so one never overlaps the replication service's drain, and
// decline when replication is meaningless at the running factors.
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

// Backlog reports the queued replication backlog: the factors it is counted
// against, how many objects hold fewer copies than their factor calls for, and
// how many hold more.
type Backlog struct {
	Factors core.ReplicationFactors
	Under   int64
	Over    int64
}

// QueueRebuild reports a rebuild of the replication queue: the factors it
// counted against and how many keys it queued.
type QueueRebuild struct {
	Factors core.ReplicationFactors
	Queued  int64
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

// Factors returns the running factors: the deployment's, and each bucket's
// own.
func (r *Replication) Factors() core.ReplicationFactors {
	return r.d.Replicator.Factors()
}

// CountBacklog reports the queued replication backlog. With replication
// disabled it reports the factors alone, with ErrReplicationDisabled.
func (r *Replication) CountBacklog(ctx context.Context) (Backlog, error) {
	factors := r.Factors()
	if !factors.Replicates() {
		return Backlog{Factors: factors}, ErrReplicationDisabled
	}
	backlog, err := r.d.Backlog.CountReplicationQueue(ctx)
	if err != nil {
		return Backlog{}, err
	}
	return Backlog{Factors: factors, Under: backlog.Under, Over: backlog.Over}, nil
}

// RebuildQueue rebuilds the replication queue from the ledger at the running
// factors, for an operator who suspects it drifted, and reports how many keys
// it queued.
func (r *Replication) RebuildQueue(ctx context.Context) (QueueRebuild, error) {
	runCfg, err := r.runConfig()
	if err != nil {
		return QueueRebuild{}, err
	}
	factors := r.Factors()
	return underLock(ctx, r.d.Locker, core.LockReplicator, ErrReplicationInProgress, func(lockCtx context.Context) (QueueRebuild, error) {
		queued, err := r.d.Replicator.RebuildQueue(lockCtx, runCfg)
		return QueueRebuild{Factors: factors, Queued: queued}, err
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
// the config loader filled. Reports ErrReplicationDisabled when no key's factor
// leaves anything to replicate.
func (r *Replication) runConfig() (config.ReplicationConfig, error) {
	if factors := r.Factors(); !factors.Replicates() {
		return config.ReplicationConfig{}, ErrReplicationDisabled
	}
	return r.d.Config.Load().Replication, nil
}
