// -------------------------------------------------------------------------------
// Ops - Lifecycle Expiration Operation
//
// Author: Alex Freidah
//
// One on-demand expiration sweep, so an operator who has just written or
// corrected a lifecycle rule can find out whether it matches anything without
// waiting out the tick. That wait is an hour plus startup jitter, and until it
// passes a rule that matches nothing looks exactly like a rule that ran and
// found nothing expired.
// -------------------------------------------------------------------------------

package ops

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

// LifecycleDeps holds the collaborators Lifecycle requires.
type LifecycleDeps struct {
	Expiry RuleProcessor
	Config *config.Live
}

// Lifecycle serves the on-demand expiration sweep shared by the admin API and
// the web UI.
type Lifecycle struct {
	expiry RuleProcessor
	cfg    *config.Live
}

// NewLifecycle is the explicit-deps constructor. Expiry is nil when the
// manager is not wired, which Run reports as unavailable.
func NewLifecycle(d LifecycleDeps) *Lifecycle {
	must.NotNil("d.Config", d.Config)
	return &Lifecycle{expiry: d.Expiry, cfg: d.Config}
}

// Run applies every configured rule once and reports its tally; Succeeded
// counts the objects deleted and Failed the ones a rule selected but could not
// delete. It declines when no rules are configured, so an empty config is not
// mistaken for a sweep that found nothing.
//
// It does not take the scheduled tick's advisory lock, so a manual sweep can
// overlap a scheduled one; applyRule is idempotent, so that is harmless.
func (l *Lifecycle) Run(ctx context.Context, observer progress.Observer) (batch.Summary, error) {
	if l.expiry == nil {
		return batch.Summary{}, ErrLifecycleUnavailable
	}
	rules := l.cfg.Load().Lifecycle.Rules
	if len(rules) == 0 {
		return batch.Summary{}, Skip("no lifecycle rules are configured")
	}

	return l.expiry.ProcessRules(ctx, rules, observer), nil
}
