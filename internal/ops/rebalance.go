// -------------------------------------------------------------------------------
// Ops - Rebalance Operation
//
// Author: Alex Freidah
//
// One on-demand rebalance cycle, moving objects between backends until their
// utilisation converges. An operator who asks for a cycle gets one: when the
// worker holds no configuration the running config supplies it, and defaults
// fill whatever is still unset, so a manual run works on a fleet that never
// configured scheduled rebalancing.
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
)

// Defaults applied to a manual rebalance cycle for any setting the worker
// config and the running config both leave unset.
const (
	defaultRebalanceStrategy    = "spread"
	defaultRebalanceBatchSize   = 100
	defaultRebalanceThreshold   = 0.1
	defaultRebalanceConcurrency = 5
)

// RebalanceResult reports one rebalance cycle.
type RebalanceResult struct {
	Moved int
}

// RebalanceDeps holds the collaborators Rebalance requires.
type RebalanceDeps struct {
	Rebalancer RebalancerOps
	Runtime    RuntimeOps
	Config     *ConfigStore
}

// Rebalance serves the on-demand rebalance operation shared by the admin API
// and the web UI.
type Rebalance struct {
	log        *slog.Logger
	rebalancer RebalancerOps
	runtime    RuntimeOps
	cfg        *ConfigStore
}

// NewRebalance is the explicit-deps constructor. Rebalancer is nil when the
// worker pool is not wired, which Run reports as unavailable.
func NewRebalance(d RebalanceDeps) *Rebalance {
	must.NotNil("d.Runtime", d.Runtime)
	must.NotNil("d.Config", d.Config)
	return &Rebalance{
		log:        slog.Default().With(logfmt.Component("ops")),
		rebalancer: d.Rebalancer,
		runtime:    d.Runtime,
		cfg:        d.Config,
	}
}

// Run executes one rebalance cycle. observer, when non-nil, receives a step
// per move. Reports a skip when the rebalancer plans no moves, so a caller can
// tell that apart from a cycle that ran and moved nothing.
func (r *Rebalance) Run(ctx context.Context, observer progress.Observer) (RebalanceResult, error) {
	if r.rebalancer == nil {
		return RebalanceResult{}, ErrRebalancerUnavailable
	}

	sum, err := r.rebalancer.Rebalance(ctx, r.runConfig(), observer)
	if err != nil {
		return RebalanceResult{}, err
	}
	if sum.SkipReason != "" {
		return RebalanceResult{}, Skip(sum.SkipReason)
	}

	if mErr := r.runtime.UpdateQuotaMetrics(ctx); mErr != nil {
		r.log.WarnContext(ctx, "failed to update quota metrics after rebalance", "error", mErr)
	}

	r.log.InfoContext(ctx, "rebalance completed", "moved", sum.Succeeded)
	return RebalanceResult{Moved: sum.Succeeded}, nil
}

// runConfig resolves the settings for one manual cycle: whatever the worker
// holds, then the running config, then the defaults.
func (r *Rebalance) runConfig() config.RebalanceConfig {
	var runCfg config.RebalanceConfig
	switch {
	case r.rebalancer.Config() != nil:
		runCfg = *r.rebalancer.Config()
	case r.cfg.Load() != nil:
		runCfg = r.cfg.Load().Rebalance
	}

	runCfg.Strategy = cmp.Or(runCfg.Strategy, defaultRebalanceStrategy)
	runCfg.BatchSize = cmp.Or(runCfg.BatchSize, defaultRebalanceBatchSize)
	runCfg.Threshold = cmp.Or(runCfg.Threshold, defaultRebalanceThreshold)
	runCfg.Concurrency = cmp.Or(runCfg.Concurrency, defaultRebalanceConcurrency)
	return runCfg
}
