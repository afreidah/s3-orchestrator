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

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/workerpool"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// PendingReaperStore defines the store operations the reaper needs.
type PendingReaperStore interface {
	core.PendingStore
}

// PendingReaper resolves abandoned PUT intents by inspecting the destination
// backend. The min-age window protects in-flight PUTs whose commit has not
// yet had a chance to clear the intent on the synchronous path.
type PendingReaper struct {
	deps        CleanupOps
	store       PendingReaperStore
	log         *slog.Logger
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
		log:         slog.Default().With(logfmt.Component("pending_reaper")),
		concurrency: concurrency,
		minAge:      minAge,
		batchSize:   batchSize,
	}
}

// -------------------------------------------------------------------------
// REAPER TICK
// -------------------------------------------------------------------------

// ProcessPendingQueue runs one reaper tick: fetch stale intents, HEAD
// their destinations, and promote or drop based on what the backend
// reports. Returns the number of intents that completed (committed or
// dropped) and the number that failed (left for the next tick).
func (r *PendingReaper) ProcessPendingQueue(ctx context.Context) (resolved, failed int) {
	ctx, span := telemetry.StartSpan(ctx, "ProcessPendingQueue",
		telemetry.AttrOperation.String("pending_queue"),
	)
	defer span.End()

	cutoff := time.Now().Add(-r.minAge)
	intents, err := r.store.GetStalePending(ctx, cutoff, r.batchSize)
	if err != nil {
		r.log.ErrorContext(ctx, "fetch stale pending intents", logfmt.Err(err), logfmt.Outcome(logfmt.OutcomeError))
		return 0, 0
	}

	var resolvedCount, failedCount atomic.Int32
	workerpool.Run(ctx, r.concurrency, intents, func(ctx context.Context, p core.PendingObject) {
		r.resolveOneIntent(ctx, &p, &resolvedCount, &failedCount)
	})

	if depth, err := r.store.PendingDepth(ctx); err == nil {
		telemetry.PendingIntentsDepth.Set(float64(depth))
	}
	return int(resolvedCount.Load()), int(failedCount.Load())
}

// resolveOneIntent applies the reaper's resolution decision to a single
// pending row: admission gating, backend lookup, HEAD probe, and
// promotion or drop depending on what the backend and store report.
func (r *PendingReaper) resolveOneIntent(ctx context.Context, p *core.PendingObject, resolvedCount, failedCount *atomic.Int32) {
	if !r.deps.AcquireAdmission(ctx) {
		telemetry.WorkerAdmissionRejectionsTotal.WithLabelValues("pending_reaper").Inc()
		return
	}
	defer r.deps.ReleaseAdmission()

	be, err := r.deps.GetBackend(p.BackendName)
	if err != nil {
		r.dropIntent(ctx, p, "backend_removed", resolvedCount, failedCount)
		return
	}

	switch r.probeBackend(ctx, be, p) {
	case probeFound:
		r.handlePromotion(ctx, p, resolvedCount, failedCount)
	case probeNotFound:
		r.dropIntent(ctx, p, "head_404", resolvedCount, failedCount)
	case probeError:
		// Transient backend or network error; leave for the next tick.
		failedCount.Add(1)
	}
}

// -------------------------------------------------------------------------
// BACKEND PROBE AND DROP / PROMOTE PATHS
// -------------------------------------------------------------------------

// probeOutcome enumerates the three states a backend HEAD can resolve to
// from the reaper's point of view.
type probeOutcome int

// probeFound and related constants used by this package.
const (
	// probeFound means HEAD returned 200; bytes are present on the backend.
	probeFound probeOutcome = iota
	// probeNotFound means HEAD returned 404; bytes were never written.
	probeNotFound
	// probeError means HEAD returned a non-404 error; the result is
	// inconclusive and the intent must be left for a later tick.
	probeError
)

// probeBackend HEADs the destination backend and classifies the result.
// Records one API call against the backend's usage tracker regardless of
// outcome so usage accounting remains accurate during reaper sweeps.
func (r *PendingReaper) probeBackend(ctx context.Context, be backend.ObjectBackend, p *core.PendingObject) probeOutcome {
	hctx, hcancel := r.deps.WithTimeout(ctx)
	_, err := be.HeadObject(hctx, p.ObjectKey)
	hcancel()
	r.deps.Usage().Record(p.BackendName, 1, 0, 0)

	switch {
	case err == nil:
		return probeFound
	case isNotFound(err):
		return probeNotFound
	default:
		r.log.WarnContext(ctx, "HEAD probe failed, leaving intent for next tick",
			"backend", p.BackendName, "key", p.ObjectKey, "intent_id", p.IntentID, logfmt.Err(err))
		return probeError
	}
}

// dropIntent deletes a pending row that has no recoverable bytes (either
// the backend is gone or HEAD returned 404). reason is recorded as a slog
// attribute so operators can distinguish the two paths in audit logs.
func (r *PendingReaper) dropIntent(ctx context.Context, p *core.PendingObject, reason string, resolvedCount, failedCount *atomic.Int32) {
	if reason == "backend_removed" {
		r.log.WarnContext(ctx, "backend not registered, dropping intent",
			"backend", p.BackendName, "key", p.ObjectKey, "intent_id", p.IntentID)
	}
	if err := r.store.DeletePending(ctx, p.IntentID); err != nil {
		r.log.ErrorContext(ctx, "delete pending intent",
			"intent_id", p.IntentID, logfmt.Err(err), logfmt.Outcome(logfmt.OutcomeError))
		failedCount.Add(1)
		return
	}
	telemetry.PendingIntentsResolvedTotal.WithLabelValues("dropped").Inc()
	resolvedCount.Add(1)
	if reason == "head_404" {
		audit.Log(ctx, "pending_reaper.dropped",
			slog.String("key", p.ObjectKey),
			slog.String("backend", p.BackendName),
			slog.String("intent_id", p.IntentID),
		)
	}
}

// handlePromotion resolves an intent whose backend HEAD returned 200 by
// calling PromotePending and dispatching to one of the four result-code
// handlers. Each handler updates metrics, audit logs, and the resolved/
// failed counters as appropriate.
func (r *PendingReaper) handlePromotion(ctx context.Context, p *core.PendingObject, resolvedCount, failedCount *atomic.Int32) {
	result, displaced, err := r.store.PromotePending(ctx, p)
	if err != nil {
		r.log.ErrorContext(ctx, "promote pending intent",
			"intent_id", p.IntentID, logfmt.Err(err), logfmt.Outcome(logfmt.OutcomeError))
		failedCount.Add(1)
		return
	}
	switch result {
	case core.PendingPromoteCommitted:
		r.onPromoteCommitted(ctx, p, displaced, resolvedCount)
	case core.PendingPromoteSuperseded:
		r.onPromoteSuperseded(ctx, p, resolvedCount)
	case core.PendingPromoteAmbiguous:
		r.onPromoteAmbiguous(ctx, p, failedCount)
	case core.PendingPromoteAlreadyResolved:
		telemetry.PendingIntentsResolvedTotal.WithLabelValues("already_resolved").Inc()
		resolvedCount.Add(1)
	}
}

// onPromoteCommitted records the success metric, emits an audit log, and
// fans out cleanup deletes for any displaced copies on other backends so
// orphan bytes do not accumulate after an overwrite-style promotion.
func (r *PendingReaper) onPromoteCommitted(ctx context.Context, p *core.PendingObject, displaced []core.DeletedCopy, resolvedCount *atomic.Int32) {
	telemetry.PendingIntentsResolvedTotal.WithLabelValues("promoted").Inc()
	resolvedCount.Add(1)
	audit.Log(ctx, "pending_reaper.promoted",
		slog.String("key", p.ObjectKey),
		slog.String("backend", p.BackendName),
		slog.String("intent_id", p.IntentID),
		slog.Int("displaced_copies", len(displaced)),
	)
	for _, dc := range displaced {
		dcBackend, err := r.deps.GetBackend(dc.BackendName)
		if err != nil {
			r.log.WarnContext(ctx, "displaced copy backend not registered",
				"backend", dc.BackendName, "key", p.ObjectKey)
			continue
		}
		r.deps.DeleteOrEnqueue(ctx, dcBackend, dc.BackendName, p.ObjectKey, "overwrite_displaced", dc.SizeBytes)
	}
}

// onPromoteSuperseded handles the timestamp-aware drop path: the store
// already deleted the pending row in-txn, so the reaper just records the
// outcome and emits an audit trail.
func (r *PendingReaper) onPromoteSuperseded(ctx context.Context, p *core.PendingObject, resolvedCount *atomic.Int32) {
	telemetry.PendingIntentsResolvedTotal.WithLabelValues("superseded").Inc()
	resolvedCount.Add(1)
	audit.Log(ctx, "pending_reaper.superseded",
		slog.String("key", p.ObjectKey),
		slog.String("backend", p.BackendName),
		slog.String("intent_id", p.IntentID),
	)
}

// onPromoteAmbiguous logs the unexpected branch loudly. The current
// resolver does not produce this result; if it ever fires it's a bug
// worth surfacing rather than silently leaving the row pinned.
func (r *PendingReaper) onPromoteAmbiguous(ctx context.Context, p *core.PendingObject, failedCount *atomic.Int32) {
	r.log.WarnContext(ctx, "promotion ambiguous, leaving for operator",
		"intent_id", p.IntentID, "key", p.ObjectKey, "backend", p.BackendName)
	telemetry.PendingIntentsResolvedTotal.WithLabelValues("ambiguous").Inc()
	failedCount.Add(1)
}
