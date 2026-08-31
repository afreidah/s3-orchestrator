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
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

// CleanupWorker processes the retry queue for failed object deletions.
type CleanupWorker struct {
	log              *slog.Logger
	deps             CleanupOps
	store            core.CleanupStore
	concurrency      int
	instanceID       string
	claimGracePeriod time.Duration
}

// CleanupWorkerDeps groups the cleanup worker's constructor parameters.
// InstanceID is stamped into cleanup_queue.claimed_by for observability;
// ClaimGracePeriod is the threshold past which an outstanding claim becomes
// reclaimable by another worker tick (typically 5m).
type CleanupWorkerDeps struct {
	Ops              CleanupOps
	Store            core.CleanupStore
	Concurrency      int
	InstanceID       string
	ClaimGracePeriod time.Duration
}

// NewCleanupWorker creates a CleanupWorker with the given dependencies.
func NewCleanupWorker(deps CleanupWorkerDeps) *CleanupWorker {
	must.NotNil("Ops", deps.Ops)
	must.NotNil("Store", deps.Store)
	return &CleanupWorker{
		deps:             deps.Ops,
		store:            deps.Store,
		concurrency:      deps.Concurrency,
		instanceID:       deps.InstanceID,
		claimGracePeriod: deps.ClaimGracePeriod,
		log:              slog.Default().With(logfmt.Component("cleanup_worker")),
	}
}

// maxCleanupAttempts is the retry ceiling. The 1-minute starting
// backoff doubled 10 times yields ~17 hours of total retry runway,
// which is enough to bridge the longest realistic backend outages.
// Beyond that the row graduates to cleanup_dlq for operator action.
const maxCleanupAttempts = 10

// logMsgCompleteCleanupFailed is the shared error log message emitted
// when CompleteCleanupItem fails. Hoisted to a constant so the three
// completion paths (success, success_absent, unknown_backend) stay in
// lockstep and the SonarQube duplicate-literal rule (S1192) stays
// satisfied.
const logMsgCompleteCleanupFailed = "failed to complete cleanup item"

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

// ProcessCleanupQueue fetches pending cleanup items and attempts to
// delete the orphaned objects from their respective backends.
func (w *CleanupWorker) ProcessCleanupQueue(ctx context.Context) WorkSummary {
	return runTickCycle(ctx, "ProcessCleanupQueue", "cleanup_queue", w.processCleanupQueue)
}

// processCleanupQueue is the body of ProcessCleanupQueue after the span is open.
func (w *CleanupWorker) processCleanupQueue(ctx context.Context) WorkSummary {
	graceCutoff := time.Now().Add(-w.claimGracePeriod)
	items, err := w.store.ClaimPendingCleanups(ctx, 50, w.instanceID, graceCutoff)
	if err != nil {
		w.log.ErrorContext(ctx, "failed to claim pending cleanups", "error", err)
		return WorkSummary{}
	}

	for _, item := range items {
		if !item.Reclaimed {
			continue
		}
		telemetry.CleanupQueueStaleClaimsRecoveredTotal.WithLabelValues(item.BackendName).Inc()
		w.log.WarnContext(ctx, "reclaimed stale cleanup_queue claim",
			slog.Int64("cleanup_id", item.ID),
			slog.String("backend", item.BackendName),
			slog.String("key", item.ObjectKey),
		)
		audit.Log(ctx, "cleanup_queue.claim_recovered",
			slog.Int64("cleanup_id", item.ID),
			slog.String("backend", item.BackendName),
			slog.String("key", item.ObjectKey),
			slog.String("reclaimed_by", w.instanceID),
		)
	}

	runner := BatchRunner[core.CleanupItem]{Name: "cleanup", Log: w.log, Concurrency: w.concurrency}
	sum := runner.Run(ctx, items, func(ctx context.Context, item core.CleanupItem) ItemResult {
		var res ItemResult
		WithAdmission(ctx, w.deps, WorkerNameCleanup, func() {
			res = w.processCleanupItem(ctx, &item)
		})
		return res
	})

	w.recordCleanupDepths(ctx)
	return sum
}

// processCleanupItem handles one cleanup queue row: resolve the backend,
// attempt the delete, and either complete, retry, or graduate the row to
// the DLQ depending on the outcome.
func (w *CleanupWorker) processCleanupItem(ctx context.Context, item *core.CleanupItem) ItemResult {
	be, err := w.deps.GetBackend(item.BackendName)
	if err != nil {
		w.completeUnknownBackendItem(ctx, item)
		return ItemResult{Outcome: ItemSucceeded, Status: "success"}
	}

	delErr := w.deps.DeleteWithTimeout(ctx, be, item.ObjectKey)
	w.deps.Acct().APICall(item.BackendName)

	if delErr == nil {
		w.completeCleanupSuccess(ctx, item)
		return ItemResult{Outcome: ItemSucceeded, Status: "success"}
	}

	// 404 means the backend already agrees the object is gone, which is
	// the desired end state. Drop the row so we don't burn 9 retries +
	// a DLQ slot on a non-event.
	if backend.IsNotFound(delErr) {
		w.completeCleanupAlreadyAbsent(ctx, item)
		return ItemResult{Outcome: ItemSucceeded, Status: "success_absent"}
	}

	newAttempts := item.Attempts + 1
	if newAttempts >= maxCleanupAttempts {
		w.exhaustCleanupToDLQ(ctx, item, newAttempts, delErr)
		return ItemResult{Outcome: ItemFailed, Status: "exhausted"}
	}
	w.scheduleCleanupRetry(ctx, item, delErr)
	return ItemResult{Outcome: ItemFailed, Status: "retry"}
}

// completeCleanupAlreadyAbsent retires a cleanup row whose backend
// DELETE returned 404. Mirrors completeCleanupSuccess but emits the
// status="success_absent" metric label and an audit subject that lets
// operators distinguish "we deleted it" from "it was already gone" on
// dashboards. The accounting effect is identical (orphan_bytes is
// decremented by CompleteCleanupItem).
func (w *CleanupWorker) completeCleanupAlreadyAbsent(ctx context.Context, item *core.CleanupItem) {
	if err := w.store.CompleteCleanupItem(ctx, item.ID); err != nil {
		w.log.ErrorContext(ctx, logMsgCompleteCleanupFailed, slog.Int64("cleanup_id", item.ID), "error", err)
	}
	telemetry.CleanupQueueProcessedTotal.WithLabelValues("success_absent").Inc()
	w.log.InfoContext(ctx, "cleanup target already absent on backend",
		slog.String("backend", item.BackendName),
		slog.String("key", item.ObjectKey),
		slog.String("reason", item.Reason),
	)
	audit.Log(ctx, "cleanup_queue.already_absent",
		slog.String("key", item.ObjectKey),
		slog.String("backend", item.BackendName),
		slog.String("reason", item.Reason),
		slog.Int("attempt", int(item.Attempts+1)),
	)
}

// completeUnknownBackendItem retires a cleanup row whose backend is no
// longer registered. Treated as success because the configured fleet
// cannot have an orphan on a backend it does not know about.
func (w *CleanupWorker) completeUnknownBackendItem(ctx context.Context, item *core.CleanupItem) {
	w.log.WarnContext(ctx, "backend not found, removing item",
		"backend", item.BackendName, "key", item.ObjectKey)
	if err := w.store.CompleteCleanupItem(ctx, item.ID); err != nil {
		w.log.ErrorContext(ctx, logMsgCompleteCleanupFailed, slog.Int64("cleanup_id", item.ID), "error", err)
	}
	telemetry.CleanupQueueProcessedTotal.WithLabelValues("success").Inc()
}

// completeCleanupSuccess records a successful backend delete: complete
// the row (which atomically decrements orphan_bytes for the backing
// backend in a single CTE), audit, and bump the success counter.
func (w *CleanupWorker) completeCleanupSuccess(ctx context.Context, item *core.CleanupItem) {
	if err := w.store.CompleteCleanupItem(ctx, item.ID); err != nil {
		w.log.ErrorContext(ctx, logMsgCompleteCleanupFailed, slog.Int64("cleanup_id", item.ID), "error", err)
	}
	telemetry.CleanupQueueProcessedTotal.WithLabelValues("success").Inc()
	audit.Log(ctx, "cleanup_queue.processed",
		slog.String("key", item.ObjectKey),
		slog.String("backend", item.BackendName),
		slog.String("reason", item.Reason),
		slog.Int("attempt", int(item.Attempts+1)),
	)
}

// exhaustCleanupToDLQ moves a cleanup item that has exhausted its retries
// into the dead-letter queue. Emits an audit entry and a CleanupExhausted
// event so operators can investigate stuck rows.
func (w *CleanupWorker) exhaustCleanupToDLQ(
	ctx context.Context,
	item *core.CleanupItem,
	newAttempts int32,
	delErr error,
) {
	w.log.ErrorContext(ctx, "max attempts reached, moving to DLQ",
		slog.String("key", item.ObjectKey),
		slog.String("backend", item.BackendName),
		slog.Int("attempts", int(newAttempts)),
		slog.Int64("size_bytes", item.SizeBytes),
		"error", delErr)
	moved, mvErr := w.store.MoveCleanupToDLQ(ctx, item.ID, delErr.Error())
	if mvErr != nil {
		w.log.ErrorContext(ctx, "failed to move cleanup item to DLQ",
			slog.Int64("cleanup_id", item.ID), "error", mvErr)
		telemetry.CleanupQueueProcessedTotal.WithLabelValues("exhausted").Inc()
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
}

// scheduleCleanupRetry stamps the next retry deadline on a still-eligible
// cleanup row using exponential backoff.
func (w *CleanupWorker) scheduleCleanupRetry(ctx context.Context, item *core.CleanupItem, delErr error) {
	telemetry.CleanupQueueProcessedTotal.WithLabelValues("retry").Inc()
	backoff := CleanupBackoff(item.Attempts)
	if err := w.store.RetryCleanupItem(ctx, item.ID, backoff, delErr.Error()); err != nil {
		w.log.ErrorContext(ctx, "failed to update cleanup retry", "id", item.ID, "error", err)
	}
}

// recordCleanupDepths refreshes the cleanup-queue and DLQ depth gauges
// at the end of a tick. Errors are tolerated because depth reads are
// purely informational.
func (w *CleanupWorker) recordCleanupDepths(ctx context.Context) {
	if depth, err := w.store.CleanupQueueDepth(ctx); err == nil {
		telemetry.CleanupQueueDepth.Set(float64(depth))
	}
	if dlqDepth, err := w.store.CleanupDLQDepth(ctx); err == nil {
		telemetry.CleanupDLQDepth.Set(float64(dlqDepth))
	}
}
