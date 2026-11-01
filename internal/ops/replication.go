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
	"cmp"
	"context"
	"log/slog"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// Defaults applied to a manual replication cycle for any setting the worker
// config and the running config both leave unset.
const (
	defaultReplicationBatchSize   = 100
	defaultReplicationConcurrency = 5
)

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
	Runtime    RuntimeOps
	Config     *config.Live
}

// Replication serves the copy-count operations shared by the admin API and
// the web UI.
type Replication struct {
	log        *slog.Logger
	replicator Replicator
	overRep    OverReplicationOps
	runtime    RuntimeOps
	cfg        *config.Live
}

// NewReplication is the explicit-deps constructor.
func NewReplication(d ReplicationDeps) *Replication {
	must.NotNil("d.Replicator", d.Replicator)
	must.NotNil("d.OverRep", d.OverRep)
	must.NotNil("d.Runtime", d.Runtime)
	must.NotNil("d.Config", d.Config)
	return &Replication{
		log:        slog.Default().With(logfmt.Component("ops")),
		replicator: d.Replicator,
		overRep:    d.OverRep,
		runtime:    d.Runtime,
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

	sum, err := r.replicator.Replicate(ctx, runCfg, observer)
	if err != nil {
		return worker.ReplicationSummary{}, err
	}

	if mErr := r.runtime.UpdateQuotaMetrics(ctx); mErr != nil {
		r.log.WarnContext(ctx, "failed to update quota metrics after replicate", "error", mErr)
	}

	r.log.InfoContext(ctx, "replication cycle completed",
		"copies_created", sum.CopiesCreated, "objects_failed", sum.Failed)
	return sum, nil
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

	sum, err := r.overRep.Clean(ctx, runCfg, observer)
	if err != nil {
		return worker.OverReplicationSummary{}, err
	}

	if mErr := r.runtime.UpdateQuotaMetrics(ctx); mErr != nil {
		r.log.WarnContext(ctx, "failed to update quota metrics after surplus cleanup", "error", mErr)
	}

	r.log.InfoContext(ctx, "surplus cleanup completed",
		"copies_removed", sum.CopiesRemoved, "objects_failed", sum.Failed)
	return sum, nil
}

// runConfig resolves the settings for one manual cycle: the running config,
// with the defaults filling anything it leaves unset. Reports
// ErrReplicationDisabled when the factor leaves nothing to replicate.
func (r *Replication) runConfig() (config.ReplicationConfig, error) {
	runCfg := r.cfg.Load().Replication
	if runCfg.Factor <= 1 {
		return config.ReplicationConfig{}, ErrReplicationDisabled
	}

	runCfg.BatchSize = cmp.Or(runCfg.BatchSize, defaultReplicationBatchSize)
	runCfg.Concurrency = cmp.Or(runCfg.Concurrency, defaultReplicationConcurrency)
	return runCfg, nil
}
