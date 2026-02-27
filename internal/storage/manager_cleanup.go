// -------------------------------------------------------------------------------
// Cleanup Queue Manager - Background Retry Worker
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

// maxCleanupAttempts is the maximum number of retries before giving up.
const maxCleanupAttempts = 10

// cleanupBackoff returns the backoff duration for the given attempt number.
// Uses exponential backoff: min(1m * 2^attempts, 24h).
func cleanupBackoff(attempts int32) time.Duration {
	d := time.Minute * (1 << attempts)
	if d > 24*time.Hour {
		d = 24 * time.Hour
	}
	return d
}

// enqueueCleanup adds a failed cleanup operation to the retry queue.
// Best-effort: if the enqueue itself fails (e.g. DB down), logs the error
// and moves on since the circuit breaker is already handling DB outages.
func (m *BackendManager) enqueueCleanup(ctx context.Context, backendName, objectKey, reason string) {
	if err := m.store.EnqueueCleanup(ctx, backendName, objectKey, reason); err != nil {
		slog.Error("Failed to enqueue cleanup (best-effort)",
			"backend", backendName, "key", objectKey, "reason", reason, "error", err)
		return
	}
	telemetry.CleanupQueueEnqueuedTotal.WithLabelValues(reason).Inc()
}

// ProcessCleanupQueue fetches pending cleanup items and attempts to delete the
// orphaned objects from their respective backends. Returns the number of items
// successfully processed and the number that failed.
func (m *BackendManager) ProcessCleanupQueue(ctx context.Context) (processed, failed int) {
	items, err := m.store.GetPendingCleanups(ctx, 50)
	if err != nil {
		slog.Error("Failed to fetch pending cleanups", "error", err)
		return 0, 0
	}

	for _, item := range items {
		backend, ok := m.backends[item.BackendName]
		if !ok {
			slog.Warn("Cleanup queue: backend not found, removing item",
				"backend", item.BackendName, "key", item.ObjectKey)
			if err := m.store.CompleteCleanupItem(ctx, item.ID); err != nil {
				slog.Error("Failed to complete cleanup item", "id", item.ID, "error", err)
			}
			telemetry.CleanupQueueProcessedTotal.WithLabelValues("success").Inc()
			processed++
			continue
		}

		dctx, dcancel := m.withTimeout(ctx)
		delErr := backend.DeleteObject(dctx, item.ObjectKey)
		dcancel()

		if delErr == nil {
			if err := m.store.CompleteCleanupItem(ctx, item.ID); err != nil {
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
			slog.Error("Cleanup queue: max attempts reached",
				"key", item.ObjectKey, "backend", item.BackendName,
				"attempts", newAttempts, "error", delErr)
			telemetry.CleanupQueueProcessedTotal.WithLabelValues("exhausted").Inc()
		} else {
			telemetry.CleanupQueueProcessedTotal.WithLabelValues("retry").Inc()
		}

		backoff := cleanupBackoff(item.Attempts)
		if err := m.store.RetryCleanupItem(ctx, item.ID, backoff, delErr.Error()); err != nil {
			slog.Error("Failed to update cleanup retry", "id", item.ID, "error", err)
		}
		failed++
	}

	// Update queue depth gauge
	depth, err := m.store.CleanupQueueDepth(ctx)
	if err == nil {
		telemetry.CleanupQueueDepth.Set(float64(depth))
	}

	return processed, failed
}
