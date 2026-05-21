// -------------------------------------------------------------------------------
// Reconciler - Background Service Constructor
//
// Author: Alex Freidah
//
// Wraps *Reconciler in a lifecycle.Runner backed by the shared
// advisory-locked ticker primitive (#925).
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle/tickrunner"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// NewReconcileService constructs the reconcile background service.
func NewReconcileService(reconciler *Reconciler, locker tickrunner.AdvisoryLocker, interval time.Duration) lifecycle.Runner {
	const slug = "reconcile"
	return tickrunner.New(tickrunner.Config{
		Locker:   locker,
		Interval: interval,
		LockID:   core.LockReconcile,
		Name:     slug,
		Log:      tickrunner.ComponentLogger(slug),
		Work: func(ctx context.Context) error {
			reconciler.Run(ctx)
			return nil
		},
	})
}
