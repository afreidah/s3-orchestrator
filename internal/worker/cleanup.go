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

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/workerpool"
)

// CleanupWorker processes the retry queue for failed object deletions.
type CleanupWorker struct {
	deps        CleanupOps
	store       CleanupWorkerStore
	concurrency int
}

// NewCleanupWorker creates a CleanupWorker with explicit dependencies.
func NewCleanupWorker(deps CleanupOps, store CleanupWorkerStore, concurrency int) *CleanupWorker {
	return &CleanupWorker{deps: deps, store: store, concurrency: concurrency}
}

// maxCleanupAttempts is the retry ceiling. The 1-minute starting
// backoff doubled 10 times yields ~17 hours of total retry runway,
// which is enough to bridge the longest realistic backend outages.
// Beyond that the row graduates to cleanup_dlq for operator action.
const maxCleanupAttempts = 10

// CleanupBackoff returns the backoff duration for the given attempt number.
// Uses exponential backoff: min(1m * 2^attempts, 24h). Short-circuits the
// shift for attempts >= 11 (where the doubling already exceeds the cap) and
// for negative inputs, since shifting by a negative or out-of-range count is
// undefined in Go.
func CleanupBackoff(attempts int32) time.Duration {
	const maxBackoff = 24 * time.Hour
	if attempts < 0 || attempts >= 11 {
		return maxBackoff
	}
	return min(time.Minute<<attempts, maxBackoff)
}

// ProcessCleanupQueue fetches pending cleanup items and attempts to delete the
// orphaned objects from their respective backends.
func (w *CleanupWorker) ProcessCleanupQueue(ctx context.Context) (processed, failed int) {
	ctx, span := telemetry.StartSpan(ctx, "ProcessCleanupQueue",
		telemetry.AttrOperation.String("cleanup_queue"),
	)
	defer span.End()

	items, err := w.store.GetPendingCleanups(ctx, 50)
	if err != nil {
		slog.ErrorContext(ctx, "failed to fetch pending cleanups", "error", err)
		return 0, 0
	}

	var processedCount, failedCount atomic.Int32

	workerpool.Run(ctx, w.concurrency, items, func(ctx context.Context, item core.CleanupItem) {
		if !w.deps.AcquireAdmission(ctx) {
			telemetry.WorkerAdmissionRejectionsTotal.WithLabelValues("cleanup").Inc()
			return
		}
		defer w.deps.ReleaseAdmission()

		be, err := w.deps.GetBackend(item.BackendName)
		if err != nil {
			slog.WarnContext(ctx, "Cleanup queue: backend not found, removing item",
				"backend", item.BackendName, "key", item.ObjectKey)
			if err := w.store.CompleteCleanupItem(ctx, item.ID); err != nil {
				slog.ErrorContext(ctx, "failed to complete cleanup item", "id", item.ID, "error", err)
			}
			telemetry.CleanupQueueProcessedTotal.WithLabelValues("success").Inc()
			processedCount.Add(1)
			return
		}

		delErr := w.deps.DeleteWithTimeout(ctx, be, item.ObjectKey)
		w.deps.Usage().Record(item.BackendName, 1, 0, 0)

		if delErr == nil {
			if err := w.store.CompleteCleanupItem(ctx, item.ID); err != nil {
				slog.ErrorContext(ctx, "failed to complete cleanup item", "id", item.ID, "error", err)
			}
			if item.SizeBytes > 0 {
				if err := w.store.DecrementOrphanBytes(ctx, item.BackendName, item.SizeBytes); err != nil {
					slog.ErrorContext(ctx, "failed to decrement orphan bytes",
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
			slog.ErrorContext(ctx, "Cleanup queue: max attempts reached, moving to DLQ",
				"key", item.ObjectKey, "backend", item.BackendName,
				"attempts", newAttempts, "size", item.SizeBytes, "error", delErr)
			moved, mvErr := w.store.MoveCleanupToDLQ(ctx, item.ID, delErr.Error())
			if mvErr != nil {
				slog.ErrorContext(ctx, "failed to move cleanup item to DLQ",
					"id", item.ID, "error", mvErr)
				telemetry.CleanupQueueProcessedTotal.WithLabelValues("exhausted").Inc()
				failedCount.Add(1)
				return
			}
			if moved {
				telemetry.CleanupDLQEnqueuedTotal.WithLabelValues(item.BackendName).Inc()
				audit.Log(ctx, "cleanup_queue.exhausted_to_dlq",
					slog.String("key", item.ObjectKey),
					slog.String("backend", item.BackendName),
					slog.String("reason", item.Reason),
					slog.Int("attempts", int(newAttempts)),
					slog.Int64("size_bytes", item.SizeBytes),
					slog.String("last_error", delErr.Error()),
				)
				if event.Emit != nil {
					event.Emit(event.Event{
						Type:    event.CleanupExhausted,
						Subject: item.BackendName,
						Data: map[string]any{
							"backend":    item.BackendName,
							"object_key": item.ObjectKey,
							"reason":     item.Reason,
							"attempts":   int(newAttempts),
							"size_bytes": item.SizeBytes,
							"last_error": delErr.Error(),
						},
					})
				}
			}
			telemetry.CleanupQueueProcessedTotal.WithLabelValues("exhausted").Inc()
			failedCount.Add(1)
			return
		}

		telemetry.CleanupQueueProcessedTotal.WithLabelValues("retry").Inc()
		backoff := CleanupBackoff(item.Attempts)
		if err := w.store.RetryCleanupItem(ctx, item.ID, backoff, delErr.Error()); err != nil {
			slog.ErrorContext(ctx, "failed to update cleanup retry", "id", item.ID, "error", err)
		}
		failedCount.Add(1)
	})

	depth, err := w.store.CleanupQueueDepth(ctx)
	if err == nil {
		telemetry.CleanupQueueDepth.Set(float64(depth))
	}
	dlqDepth, err := w.store.CleanupDLQDepth(ctx)
	if err == nil {
		telemetry.CleanupDLQDepth.Set(float64(dlqDepth))
	}

	return int(processedCount.Load()), int(failedCount.Load())
}
