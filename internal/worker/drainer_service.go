// -------------------------------------------------------------------------------
// Drainer - Background Service Constructor
//
// Author: Alex Freidah
//
// Wraps *Drainer in a lifecycle.Runner backed by the shared advisory-locked
// ticker primitive. The tick only decides how soon a new or resumed drain is
// picked up: a pass works each drain until it finishes or stalls.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle/tickrunner"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// DefaultDrainTick is how often the drainer looks for drains to work.
const DefaultDrainTick = 10 * time.Second

// NewDrainerService constructs the drain background service.
func NewDrainerService(manager tickrunner.QuotaMetricsRefresher, drainer *Drainer, locker tickrunner.AdvisoryLocker) lifecycle.Runner {
	const slug = "drain"
	log := tickrunner.ComponentLogger(slug)
	return tickrunner.New(tickrunner.Config{
		Locker:   locker,
		Interval: DefaultDrainTick,
		LockID:   core.LockDrain,
		Name:     slug,
		Log:      log,
		Work: func(ctx context.Context) error {
			sum, err := drainer.Drain(ctx, nil)
			return tickrunner.HandlePassResult(ctx, log, manager, sum.Succeeded, err, "objects_moved")
		},
	})
}
