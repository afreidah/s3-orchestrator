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
	"log/slog"
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
			return scrubCycle(ctx, scrubber, log)
		},
	})
}

// scrubCycle runs one scrub pass and reports what it found. Named rather than
// inlined into the tick closure so the reporting can be asserted without
// driving a ticker.
func scrubCycle(ctx context.Context, scrubber *Scrubber, log *slog.Logger) error {
	icfg := scrubber.Config()
	if icfg == nil {
		return nil
	}
	sum := scrubber.Scrub(ctx, icfg.ScrubberBatchSize, nil)
	// Skipped is excluded from Attempted, so a cycle whose every copy was
	// unreadable used to satisfy neither arm and log nothing at all - the
	// quietest possible report of the loudest problem.
	if sum.Attempted > 0 || sum.Failed > 0 || sum.Skipped > 0 {
		log.InfoContext(ctx, "scrub completed",
			"checked", sum.Attempted, "failed", sum.Failed, "unreadable", sum.Skipped)
	}
	return nil
}
