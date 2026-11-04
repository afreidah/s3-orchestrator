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
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/cycle"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// PendingReaper resolves abandoned PUT intents by inspecting the destination
// backend. The min-age window protects in-flight PUTs whose commit has not
// yet had a chance to clear the intent on the synchronous path.
type PendingReaper struct {
	d   PendingReaperDeps
	log *slog.Logger
}

// PendingReaperDeps groups the pending reaper's constructor parameters.
// MinAge and BatchSize come from config, which fills their defaults;
// Concurrency falls back to 4 when zero or negative.
type PendingReaperDeps struct {
	Fleet
	Store       core.PendingStore
	Concurrency int
	MinAge      time.Duration
	BatchSize   int
}

// NewPendingReaper creates a PendingReaper with the given dependencies.
//
//nolint:gocritic // hugeParam: the Deps are copied once, at construction.
func NewPendingReaper(d PendingReaperDeps) *PendingReaper {
	d.mustBeWired()
	must.NotNil("d.Store", d.Store)
	if d.Concurrency <= 0 {
		d.Concurrency = 4
	}
	return &PendingReaper{d: d, log: logfmt.Logger(string(audit.WorkloadPendingReaper))}
}

// -------------------------------------------------------------------------
// REAPER TICK
// -------------------------------------------------------------------------

// ProcessPendingQueue runs one reaper pass: fetch stale intents, HEAD their
// destinations, and promote or drop each on what the backend reports. An
// intent that fails is left for the next pass.
func (r *PendingReaper) ProcessPendingQueue(ctx context.Context) (batch.Summary, error) {
	return cycle.Run(ctx, cycle.Pass{Workload: audit.WorkloadPendingReaper}, r.processPendingQueue)
}

// processPendingQueue is the body of ProcessPendingQueue after the span is open.
// Intents whose backend is refusing HEAD are counted per backend and logged
// once each, rather than once per intent.
func (r *PendingReaper) processPendingQueue(ctx context.Context) (batch.Summary, error) {
	cutoff := time.Now().Add(-r.d.MinAge)
	intents, err := r.d.Store.GetStalePending(ctx, cutoff, r.d.BatchSize)
	if err != nil {
		return batch.Summary{}, fmt.Errorf("fetch stale pending intents: %w", err)
	}

	var refused headRefusals
	runner := batch.Runner[core.PendingObject]{Concurrency: r.d.Concurrency, Admission: r.d.Ops}
	sum := runner.Run(ctx, intents, func(ctx context.Context, p core.PendingObject) batch.ItemResult {
		return r.resolveOneIntent(ctx, &p, &refused)
	})
	for _, name := range slices.Sorted(maps.Keys(refused.n)) {
		r.log.InfoContext(ctx, "backend refusing HEAD, skipping pending intents",
			"backend", name, "intents_skipped", refused.n[name])
	}

	if depth, err := r.d.Store.PendingDepth(ctx); err == nil {
		r.d.Metrics.PublishWorkerGauges(ctx, telemetry.GaugeSourcePendingReaper,
			telemetry.WorkerGauges{PendingIntentsDepth: &depth})
	}
	return sum, nil
}

// headRefusals counts, per backend, the intents a pass left queued because
// the backend is refusing HEAD. Items run concurrently, so adding takes the
// lock; the pass reads the counts once every item is done.
type headRefusals struct {
	mu sync.Mutex
	n  map[string]int
}

// add counts one intent left queued on backend.
func (h *headRefusals) add(backend string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.n == nil {
		h.n = make(map[string]int)
	}
	h.n[backend]++
}

// resolveOneIntent applies the reaper's resolution decision to a single
// pending row: backend lookup, HEAD probe, and promotion or drop depending
// on what the backend and store report. An intent whose backend is refusing
// HEAD counts as failed, so it stays queued, and is added to refused.
func (r *PendingReaper) resolveOneIntent(ctx context.Context, p *core.PendingObject, refused *headRefusals) batch.ItemResult {
	be, err := r.d.Ops.GetBackend(p.BackendName)
	if err != nil {
		return batch.ItemResult{Outcome: r.dropIntent(ctx, p, "backend_removed")}
	}

	if !backend.ServedCalls(be).Has(core.RequiredForProbe) {
		refused.add(p.BackendName)
		return batch.Failed()
	}

	switch r.probeBackend(ctx, be, p) {
	case probeFound:
		return batch.ItemResult{Outcome: r.handlePromotion(ctx, p)}
	case probeNotFound:
		return batch.ItemResult{Outcome: r.dropIntent(ctx, p, "head_404")}
	default:
		// Transient backend or network error; leave for the next tick.
		return batch.Failed()
	}
}

// -------------------------------------------------------------------------
// BACKEND PROBE AND DROP / PROMOTE PATHS
// -------------------------------------------------------------------------

// probeOutcome enumerates the three states a backend HEAD can resolve to
// from the reaper's point of view.
type probeOutcome int

// The HEAD outcomes a probe can report. An error is inconclusive rather than
// negative: the bytes may well be there, so the intent is left for a later tick
// instead of being resolved on a guess.
const (
	probeFound    probeOutcome = iota // 200: the bytes are on the backend
	probeNotFound                     // 404: they were never written
	probeError                        // anything else: inconclusive
)

// probeBackend HEADs the intent's own storage path on the destination and
// classifies the result, so a 200 means this write's bytes are there. The
// runtime charges the HEAD whatever the outcome.
func (r *PendingReaper) probeBackend(ctx context.Context, be backend.ObjectBackend, p *core.PendingObject) probeOutcome {
	_, err := r.d.Ops.Head(ctx, p.BackendName, be, core.StoragePath(p.ObjectKey, p.StorageKey))

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

// dropIntent deletes a pending row that has no recoverable bytes: the
// backend is gone, or HEAD returned 404. reason is recorded on the audit
// entry so operators can tell the two apart.
func (r *PendingReaper) dropIntent(ctx context.Context, p *core.PendingObject, reason string) batch.ItemOutcome {
	if reason == "backend_removed" {
		r.log.WarnContext(ctx, "backend not registered, dropping intent",
			"backend", p.BackendName, "key", p.ObjectKey, "intent_id", p.IntentID)
	}
	if err := r.d.Store.DeletePending(ctx, p.IntentID); err != nil {
		r.log.ErrorContext(ctx, "delete pending intent",
			"intent_id", p.IntentID, "error", err)
		return batch.ItemFailed
	}
	r.onResolved(ctx, p, "dropped", slog.String("reason", reason))
	return batch.ItemSucceeded
}

// handlePromotion resolves an intent whose backend HEAD returned 200 by
// calling PromotePending and dispatching to one of the four result-code
// handlers. Each handler updates metrics, audit logs, and the resolved/
// failed counters as appropriate.
func (r *PendingReaper) handlePromotion(ctx context.Context, p *core.PendingObject) batch.ItemOutcome {
	result, cleanup, err := r.d.Store.PromotePending(ctx, p)
	if err != nil {
		r.log.ErrorContext(ctx, "promote pending intent",
			"intent_id", p.IntentID, "error", err)
		return batch.ItemFailed
	}
	// Whatever bytes the resolution stopped recording - the copies a promotion
	// displaced, or an intent's own upload it dropped - were queued in its
	// transaction.
	r.d.Placement.Settle(ctx, cleanup)
	switch result {
	case core.PendingPromoteCommitted:
		// The promotion charged the bytes and cleared the intent in one
		// transaction, moving them from what the backend has in flight to what
		// it stores without either total ever missing them.
		r.onResolved(ctx, p, "promoted", slog.Int("displaced_copies", len(cleanup)))
		return batch.ItemSucceeded
	case core.PendingPromoteSuperseded:
		r.onResolved(ctx, p, "superseded")
		return batch.ItemSucceeded
	case core.PendingPromoteCompanionKept:
		r.onResolved(ctx, p, "companion_kept")
		return batch.ItemSucceeded
	case core.PendingPromoteCompanionDiscarded:
		// Nothing recorded these bytes and nothing can vouch for them, so they
		// go and the replication worker rebuilds the copy from one we trust.
		r.onResolved(ctx, p, "companion_discarded")
		return batch.ItemSucceeded
	case core.PendingPromoteAmbiguous:
		r.onPromoteAmbiguous(ctx, p)
		return batch.ItemFailed
	case core.PendingPromoteAlreadyResolved:
		telemetry.PendingIntentsResolvedTotal.WithLabelValues("already_resolved").Inc()
		return batch.ItemSucceeded
	}
	return batch.ItemFailed
}

// onResolved records an intent the reaper settled: the outcome's metric and
// an audit entry naming the intent, plus any outcome-specific fields. The
// store has already applied the outcome, so nothing is left to do here.
func (r *PendingReaper) onResolved(ctx context.Context, p *core.PendingObject, outcome string, extra ...slog.Attr) {
	telemetry.PendingIntentsResolvedTotal.WithLabelValues(outcome).Inc()
	audit.Log(ctx, "pending_reaper."+outcome, append([]slog.Attr{
		slog.String("key", p.ObjectKey),
		slog.String("backend", p.BackendName),
		slog.String("intent_id", p.IntentID),
	}, extra...)...)
}

// onPromoteAmbiguous logs the unexpected branch loudly. The current
// resolver does not produce this result; if it ever fires it's a bug
// worth surfacing rather than silently leaving the row pinned.
func (r *PendingReaper) onPromoteAmbiguous(ctx context.Context, p *core.PendingObject) {
	r.log.WarnContext(ctx, "promotion ambiguous, leaving for operator",
		"intent_id", p.IntentID, "key", p.ObjectKey, "backend", p.BackendName)
	telemetry.PendingIntentsResolvedTotal.WithLabelValues("ambiguous").Inc()
}
