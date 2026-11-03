// -------------------------------------------------------------------------------
// Ops - Rebalance Operation
//
// Author: Alex Freidah
//
// One on-demand rebalance cycle, moving objects between backends until their
// utilisation converges. An operator who asks for a cycle gets one under the
// running config, whose defaults the config loader filled, so a manual run
// works on a fleet that never enabled scheduled rebalancing.
// -------------------------------------------------------------------------------

package ops

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// RebalanceDeps holds the collaborators Rebalance requires.
type RebalanceDeps struct {
	Rebalancer Rebalancer
	Config     *config.Live
}

// Rebalance serves the on-demand rebalance operation shared by the admin API
// and the web UI. The rebalancer reports its own passes.
type Rebalance struct {
	rebalancer Rebalancer
	cfg        *config.Live
}

// NewRebalance is the explicit-deps constructor. Rebalancer is nil when the
// worker pool is not wired, which Run reports as unavailable.
func NewRebalance(d RebalanceDeps) *Rebalance {
	must.NotNil("d.Config", d.Config)
	return &Rebalance{
		rebalancer: d.Rebalancer,
		cfg:        d.Config,
	}
}

// Run executes one rebalance cycle and reports its tally; Succeeded counts the
// objects moved. observer, when non-nil, receives a step per move. Reports a
// skip when the rebalancer plans no moves, so a caller can tell that apart from
// a cycle that ran and moved nothing.
func (r *Rebalance) Run(ctx context.Context, observer progress.Observer) (batch.Summary, error) {
	if r.rebalancer == nil {
		return batch.Summary{}, ErrRebalancerUnavailable
	}

	sum, err := r.rebalancer.Rebalance(ctx, r.cfg.Load().Rebalance, observer)
	if err != nil {
		return batch.Summary{}, err
	}
	if sum.SkipReason != "" {
		return batch.Summary{}, Skip(sum.SkipReason)
	}
	return sum.Summary, nil
}
