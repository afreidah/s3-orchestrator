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
	"log/slog"

	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
)

// LifecycleDeps holds the collaborators Lifecycle requires.
type LifecycleDeps struct {
	Expiry LifecycleOps
}

// Lifecycle serves the on-demand expiration sweep shared by the admin API and
// the web UI.
type Lifecycle struct {
	log    *slog.Logger
	expiry LifecycleOps
}

// NewLifecycle is the explicit-deps constructor. Expiry is nil when the
// manager is not wired, which Run reports as unavailable.
func NewLifecycle(d LifecycleDeps) *Lifecycle {
	return &Lifecycle{
		log:    slog.Default().With(logfmt.Component("ops")),
		expiry: d.Expiry,
	}
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
	cfg := l.expiry.Config()
	if cfg == nil || len(cfg.Rules) == 0 {
		return batch.Summary{}, Skip("no lifecycle rules are configured")
	}

	sum := l.expiry.ProcessRules(ctx, cfg.Rules, observer)

	event.Publish(event.LifecycleCompleted, "", map[string]any{
		"deleted": sum.Succeeded,
		"failed":  sum.Failed,
	})
	l.log.InfoContext(ctx, "expiration completed", "deleted", sum.Succeeded, "failed", sum.Failed)
	return sum, nil
}
