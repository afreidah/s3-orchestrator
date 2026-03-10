// -------------------------------------------------------------------------------
// Cleanup Worker - Background Retry Worker
//
// Author: Alex Freidah
//
// Processes failed object cleanup operations from the retry queue. Uses
// exponential backoff (1 minute to 24 hours) with a maximum of 10 attempts.
// Enqueue is best-effort to avoid cascading failures when the database is down.
// -------------------------------------------------------------------------------

package storage

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/audit"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/workerpool"
)

// CleanupWorker processes the retry queue for failed object deletions.
type CleanupWorker struct {
	*backendCore
	concurrency int
}

// NewCleanupWorker creates a CleanupWorker sharing the given core infrastructure.
func NewCleanupWorker(core *backendCore, concurrency int) *CleanupWorker {
	return &CleanupWorker{backendCore: core, concurrency: concurrency}
}

// maxCleanupAttempts is the maximum number of retries before giving up.
const maxCleanupAttempts = 10

// cleanupBackoff returns the backoff duration for the given attempt number.
// Uses exponential backoff: min(1m * 2^attempts, 24h).
func cleanupBackoff(attempts int32) time.Duration {
	// Clamp before shifting to prevent int64 overflow at attempts >= 33
	if attempts > 20 {
		return 24 * time.Hour
	}
	d := time.Minute * (1 << attempts)
	if d > 24*time.Hour {
		d = 24 * time.Hour
	}
	return d
}

// ProcessCleanupQueue fetches pending cleanup items and attempts to delete the
// orphaned objects from their respective backends. Returns the number of items
// successfully processed and the number that failed.
func (w *CleanupWorker) ProcessCleanupQueue(ctx context.Context) (processed, failed int) {
	ctx, span := telemetry.StartSpan(ctx, "ProcessCleanupQueue",
		telemetry.AttrOperation.String("cleanup_queue"),
	)
	defer span.End()

	items, err := w.store.GetPendingCleanups(ctx, 50)
	if err != nil {
		slog.Error("Failed to fetch pending cleanups", "error", err)
		return 0, 0
	}

	var processedCount, failedCount atomic.Int32

	workerpool.Run(ctx, w.concurrency, items, func(ctx context.Context, item CleanupItem) {
		backend, ok := w.backends[item.BackendName]
		if !ok {
			slog.Warn("Cleanup queue: backend not found, removing item",
				"backend", item.BackendName, "key", item.ObjectKey)
			if err := w.store.CompleteCleanupItem(ctx, item.ID); err != nil {
				slog.Error("Failed to complete cleanup item", "id", item.ID, "error", err)
			}
			telemetry.CleanupQueueProcessedTotal.WithLabelValues("success").Inc()
			processedCount.Add(1)
			return
		}

		delErr := w.deleteWithTimeout(ctx, backend, item.ObjectKey)
		w.usage.Record(item.BackendName, 1, 0, 0)

		if delErr == nil {
			if err := w.store.CompleteCleanupItem(ctx, item.ID); err != nil {
				slog.Error("Failed to complete cleanup item", "id", item.ID, "error", err)
			}
			if item.SizeBytes > 0 {
				if err := w.store.DecrementOrphanBytes(ctx, item.BackendName, item.SizeBytes); err != nil {
					slog.Error("Failed to decrement orphan bytes",
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

		// Retry or exhaust
		newAttempts := item.Attempts + 1
		if newAttempts >= maxCleanupAttempts {
			// Leave the item in the queue — the SQL filter (attempts < 10)
			// prevents it from being re-fetched, but orphan_bytes stays
			// incremented so the write path won't overcommit. The item
			// remains visible for operator intervention.
			slog.Error("Cleanup queue: max attempts reached, item remains for operator review",
				"key", item.ObjectKey, "backend", item.BackendName,
				"attempts", newAttempts, "size", item.SizeBytes, "error", delErr)
			// Persist the final attempt count and error
			if err := w.store.RetryCleanupItem(ctx, item.ID, 0, delErr.Error()); err != nil {
				slog.Error("Failed to update exhausted cleanup item", "id", item.ID, "error", err)
			}
			telemetry.CleanupQueueProcessedTotal.WithLabelValues("exhausted").Inc()
			failedCount.Add(1)
			return
		}

		telemetry.CleanupQueueProcessedTotal.WithLabelValues("retry").Inc()
		backoff := cleanupBackoff(item.Attempts)
		if err := w.store.RetryCleanupItem(ctx, item.ID, backoff, delErr.Error()); err != nil {
			slog.Error("Failed to update cleanup retry", "id", item.ID, "error", err)
		}
		failedCount.Add(1)
	})

	// Update queue depth gauge
	depth, err := w.store.CleanupQueueDepth(ctx)
	if err == nil {
		telemetry.CleanupQueueDepth.Set(float64(depth))
	}

	return int(processedCount.Load()), int(failedCount.Load())
}
