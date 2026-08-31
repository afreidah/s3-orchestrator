// -------------------------------------------------------------------------------
// CleanupWorker - Background Service Constructor
//
// Author: Alex Freidah
//
// Wraps *CleanupWorker in a lifecycle.Runner backed by the shared
// advisory-locked ticker primitive. Lives next to the worker itself
// (rather than in internal/di) so the run-loop semantics ship with the
// worker that owns the work.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle/tickrunner"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// DefaultCleanupQueueTick is the cleanup-queue worker's per-tick
// cadence when the config does not override it.
const DefaultCleanupQueueTick = 1 * time.Minute

// NewCleanupQueueService constructs the cleanup-queue background service.
func NewCleanupQueueService(cleanup *CleanupWorker, locker tickrunner.AdvisoryLocker) lifecycle.Runner {
	const slug = "cleanup_queue"
	log := tickrunner.ComponentLogger(slug)
	return tickrunner.New(tickrunner.Config{
		Locker:   locker,
		Interval: DefaultCleanupQueueTick,
		LockID:   core.LockCleanupQueue,
		Name:     slug,
		Log:      log,
		Work: tickrunner.QueueWork(log, "queue processed", func(ctx context.Context) (int, int) {
			sum := cleanup.ProcessCleanupQueue(ctx)
			return sum.Succeeded, sum.Failed
		}),
	})
}
