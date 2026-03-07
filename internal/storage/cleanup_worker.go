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
	"time"

	"github.com/afreidah/s3-orchestrator/internal/audit"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"
)

// CleanupWorker processes the retry queue for failed object deletions.
type CleanupWorker struct {
	*backendCore
}

// NewCleanupWorker creates a CleanupWorker sharing the given core infrastructure.
func NewCleanupWorker(core *backendCore) *CleanupWorker {
	return &CleanupWorker{backendCore: core}
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
	items, err := w.store.GetPendingCleanups(ctx, 50)
	if err != nil {
		slog.Error("Failed to fetch pending cleanups", "error", err)
		return 0, 0
	}

	for _, item := range items {
		backend, ok := w.backends[item.BackendName]
		if !ok {
			slog.Warn("Cleanup queue: backend not found, removing item",
				"backend", item.BackendName, "key", item.ObjectKey)
			if err := w.store.CompleteCleanupItem(ctx, item.ID); err != nil {
				slog.Error("Failed to complete cleanup item", "id", item.ID, "error", err)
			}
			telemetry.CleanupQueueProcessedTotal.WithLabelValues("success").Inc()
			processed++
			continue
		}

		delErr := w.deleteWithTimeout(ctx, backend, item.ObjectKey)
		w.usage.Record(item.BackendName, 1, 0, 0)

		if delErr == nil {
			if err := w.store.CompleteCleanupItem(ctx, item.ID); err != nil {
				slog.Error("Failed to complete cleanup item", "id", item.ID, "error", err)
			}
			telemetry.CleanupQueueProcessedTotal.WithLabelValues("success").Inc()
			processed++

			audit.Log(ctx, "cleanup_queue.processed",
				slog.String("key", item.ObjectKey),
				slog.String("backend", item.BackendName),
				slog.String("reason", item.Reason),
				slog.Int("attempt", int(item.Attempts+1)),
			)
			continue
		}

		// Retry or exhaust
		newAttempts := item.Attempts + 1
		if newAttempts >= maxCleanupAttempts {
			slog.Error("Cleanup queue: max attempts reached, removing item",
				"key", item.ObjectKey, "backend", item.BackendName,
				"attempts", newAttempts, "error", delErr)
			if err := w.store.CompleteCleanupItem(ctx, item.ID); err != nil {
				slog.Error("Failed to remove exhausted cleanup item", "id", item.ID, "error", err)
			}
			telemetry.CleanupQueueProcessedTotal.WithLabelValues("exhausted").Inc()
			failed++
			continue
		}

		telemetry.CleanupQueueProcessedTotal.WithLabelValues("retry").Inc()
		backoff := cleanupBackoff(item.Attempts)
		if err := w.store.RetryCleanupItem(ctx, item.ID, backoff, delErr.Error()); err != nil {
			slog.Error("Failed to update cleanup retry", "id", item.ID, "error", err)
		}
		failed++
	}

	// Update queue depth gauge
	depth, err := w.store.CleanupQueueDepth(ctx)
	if err == nil {
		telemetry.CleanupQueueDepth.Set(float64(depth))
	}

	return processed, failed
}
