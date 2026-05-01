// -------------------------------------------------------------------------------
// Pending Reaper - Background Resolver for In-Flight PUT Intents
//
// Author: Alex Freidah
//
// Resolves pending_objects rows left behind by failed write-path commits.
// Each tick fetches intents older than the configured min-age, HEADs the
// destination backend, and either promotes the intent into object_locations
// (bytes present) or drops it (bytes absent). Min-age guards against racing
// in-flight PUTs whose synchronous commit has not yet run.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/util/workerpool"
)

// PendingReaperStore defines the store operations the reaper needs.
type PendingReaperStore interface {
	store.PendingStore
}

// PendingReaper resolves abandoned PUT intents by inspecting the destination
// backend. The min-age window protects in-flight PUTs whose commit has not
// yet had a chance to clear the intent on the synchronous path.
type PendingReaper struct {
	deps        CleanupOps
	store       PendingReaperStore
	concurrency int
	minAge      time.Duration
	batchSize   int
}

// NewPendingReaper creates a PendingReaper with explicit dependencies.
// concurrency, minAge, and batchSize fall back to safe defaults when zero
// or negative.
func NewPendingReaper(deps CleanupOps, store PendingReaperStore, concurrency int, minAge time.Duration, batchSize int) *PendingReaper {
	if concurrency <= 0 {
		concurrency = 4
	}
	if minAge <= 0 {
		minAge = 5 * time.Minute
	}
	if batchSize <= 0 {
		batchSize = 50
	}
	return &PendingReaper{
		deps:        deps,
		store:       store,
		concurrency: concurrency,
		minAge:      minAge,
		batchSize:   batchSize,
	}
}

// ProcessPendingQueue runs one reaper tick: fetch stale intents, HEAD their
// destinations, and promote or drop based on what the backend reports.
// Returns the number of intents that completed (committed or dropped) and
// the number that failed (left for the next tick).
func (r *PendingReaper) ProcessPendingQueue(ctx context.Context) (resolved, failed int) {
	ctx, span := telemetry.StartSpan(ctx, "ProcessPendingQueue",
		telemetry.AttrOperation.String("pending_queue"),
	)
	defer span.End()

	cutoff := time.Now().Add(-r.minAge)
	intents, err := r.store.GetStalePending(ctx, cutoff, r.batchSize)
	if err != nil {
		slog.ErrorContext(ctx, "failed to fetch stale pending intents", "error", err)
		return 0, 0
	}

	var resolvedCount, failedCount atomic.Int32

	workerpool.Run(ctx, r.concurrency, intents, func(ctx context.Context, p store.PendingObject) {
		if !r.deps.AcquireAdmission(ctx) {
			telemetry.WorkerAdmissionRejectionsTotal.WithLabelValues("pending_reaper").Inc()
			return
		}
		defer r.deps.ReleaseAdmission()

		be, err := r.deps.GetBackend(p.BackendName)
		if err != nil {
			// Backend has been removed but the intent outlived it. Drop the
			// intent so it does not block FK-cascade deletion of the backend.
			slog.WarnContext(ctx, "Pending reaper: backend not registered, dropping intent",
				"backend", p.BackendName, "key", p.ObjectKey, "intent_id", p.IntentID)
			if err := r.store.DeletePending(ctx, p.IntentID); err != nil {
				slog.ErrorContext(ctx, "failed to delete pending intent", "intent_id", p.IntentID, "error", err)
				failedCount.Add(1)
				return
			}
			telemetry.PendingIntentsResolvedTotal.WithLabelValues("dropped").Inc()
			resolvedCount.Add(1)
			return
		}

		hctx, hcancel := r.deps.WithTimeout(ctx)
		_, headErr := be.HeadObject(hctx, p.ObjectKey)
		hcancel()
		r.deps.Usage().Record(p.BackendName, 1, 0, 0)

		if headErr != nil && isNotFound(headErr) {
			// Bytes never landed. Drop the intent; nothing to recover.
			if err := r.store.DeletePending(ctx, p.IntentID); err != nil {
				slog.ErrorContext(ctx, "failed to delete pending intent", "intent_id", p.IntentID, "error", err)
				failedCount.Add(1)
				return
			}
			telemetry.PendingIntentsResolvedTotal.WithLabelValues("dropped").Inc()
			resolvedCount.Add(1)
			audit.Log(ctx, "pending_reaper.dropped",
				slog.String("key", p.ObjectKey),
				slog.String("backend", p.BackendName),
				slog.String("intent_id", p.IntentID),
			)
			return
		}
		if headErr != nil {
			// Transient backend or network error; leave for the next tick.
			slog.WarnContext(ctx, "Pending reaper: HEAD failed, leaving intent",
				"backend", p.BackendName, "key", p.ObjectKey, "error", headErr)
			failedCount.Add(1)
			return
		}

		// Bytes present. Promote the intent into object_locations.
		result, displaced, err := r.store.PromotePending(ctx, &p)
		if err != nil {
			slog.ErrorContext(ctx, "failed to promote pending intent", "intent_id", p.IntentID, "error", err)
			failedCount.Add(1)
			return
		}
		switch result {
		case store.PendingPromoteCommitted:
			telemetry.PendingIntentsResolvedTotal.WithLabelValues("promoted").Inc()
			resolvedCount.Add(1)
			audit.Log(ctx, "pending_reaper.promoted",
				slog.String("key", p.ObjectKey),
				slog.String("backend", p.BackendName),
				slog.String("intent_id", p.IntentID),
				slog.Int("displaced_copies", len(displaced)),
			)
			// Displaced copies on other backends become orphans: enqueue
			// each for cleanup so the bytes are not silently abandoned.
			for _, dc := range displaced {
				dcBackend, err := r.deps.GetBackend(dc.BackendName)
				if err != nil {
					slog.WarnContext(ctx, "displaced copy backend not registered",
						"backend", dc.BackendName, "key", p.ObjectKey)
					continue
				}
				r.deps.DeleteOrEnqueue(ctx, dcBackend, dc.BackendName, p.ObjectKey, "overwrite_displaced", dc.SizeBytes)
			}
		case store.PendingPromoteSuperseded:
			// A successful write committed for the same key after this
			// intent was inserted. The store deleted the pending row in
			// the same transaction as the timestamp check, so nothing to
			// do here beyond accounting and an audit trail.
			telemetry.PendingIntentsResolvedTotal.WithLabelValues("superseded").Inc()
			resolvedCount.Add(1)
			audit.Log(ctx, "pending_reaper.superseded",
				slog.String("key", p.ObjectKey),
				slog.String("backend", p.BackendName),
				slog.String("intent_id", p.IntentID),
			)
		case store.PendingPromoteAmbiguous:
			// Reserved for pathological cases the timestamp comparison
			// does not cover. The current resolver does not produce this
			// result; if it ever appears it's a bug worth surfacing
			// loudly rather than silently leaving the row pinned.
			slog.WarnContext(ctx, "Pending reaper: promotion ambiguous, leaving for operator",
				"intent_id", p.IntentID, "key", p.ObjectKey, "backend", p.BackendName)
			telemetry.PendingIntentsResolvedTotal.WithLabelValues("ambiguous").Inc()
			failedCount.Add(1)
		case store.PendingPromoteAlreadyResolved:
			telemetry.PendingIntentsResolvedTotal.WithLabelValues("already_resolved").Inc()
			resolvedCount.Add(1)
		}
	})

	if depth, err := r.store.PendingDepth(ctx); err == nil {
		telemetry.PendingIntentsDepth.Set(float64(depth))
	}

	return int(resolvedCount.Load()), int(failedCount.Load())
}

