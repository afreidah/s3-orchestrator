// -------------------------------------------------------------------------------
// PendingReaper - Background Service Constructor
//
// Author: Alex Freidah
//
// Wraps *PendingReaper in a lifecycle.Runner backed by the shared
// advisory-locked ticker primitive. Returns nil when the reaper is
// disabled so the lifecycle manager can skip registration without a
// dependency hole.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle/tickrunner"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// DefaultPendingReaperTick is the pending reaper's per-tick cadence
// when the config does not specify one.
const DefaultPendingReaperTick = 1 * time.Minute

// NewPendingReaperService constructs the pending-objects reaper
// background service. The reaper resolves abandoned PUT intents by
// HEADing the destination backend and either promoting the intent into
// object_locations (bytes present) or dropping it (bytes absent).
// Returns nil when no pending reaper is configured.
func NewPendingReaperService(reaper *PendingReaper, locker tickrunner.AdvisoryLocker, tick time.Duration) lifecycle.Runner {
	if reaper == nil {
		return nil
	}
	if tick <= 0 {
		tick = DefaultPendingReaperTick
	}
	const slug = "pending_reaper"
	log := tickrunner.ComponentLogger(slug)
	return tickrunner.New(tickrunner.Config{
		Locker:   locker,
		Interval: tick,
		LockID:   core.LockPendingReaper,
		Name:     slug,
		Log:      log,
		Work: func(ctx context.Context) error {
			sum := reaper.ProcessPendingQueue(ctx)
			if sum.Succeeded > 0 || sum.Failed > 0 {
				log.InfoContext(ctx, "pending queue processed", "resolved", sum.Succeeded, "failed", sum.Failed)
			}
			return nil
		},
	})
}
