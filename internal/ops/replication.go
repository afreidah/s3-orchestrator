// -------------------------------------------------------------------------------
// Ops - Replication Operations
//
// Author: Alex Freidah
//
// The two halves of keeping copy counts at the configured factor: a cycle that
// creates the copies under-replicated objects are missing, and a cycle that
// removes the surplus copies over-replicated objects carry. Both decline only
// when replication is meaningless at the running factor.
// -------------------------------------------------------------------------------

package ops

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/progress"
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

// ReplicationDeps holds the collaborators Replication requires.
type ReplicationDeps struct {
	Replicator Replicator
	OverRep    OverReplicationOps
	Config     *config.Live
}

// Replication serves the copy-count operations shared by the admin API and
// the web UI. The workers report their own passes, so it only resolves the
// running config and passes the request on.
type Replication struct {
	replicator Replicator
	overRep    OverReplicationOps
	cfg        *config.Live
}

// NewReplication is the explicit-deps constructor.
func NewReplication(d ReplicationDeps) *Replication {
	must.NotNil("d.Replicator", d.Replicator)
	must.NotNil("d.OverRep", d.OverRep)
	must.NotNil("d.Config", d.Config)
	return &Replication{
		replicator: d.Replicator,
		overRep:    d.OverRep,
		cfg:        d.Config,
	}
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
	return r.replicator.Replicate(ctx, runCfg, observer)
}

// CountSurplus reports the current over-replication backlog at the running
// factor.
func (r *Replication) CountSurplus(ctx context.Context) (SurplusCount, error) {
	runCfg, err := r.runConfig()
	if err != nil {
		return SurplusCount{}, err
	}

	pending, err := r.overRep.CountPending(ctx, runCfg.Factor)
	if err != nil {
		return SurplusCount{}, err
	}
	return SurplusCount{Factor: runCfg.Factor, Pending: pending}, nil
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
	return r.overRep.Clean(ctx, runCfg, observer)
}

// runConfig returns the running config for one manual cycle, whose defaults
// the config loader filled. Reports ErrReplicationDisabled when the factor
// leaves nothing to replicate.
func (r *Replication) runConfig() (config.ReplicationConfig, error) {
	runCfg := r.cfg.Load().Replication
	if runCfg.Factor <= 1 {
		return config.ReplicationConfig{}, ErrReplicationDisabled
	}
	return runCfg, nil
}
