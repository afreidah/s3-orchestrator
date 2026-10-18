// -------------------------------------------------------------------------------
// Cleanup Worker - Background Retry Worker
//
// Author: Alex Freidah
//
// Claims cleanup_queue rows no instance is settling - a delete that failed and
// is due for retry, or a row whose claim outlived an instance that died before
// settling it - and settles them through the shared cleanup settler. Retries
// back off exponentially from 1 minute to 24 hours, with a maximum of 10
// attempts before a row graduates to the dead-letter queue.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/cleanup"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// CleanupWorker processes the cleanup queue rows no request is settling.
type CleanupWorker struct {
	log              *slog.Logger
	store            core.CleanupStore
	settler          *cleanup.Settler
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

// NewCleanupWorker creates a CleanupWorker with the given dependencies. Each
// backend's delete runs under an admission slot, so a tick competes for
// backend capacity like every other worker.
func NewCleanupWorker(deps CleanupWorkerDeps) *CleanupWorker {
	must.NotNil("Ops", deps.Ops)
	must.NotNil("Store", deps.Store)
	return &CleanupWorker{
		store: deps.Store,
		settler: cleanup.New(cleanup.Deps{
			Deleter:     deps.Ops,
			Store:       deps.Store,
			Concurrency: deps.Concurrency,
			Name:        "cleanup",
			Admit: func(ctx context.Context, fn func()) {
				WithAdmission(ctx, deps.Ops, WorkerNameCleanup, fn)
			},
		}),
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

// cleanupClaimBatch is how many rows one claim reserves.
const cleanupClaimBatch = 50

// cleanupMaxBatchesPerTick bounds one tick, so it cannot hold the cleanup
// queue lock indefinitely while a backlog drains.
const cleanupMaxBatchesPerTick = 20

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// ProcessCleanupQueue claims pending cleanup rows and settles them.
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
		w.reportReclaimed(ctx, items)
		sum := w.settler.Settle(ctx, items)
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

// reportReclaimed records each row this claim took over from a claim that
// outlived the grace period, which is a sign an instance died before settling
// rows it queued or claimed.
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
