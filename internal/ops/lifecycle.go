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
)

// LifecycleResult reports one expiration sweep. Failed counts the objects a
// rule selected but could not delete, so a sweep that deleted nothing because
// everything failed does not read the same as one with nothing to expire.
type LifecycleResult struct {
	Deleted int
	Failed  int
}

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

// Run applies every configured rule once and reports what it removed.
//
// Declines when no rules are configured rather than reporting a sweep of zero:
// those answer different questions, and an operator checking a rule they just
// wrote needs to know the config never reached the process.
//
// The advisory lock the scheduled tick holds is deliberately not taken, which
// matches every other manual trigger. A manual sweep can therefore overlap a
// scheduled one; applyRule is idempotent and a doubled delete of the same key
// is harmless.
func (l *Lifecycle) Run(ctx context.Context) (LifecycleResult, error) {
	if l.expiry == nil {
		return LifecycleResult{}, ErrLifecycleUnavailable
	}
	cfg := l.expiry.Config()
	if cfg == nil || len(cfg.Rules) == 0 {
		return LifecycleResult{}, Skip("no lifecycle rules are configured")
	}

	deleted, failed := l.expiry.ProcessRules(ctx, cfg.Rules)

	event.Publish(event.LifecycleCompleted, "", map[string]any{
		"deleted": deleted,
		"failed":  failed,
	})
	l.log.InfoContext(ctx, "expiration completed", "deleted", deleted, "failed", failed)
	return LifecycleResult{Deleted: deleted, Failed: failed}, nil
}
