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
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

// Defaults applied to a manual replication cycle for any setting the worker
// config and the running config both leave unset.
const (
	defaultReplicationBatchSize   = 100
	defaultReplicationConcurrency = 5
)

// maxCleanBatchSize caps a caller-supplied surplus-cleanup batch so one
// request cannot schedule an unbounded pass.
const maxCleanBatchSize = 10000

// ReplicateResult reports one replication cycle. Failed counts the objects the
// cycle could not bring up to factor, so a pass that created nothing because
// everything failed does not read the same as one with nothing to do.
type ReplicateResult struct {
	CopiesCreated int
	Failed        int
}

// CleanExcessResult reports one surplus-copy cleanup cycle. Failed counts the
// objects whose surplus could not be removed.
type CleanExcessResult struct {
	CopiesRemoved int
	Failed        int
}

// SurplusCount reports how many objects currently hold more copies than the
// running factor calls for.
type SurplusCount struct {
	Factor  int
	Pending int64
}

// ReplicationDeps holds the collaborators Replication requires.
type ReplicationDeps struct {
	Replicator ReplicatorOps
	OverRep    OverReplicationOps
	Runtime    RuntimeOps
	Config     *ConfigStore
}

// Replication serves the copy-count operations shared by the admin API and
// the web UI.
type Replication struct {
	log        *slog.Logger
	replicator ReplicatorOps
	overRep    OverReplicationOps
	runtime    RuntimeOps
	cfg        *ConfigStore
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

// Replicate runs one replication cycle and returns the copies it created.
// observer, when non-nil, receives a start and end step per object replicated.
func (r *Replication) Replicate(ctx context.Context, observer progress.Observer) (ReplicateResult, error) {
	runCfg, err := r.runConfig(r.replicator.Config())
	if err != nil {
		return ReplicateResult{}, err
	}

	sum, err := r.replicator.Replicate(ctx, runCfg, observer)
	if err != nil {
		return ReplicateResult{}, err
	}

	if mErr := r.runtime.UpdateQuotaMetrics(ctx); mErr != nil {
		r.log.WarnContext(ctx, "failed to update quota metrics after replicate", "error", mErr)
	}

	event.Publish(event.ReplicationCompleted, "", map[string]any{
		"copies_created": sum.CopiesCreated,
		"objects_failed": sum.Failed,
	})
	r.log.InfoContext(ctx, "replication cycle completed",
		"copies_created", sum.CopiesCreated, "objects_failed", sum.Failed)
	return ReplicateResult{CopiesCreated: sum.CopiesCreated, Failed: sum.Failed}, nil
}

// CountSurplus reports the current over-replication backlog at the running
// factor.
func (r *Replication) CountSurplus(ctx context.Context) (SurplusCount, error) {
	runCfg, err := r.runConfig(r.overRep.Config())
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
func (r *Replication) CleanExcess(ctx context.Context, batchSize int, observer progress.Observer) (CleanExcessResult, error) {
	runCfg, err := r.runConfig(r.overRep.Config())
	if err != nil {
		return CleanExcessResult{}, err
	}
	if batchSize > 0 {
		runCfg.BatchSize = min(batchSize, maxCleanBatchSize)
	}

	sum, err := r.overRep.Clean(ctx, runCfg, observer)
	if err != nil {
		return CleanExcessResult{}, err
	}

	if mErr := r.runtime.UpdateQuotaMetrics(ctx); mErr != nil {
		r.log.WarnContext(ctx, "failed to update quota metrics after surplus cleanup", "error", mErr)
	}

	r.log.InfoContext(ctx, "surplus cleanup completed",
		"copies_removed", sum.CopiesRemoved, "objects_failed", sum.Failed)
	return CleanExcessResult{CopiesRemoved: sum.CopiesRemoved, Failed: sum.Failed}, nil
}

// runConfig resolves the settings for one manual cycle: whatever the worker
// holds, then the running config, then the defaults. Reports
// ErrReplicationDisabled when the resolved factor leaves nothing to replicate.
func (r *Replication) runConfig(workerCfg *config.ReplicationConfig) (config.ReplicationConfig, error) {
	var runCfg config.ReplicationConfig
	switch {
	case workerCfg != nil:
		runCfg = *workerCfg
	case r.cfg.Load() != nil:
		runCfg = r.cfg.Load().Replication
	}

	if runCfg.Factor <= 1 {
		return config.ReplicationConfig{}, ErrReplicationDisabled
	}

	runCfg.BatchSize = cmp.Or(runCfg.BatchSize, defaultReplicationBatchSize)
	runCfg.Concurrency = cmp.Or(runCfg.Concurrency, defaultReplicationConcurrency)
	return runCfg, nil
}
