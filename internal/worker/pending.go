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
	"sync"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// PendingReaperStore is the narrow persistence surface the pending reaper
// needs. Declared locally so the worker does not pull in the full
// MetadataStore.
type PendingReaperStore interface {
	core.PendingStore
}

// PendingReaper resolves abandoned PUT intents by inspecting the destination
// backend. The min-age window protects in-flight PUTs whose commit has not
// yet had a chance to clear the intent on the synchronous path.
type PendingReaper struct {
	deps        CleanupOps
	placement   Placement
	store       PendingReaperStore
	log         *slog.Logger
	concurrency int
	minAge      time.Duration
	batchSize   int
}

// PendingReaperDeps groups the pending reaper's constructor parameters.
// Concurrency, MinAge, and BatchSize fall back to safe defaults when zero
// or negative.
type PendingReaperDeps struct {
	Ops         CleanupOps
	Placement   Placement
	Store       PendingReaperStore
	Concurrency int
	MinAge      time.Duration
	BatchSize   int
}

// NewPendingReaper creates a PendingReaper with the given dependencies.
func NewPendingReaper(deps PendingReaperDeps) *PendingReaper {
	must.NotNil("Ops", deps.Ops)
	must.NotNil("Placement", deps.Placement)
	must.NotNil("Store", deps.Store)
	concurrency, minAge, batchSize := deps.Concurrency, deps.MinAge, deps.BatchSize
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
		deps:        deps.Ops,
		placement:   deps.Placement,
		store:       deps.Store,
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
func (r *PendingReaper) ProcessPendingQueue(ctx context.Context) WorkSummary {
	return runTickCycle(ctx, "ProcessPendingQueue", "pending_queue", r.processPendingQueue)
}

// processPendingQueue is the body of ProcessPendingQueue after the span is open.
func (r *PendingReaper) processPendingQueue(ctx context.Context) WorkSummary {
	cutoff := time.Now().Add(-r.minAge)
	intents, err := r.store.GetStalePending(ctx, cutoff, r.batchSize)
	if err != nil {
		r.log.ErrorContext(ctx, "fetch stale pending intents", "error", err, logfmt.Outcome(logfmt.OutcomeError))
		return WorkSummary{}
	}

	// skipped accumulates the per-backend count of intents short-circuited
	// because their destination backend's circuit breaker is currently open.
	// One INFO log line is emitted per skipped backend at the end of the
	// tick instead of per-intent WARN spam from the probe path.
	var skipped sync.Map
	runner := BatchRunner[core.PendingObject]{Name: "pending-reaper", Log: r.log, Concurrency: r.concurrency}
	sum := runner.Run(ctx, intents, func(ctx context.Context, p core.PendingObject) ItemResult {
		return r.resolveOneIntent(ctx, &p, &skipped)
	})
	skipped.Range(func(k, v any) bool {
		r.log.InfoContext(ctx, "backend circuit open, skipping pending intents",
			"backend", k.(string), "intents_skipped", v.(*atomic.Int32).Load())
		return true
	})

	if depth, err := r.store.PendingDepth(ctx); err == nil {
		telemetry.PendingIntentsDepth.Set(float64(depth))
	}
	return sum
}

// resolveOneIntent applies the reaper's resolution decision to a single
// pending row: admission gating, backend lookup, HEAD probe, and
// promotion or drop depending on what the backend and store report.
//
// When the destination backend's circuit breaker is open and not yet
// probe-eligible the intent is short-circuited: it is counted as failed
// (so it stays queued for the next tick) and tallied in skipped so the
// caller can emit one INFO log per backend instead of a probe-failed WARN
// per intent.
func (r *PendingReaper) resolveOneIntent(ctx context.Context, p *core.PendingObject, skipped *sync.Map) ItemResult {
	var res ItemResult // zero value (ItemSkipped) when admission blocks the work
	WithAdmission(ctx, r.deps, WorkerNamePendingReaper, func() {
		be, err := r.deps.GetBackend(p.BackendName)
		if err != nil {
			res = ItemResult{Outcome: r.dropIntent(ctx, p, "backend_removed")}
			return
		}

		if cb, ok := be.(*backend.CircuitBreakerBackend); ok &&
			cb.State() == breaker.StateOpen && !cb.ProbeEligible() {
			cnt, _ := skipped.LoadOrStore(p.BackendName, &atomic.Int32{})
			cnt.(*atomic.Int32).Add(1)
			res = ItemResult{Outcome: ItemFailed}
			return
		}

		switch r.probeBackend(ctx, be, p) {
		case probeFound:
			res = ItemResult{Outcome: r.handlePromotion(ctx, p)}
		case probeNotFound:
			res = ItemResult{Outcome: r.dropIntent(ctx, p, "head_404")}
		case probeError:
			// Transient backend or network error; leave for the next tick.
			res = ItemResult{Outcome: ItemFailed}
		}
	})
	return res
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
	_, err := r.deps.HeadWithTimeout(ctx, be, p.ObjectKey)
	r.deps.Acct().APICall(p.BackendName)

	switch {
	case err == nil:
		return probeFound
	case backend.IsNotFound(err):
		return probeNotFound
	default:
		r.log.WarnContext(ctx, "HEAD probe failed, leaving intent for next tick",
			"backend", p.BackendName, "key", p.ObjectKey, "intent_id", p.IntentID, "error", err)
		return probeError
	}
}

// dropIntent deletes a pending row that has no recoverable bytes (either
// the backend is gone or HEAD returned 404). reason is recorded as a slog
// attribute so operators can distinguish the two paths in audit logs.
func (r *PendingReaper) dropIntent(ctx context.Context, p *core.PendingObject, reason string) ItemOutcome {
	if reason == "backend_removed" {
		r.log.WarnContext(ctx, "backend not registered, dropping intent",
			"backend", p.BackendName, "key", p.ObjectKey, "intent_id", p.IntentID)
	}
	if err := r.store.DeletePending(ctx, p.IntentID); err != nil {
		r.log.ErrorContext(ctx, "delete pending intent",
			"intent_id", p.IntentID, "error", err, logfmt.Outcome(logfmt.OutcomeError))
		return ItemFailed
	}
	telemetry.PendingIntentsResolvedTotal.WithLabelValues("dropped").Inc()
	if reason == "head_404" {
		audit.Log(ctx, "pending_reaper.dropped",
			slog.String("key", p.ObjectKey),
			slog.String("backend", p.BackendName),
			slog.String("intent_id", p.IntentID),
		)
	}
	return ItemSucceeded
}

// handlePromotion resolves an intent whose backend HEAD returned 200 by
// calling PromotePending and dispatching to one of the four result-code
// handlers. Each handler updates metrics, audit logs, and the resolved/
// failed counters as appropriate.
func (r *PendingReaper) handlePromotion(ctx context.Context, p *core.PendingObject) ItemOutcome {
	result, displaced, err := r.store.PromotePending(ctx, p)
	if err != nil {
		r.log.ErrorContext(ctx, "promote pending intent",
			"intent_id", p.IntentID, "error", err, logfmt.Outcome(logfmt.OutcomeError))
		return ItemFailed
	}
	switch result {
	case core.PendingPromoteCommitted:
		r.onPromoteCommitted(ctx, p, displaced)
		return ItemSucceeded
	case core.PendingPromoteSuperseded:
		r.onPromoteSuperseded(ctx, p)
		return ItemSucceeded
	case core.PendingPromoteAmbiguous:
		r.onPromoteAmbiguous(ctx, p)
		return ItemFailed
	case core.PendingPromoteAlreadyResolved:
		telemetry.PendingIntentsResolvedTotal.WithLabelValues("already_resolved").Inc()
		return ItemSucceeded
	}
	return ItemFailed
}

// onPromoteCommitted records the success metric, emits an audit log, and
// fans out cleanup deletes for any displaced copies on other backends so
// orphan bytes do not accumulate after an overwrite-style promotion.
func (r *PendingReaper) onPromoteCommitted(ctx context.Context, p *core.PendingObject, displaced []core.DeletedCopy) {
	telemetry.PendingIntentsResolvedTotal.WithLabelValues("promoted").Inc()
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
		r.placement.DeleteOrEnqueue(ctx, dcBackend, dc.BackendName, p.ObjectKey, "overwrite_displaced", dc.SizeBytes)
	}
}

// onPromoteSuperseded handles the timestamp-aware drop path: the store
// already deleted the pending row in-txn, so the reaper just records the
// outcome and emits an audit trail.
func (r *PendingReaper) onPromoteSuperseded(ctx context.Context, p *core.PendingObject) {
	telemetry.PendingIntentsResolvedTotal.WithLabelValues("superseded").Inc()
	audit.Log(ctx, "pending_reaper.superseded",
		slog.String("key", p.ObjectKey),
		slog.String("backend", p.BackendName),
		slog.String("intent_id", p.IntentID),
	)
}

// onPromoteAmbiguous logs the unexpected branch loudly. The current
// resolver does not produce this result; if it ever fires it's a bug
// worth surfacing rather than silently leaving the row pinned.
func (r *PendingReaper) onPromoteAmbiguous(ctx context.Context, p *core.PendingObject) {
	r.log.WarnContext(ctx, "promotion ambiguous, leaving for operator",
		"intent_id", p.IntentID, "key", p.ObjectKey, "backend", p.BackendName)
	telemetry.PendingIntentsResolvedTotal.WithLabelValues("ambiguous").Inc()
}
