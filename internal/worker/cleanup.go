// -------------------------------------------------------------------------------
// Cleanup Worker - Background Retry Worker
//
// Author: Alex Freidah
//
// Processes failed object cleanup operations from the retry queue. Uses
// exponential backoff (1 minute to 24 hours) with a maximum of 10 attempts.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/audit"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/workerpool"
)

// CleanupWorker processes the retry queue for failed object deletions.
type CleanupWorker struct {
	ops         Ops
	concurrency int
}

// NewCleanupWorker creates a CleanupWorker with explicit dependencies.
func NewCleanupWorker(ops Ops, concurrency int) *CleanupWorker {
	return &CleanupWorker{ops: ops, concurrency: concurrency}
}

const maxCleanupAttempts = 10

func CleanupBackoff(attempts int32) time.Duration {
	if attempts > 20 {
		return 24 * time.Hour
	}
	return min(time.Minute*(1<<attempts), 24*time.Hour)
}

// ProcessCleanupQueue fetches pending cleanup items and attempts to delete the
// orphaned objects from their respective backends.
func (w *CleanupWorker) ProcessCleanupQueue(ctx context.Context) (processed, failed int) {
	ctx, span := telemetry.StartSpan(ctx, "ProcessCleanupQueue",
		telemetry.AttrOperation.String("cleanup_queue"),
	)
	defer span.End()

	st := w.ops.Store()
	items, err := st.GetPendingCleanups(ctx, 50)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to fetch pending cleanups", "error", err)
		return 0, 0
	}

	var processedCount, failedCount atomic.Int32

	workerpool.Run(ctx, w.concurrency, items, func(ctx context.Context, item store.CleanupItem) {
		if !w.ops.AcquireAdmission(ctx) {
			return
		}
		defer w.ops.ReleaseAdmission()

		be, err := w.ops.GetBackend(item.BackendName)
		if err != nil {
			slog.WarnContext(ctx, "Cleanup queue: backend not found, removing item",
				"backend", item.BackendName, "key", item.ObjectKey)
			if err := st.CompleteCleanupItem(ctx, item.ID); err != nil {
				slog.ErrorContext(ctx, "Failed to complete cleanup item", "id", item.ID, "error", err)
			}
			telemetry.CleanupQueueProcessedTotal.WithLabelValues("success").Inc()
			processedCount.Add(1)
			return
		}

		delErr := w.ops.DeleteWithTimeout(ctx, be, item.ObjectKey)
		w.ops.Usage().Record(item.BackendName, 1, 0, 0)

		if delErr == nil {
			if err := st.CompleteCleanupItem(ctx, item.ID); err != nil {
				slog.ErrorContext(ctx, "Failed to complete cleanup item", "id", item.ID, "error", err)
			}
			if item.SizeBytes > 0 {
				if err := st.DecrementOrphanBytes(ctx, item.BackendName, item.SizeBytes); err != nil {
					slog.ErrorContext(ctx, "Failed to decrement orphan bytes",
						"backend", item.BackendName, "size", item.SizeBytes, "error", err)
				}
			}
			telemetry.CleanupQueueProcessedTotal.WithLabelValues("success").Inc()
			processedCount.Add(1)
			audit.Log(ctx, "cleanup_queue.processed",
				slog.String("key", item.ObjectKey),
				slog.String("backend", item.BackendName),
				slog.String("reason", item.Reason),
				slog.Int("attempt", int(item.Attempts+1)),
			)
			return
		}

		newAttempts := item.Attempts + 1
		if newAttempts >= maxCleanupAttempts {
			slog.ErrorContext(ctx, "Cleanup queue: max attempts reached",
				"key", item.ObjectKey, "backend", item.BackendName,
				"attempts", newAttempts, "size", item.SizeBytes, "error", delErr)
			if err := st.RetryCleanupItem(ctx, item.ID, 0, delErr.Error()); err != nil {
				slog.ErrorContext(ctx, "Failed to update exhausted cleanup item", "id", item.ID, "error", err)
			}
			telemetry.CleanupQueueProcessedTotal.WithLabelValues("exhausted").Inc()
			failedCount.Add(1)
			return
		}

		telemetry.CleanupQueueProcessedTotal.WithLabelValues("retry").Inc()
		backoff := CleanupBackoff(item.Attempts)
		if err := st.RetryCleanupItem(ctx, item.ID, backoff, delErr.Error()); err != nil {
			slog.ErrorContext(ctx, "Failed to update cleanup retry", "id", item.ID, "error", err)
		}
		failedCount.Add(1)
	})

	depth, err := st.CleanupQueueDepth(ctx)
	if err == nil {
		telemetry.CleanupQueueDepth.Set(float64(depth))
	}

	return int(processedCount.Load()), int(failedCount.Load())
}
