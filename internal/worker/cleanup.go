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
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
	"github.com/afreidah/s3-orchestrator/internal/util/workerpool"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// CleanupWorker processes the retry queue for failed object deletions.
type CleanupWorker struct {
	log              *slog.Logger
	deps             Ops
	store            core.CleanupStore
	concurrency      int
	instanceID       string
	claimGracePeriod time.Duration
	gauges           GaugePublisher
}

// CleanupWorkerDeps groups the cleanup worker's constructor parameters.
// InstanceID is stamped into cleanup_queue.claimed_by for observability;
// ClaimGracePeriod is the threshold past which an outstanding claim becomes
// reclaimable by another worker tick (typically 5m).
type CleanupWorkerDeps struct {
	Ops              Ops
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

// SetGaugePublisher shares the queue depths with every instance. Called once
// at wiring, before the first tick; without it the gauges are set locally.
func (w *CleanupWorker) SetGaugePublisher(p GaugePublisher) {
	w.gauges = p
}

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// maxCleanupAttempts is the retry ceiling. The 1-minute starting
// backoff doubled 10 times yields ~17 hours of total retry runway,
// which is enough to bridge the longest realistic backend outages.
// Beyond that the row graduates to cleanup_dlq for operator action.
const maxCleanupAttempts = 10

// cleanupClaimBatch is how many rows one claim reserves.
const cleanupClaimBatch = 50

// cleanupMaxBatchesPerTick bounds one tick, so it cannot hold the cleanup
// queue lock indefinitely while a backlog drains.
const cleanupMaxBatchesPerTick = 20

// logMsgCompleteCleanupFailed is the shared error log message emitted
// when CompleteCleanupItem fails. Hoisted to a constant so the three
// completion paths (success, success_absent, unknown_backend) stay in
// lockstep and the SonarQube duplicate-literal rule (S1192) stays
// satisfied.
const logMsgCompleteCleanupFailed = "failed to complete cleanup item"

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

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
func (w *CleanupWorker) ProcessCleanupQueue(ctx context.Context) batch.Summary {
	return runTickCycle(ctx, "ProcessCleanupQueue", "cleanup_queue", w.processCleanupQueue)
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// processCleanupQueue is the body of ProcessCleanupQueue after the span is open.
// It keeps claiming while batches come back full, up to cleanupMaxBatchesPerTick,
// so a backlog drains faster than one batch a tick. Claimed rows leave the
// claimable set, and a retried row's next_retry moves past now, so each claim
// takes new rows; a batch that settles nothing ends the tick, so a down backend
// cannot churn through the whole queue.
func (w *CleanupWorker) processCleanupQueue(ctx context.Context) batch.Summary {
	pager := batch.Pager[core.CleanupItem, struct{}]{
		PageSize: batch.FixedPage(cleanupClaimBatch),
		List: func(ctx context.Context, limit int, _ struct{}) ([]core.CleanupItem, error) {
			graceCutoff := time.Now().Add(-w.claimGracePeriod)
			return w.store.ClaimPendingCleanups(ctx, limit, w.instanceID, graceCutoff)
		},
	}
	var total batch.Summary
	batches := 0
	stop, err := pager.Walk(ctx, func(ctx context.Context, items []core.CleanupItem) (batch.Step, error) {
		sum := w.settleBatch(ctx, items)
		total = total.Plus(sum)
		batches++
		return batch.Step{Progress: sum.Succeeded, Stop: batches == cleanupMaxBatchesPerTick}, nil
	})
	if stop == batch.Errored {
		w.log.ErrorContext(ctx, "failed to claim pending cleanups", "error", err)
	}
	w.recordCleanupDepths(ctx)
	return total
}

// settleBatch deletes one claimed batch's bytes and settles every row.
func (w *CleanupWorker) settleBatch(ctx context.Context, items []core.CleanupItem) batch.Summary {
	w.reportReclaimed(ctx, items)
	deleted := w.deleteClaimed(ctx, items)

	runner := batch.Runner[core.CleanupItem]{Name: "cleanup", Log: w.log, Concurrency: w.concurrency}
	return runner.Run(ctx, items, func(ctx context.Context, item core.CleanupItem) batch.ItemResult {
		d, attempted := deleted[item.ID]
		if !attempted {
			return batch.ItemResult{} // admission declined its backend; the row waits for a later cycle
		}
		return w.settleCleanupItem(ctx, &item, d)
	})
}

// reportReclaimed records each row this claim took over from a claim that
// outlived the grace period, which is a sign an instance died mid-batch.
func (w *CleanupWorker) reportReclaimed(ctx context.Context, items []core.CleanupItem) {
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
}

// cleanupDelete is what deleting one claimed row's bytes came to.
// unknownBackend means the row names a backend that is no longer registered,
// so nothing was attempted.
type cleanupDelete struct {
	err            error
	unknownBackend bool
}

// deleteClaimed deletes every claimed row's bytes, one batched delete per
// backend, each under one admission slot. It deletes each row's queued path,
// not the object's key: the row names the bytes one write put on the backend,
// and a later write of the same object has its own bytes this must not reach.
func (w *CleanupWorker) deleteClaimed(ctx context.Context, items []core.CleanupItem) map[int64]cleanupDelete {
	byBackend := make(map[string][]core.CleanupItem)
	for i := range items {
		byBackend[items[i].BackendName] = append(byBackend[items[i].BackendName], items[i])
	}

	var mu sync.Mutex
	deleted := make(map[int64]cleanupDelete, len(items))
	record := func(id int64, d cleanupDelete) {
		mu.Lock()
		deleted[id] = d
		mu.Unlock()
	}
	groups := slices.Collect(maps.Values(byBackend))
	workerpool.Run(ctx, w.concurrency, groups, func(ctx context.Context, group []core.CleanupItem) {
		name := group[0].BackendName
		be, err := w.deps.GetBackend(name)
		if err != nil {
			for i := range group {
				record(group[i].ID, cleanupDelete{unknownBackend: true})
			}
			return
		}
		paths := make([]string, len(group))
		for i := range group {
			paths[i] = core.StoragePath(group[i].ObjectKey, group[i].StorageKey)
		}
		WithAdmission(ctx, w.deps, WorkerNameCleanup, func() {
			failed := w.deps.DeleteMany(ctx, name, be, paths)
			for i := range group {
				record(group[i].ID, cleanupDelete{err: failed[paths[i]]})
			}
		})
	})
	return deleted
}

// settleCleanupItem gives one cleanup queue row its outcome from deleting its
// bytes: complete, retry, or graduate to the DLQ.
func (w *CleanupWorker) settleCleanupItem(ctx context.Context, item *core.CleanupItem, d cleanupDelete) batch.ItemResult {
	if d.unknownBackend {
		w.completeUnknownBackendItem(ctx, item)
		return batch.ItemResult{Outcome: batch.ItemSucceeded, Status: "success"}
	}

	delErr := d.err
	if delErr == nil {
		w.completeCleanupSuccess(ctx, item)
		return batch.ItemResult{Outcome: batch.ItemSucceeded, Status: "success"}
	}

	// 404 means the backend already agrees the object is gone, which is
	// the desired end state. Drop the row so we don't burn 9 retries +
	// a DLQ slot on a non-event.
	if backend.IsNotFound(delErr) {
		w.completeCleanupAlreadyAbsent(ctx, item)
		return batch.ItemResult{Outcome: batch.ItemSucceeded, Status: "success_absent"}
	}

	newAttempts := item.Attempts + 1
	if newAttempts >= maxCleanupAttempts {
		w.exhaustCleanupToDLQ(ctx, item, newAttempts, delErr)
		return batch.ItemResult{Outcome: batch.ItemFailed, Status: "exhausted"}
	}
	w.scheduleCleanupRetry(ctx, item, delErr)
	return batch.ItemResult{Outcome: batch.ItemFailed, Status: "retry"}
}

// completeCleanupAlreadyAbsent retires a cleanup row whose backend DELETE
// returned 404. It matches completeCleanupSuccess but reports
// status="success_absent" in its metric and audit event.
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
		event.Publish(event.CleanupExhausted, item.BackendName, map[string]any{
			"backend":    item.BackendName,
			"object_key": item.ObjectKey,
			"reason":     item.Reason,
			"attempts":   int(newAttempts),
			"size_bytes": item.SizeBytes,
			"last_error": delErr.Error(),
		})
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

// recordCleanupDepths publishes the cleanup-queue and DLQ depths at the end
// of a tick. A failed read leaves that gauge as it was, because depth reads
// are purely informational.
func (w *CleanupWorker) recordCleanupDepths(ctx context.Context) {
	var g telemetry.WorkerGauges
	if depth, err := w.store.CleanupQueueDepth(ctx); err == nil {
		g.CleanupQueueDepth = &depth
	}
	if dlqDepth, err := w.store.CleanupDLQDepth(ctx); err == nil {
		g.CleanupDLQDepth = &dlqDepth
	}
	publishGauges(ctx, w.gauges, telemetry.GaugeSourceCleanup, g)
}
