// -------------------------------------------------------------------------------
// Scrubber - Background Service Constructor
//
// Author: Alex Freidah
//
// Wraps *Scrubber in a lifecycle.Runner backed by the shared
// advisory-locked ticker primitive.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle/tickrunner"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// DefaultScrubberInterval is the integrity scrubber's per-tick cadence
// when the integrity config does not specify one.
const DefaultScrubberInterval = 6 * time.Hour

// NewScrubberService constructs the integrity scrubber background service.
func NewScrubberService(scrubber *Scrubber, locker tickrunner.AdvisoryLocker) lifecycle.Runner {
	interval := DefaultScrubberInterval
	if icfg := scrubber.Config(); icfg != nil && icfg.ScrubberInterval > 0 {
		interval = icfg.ScrubberInterval
	}
	const slug = "scrubber"
	log := tickrunner.ComponentLogger(slug)
	return tickrunner.New(tickrunner.Config{
		Locker:   locker,
		Interval: interval,
		LockID:   core.LockScrubber,
		Name:     slug,
		Log:      log,
		ShouldRun: func() bool {
			icfg := scrubber.Config()
			return icfg != nil && icfg.Enabled && icfg.ScrubberInterval > 0
		},
		Work: func(ctx context.Context) error {
			icfg := scrubber.Config()
			if icfg == nil {
				return nil
			}
			checked, failed := scrubber.Scrub(ctx, icfg.ScrubberBatchSize, nil)
			if checked > 0 || failed > 0 {
				log.InfoContext(ctx, "scrub completed", "checked", checked, "failed", failed)
			}
			return nil
		},
	})
}
