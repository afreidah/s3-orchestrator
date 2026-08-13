// -------------------------------------------------------------------------------
// Admin API - Integrity (Scrub, Checksum Backfill, Reconcile)
//
// Author: Alex Freidah
//
// On-demand counterparts to the scheduled integrity workers: scrub kicks
// one verification pass, backfill-checksums fills SHA-256 columns for
// objects predating integrity verification, and reconcile lists each
// backend, diffs against DB, and imports/removes drift. Scrub and
// BackfillChecksums are also exposed as typed methods so UI handlers
// and tests can read the per-pass counts as Go values.
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// integrityDisabledReason is the skip reason the scrub and backfill endpoints
// report when integrity verification is turned off in config.
const integrityDisabledReason = "integrity verification is not enabled"

// Scrub runs one integrity-verification scrub pass synchronously and
// returns the per-pass counts. batchSize <= 0 means use the configured
// ScrubberBatchSize. observer, when non-nil, receives a start and end step per
// object verified. Skips when integrity verification is not enabled.
func (h *Handler) Scrub(ctx context.Context, batchSize int, observer progress.Observer) adminapi.ScrubResponse {
	icfg := h.backendOps.IntegrityConfig()
	if icfg == nil || !icfg.Enabled {
		return adminapi.ScrubResponse{IntegrityOutcome: adminapi.IntegrityOutcome{
			Status: "skipped",
			Reason: integrityDisabledReason,
		}}
	}
	if batchSize <= 0 {
		batchSize = icfg.ScrubberBatchSize
	}
	sum := h.scrubber.Scrub(ctx, batchSize, observer)
	return adminapi.ScrubResponse{
		IntegrityOutcome: adminapi.IntegrityOutcome{Status: "ok"},
		Checked:          sum.Attempted,
		Failed:           sum.Failed,
		Unreadable:       sum.Skipped,
		Deferred:         sum.Deferred,
	}
}

// handleScrub triggers an on-demand scrub cycle. Accepts an optional batch_size
// query parameter. Streams per-object NDJSON progress when the client accepts
// the stream content type; otherwise returns a single JSON result.
func (h *Handler) handleScrub(w http.ResponseWriter, r *http.Request) {
	batchSize := queryPositiveInt(r.URL.Query().Get("batch_size"))

	if acceptsStream(r) {
		h.streamScrub(w, r, batchSize)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, h.Scrub(r.Context(), batchSize, nil))
}

// handleScrubKey verifies every copy of one object immediately.
//
// Separate from handleScrub because the answers differ in kind: a pass reports
// counts, this reports a verdict per copy. Folding both onto one endpoint would
// mean a response whose shape depends on whether a parameter was supplied.
func (h *Handler) handleScrubKey(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		httputil.WriteJSONError(w, http.StatusBadRequest, "key is required")
		return
	}

	icfg := h.backendOps.IntegrityConfig()
	if icfg == nil || !icfg.Enabled {
		httputil.WriteJSONError(w, http.StatusConflict, integrityDisabledReason)
		return
	}

	copies, err := h.scrubber.ScrubKey(r.Context(), key)
	if err != nil {
		h.internalError(r.Context(), w, "failed to verify object", err, slog.String("key", key))
		return
	}
	if len(copies) == 0 {
		httputil.WriteJSONError(w, http.StatusNotFound, "no copies of that key are recorded")
		return
	}

	resp := adminapi.ScrubKeyResponse{Key: key}
	for _, c := range copies {
		resp.Copies = append(resp.Copies, wireCopyResult(c))
	}
	httputil.WriteJSON(w, http.StatusOK, resp)
}

// scrubOutcomes words each verdict for the wire. The scrubber reports what it
// established; what that means to an operator, including what became of a copy
// that failed, is the transport's business.
var scrubOutcomes = map[worker.CopyOutcome]adminapi.CopyScrubResult{
	worker.CopyVerified: {Outcome: adminapi.CopyVerified},
	worker.CopyMismatch: {
		Outcome: adminapi.CopyMismatch,
		Detail:  "stored bytes did not match the recorded hash; the copy was discarded and will be rebuilt",
	},
	worker.CopyUnreadable: {Outcome: adminapi.CopyUnreadable, Detail: "the copy could not be read"},
	worker.CopyNotHashed:  {Outcome: adminapi.CopyNotHashed, Detail: "no stored content hash to verify against"},
}

// wireCopyResult renders one verdict. An outcome this handler does not know is
// reported as unreadable rather than passed through, so a vocabulary that grows
// on the worker side cannot make a copy look verified here.
func wireCopyResult(c worker.CopyVerification) adminapi.CopyScrubResult {
	res, ok := scrubOutcomes[c.Outcome]
	if !ok {
		res = scrubOutcomes[worker.CopyUnreadable]
	}
	res.Backend = c.Backend
	return res
}

// streamScrub runs a scrub as an NDJSON step stream, one "verifying <key>" line
// per object plus a terminal summary of checked/failed counts.
func (h *Handler) streamScrub(w http.ResponseWriter, r *http.Request, batchSize int) {
	h.streamSteps(w, "scrub", "verifying", true, func(obs progress.Observer) (stepResult, error) {
		res := h.Scrub(r.Context(), batchSize, obs)
		if res.Status == "skipped" {
			return stepResult{Skipped: res.Reason}, nil
		}
		return stepResult{
			Processed: res.Checked,
			Summary: fmt.Sprintf("checked %d, failed %d, unreadable %d, deferred %d",
				res.Checked, res.Failed, res.Unreadable, res.Deferred),
			Fields: map[string]any{
				"checked":    res.Checked,
				"failed":     res.Failed,
				"unreadable": res.Unreadable,
				"deferred":   res.Deferred,
			},
		}, nil
	})
}

// BackfillChecksums computes and stores content hashes for objects that
// don't have one. It processes batchSize objects per pass, pausing for
// pause between passes to rate-limit backend reads, and stops after
// maxObjects objects (maxObjects <= 0 drains the whole backlog). batchSize
// <= 0 means use 100. Done reports whether the backlog is fully drained.
// observer, when non-nil, receives a start and end event for each object so a
// streaming caller can report per-object progress. Skips when integrity
// verification is not enabled.
func (h *Handler) BackfillChecksums(ctx context.Context, batchSize, maxObjects int, pause time.Duration, observer progress.Observer) adminapi.BackfillChecksumsResponse {
	icfg := h.backendOps.IntegrityConfig()
	if icfg == nil || !icfg.Enabled {
		return adminapi.BackfillChecksumsResponse{IntegrityOutcome: adminapi.IntegrityOutcome{
			Status: "skipped",
			Reason: integrityDisabledReason,
		}}
	}
	if batchSize <= 0 {
		batchSize = 100
	}

	h.log.InfoContext(ctx, "Backfill-checksums started",
		"batch_size", batchSize, "max_objects", maxObjects, "pause", pause)

	var total int
	done := h.drainBackfill(ctx, batchSize, maxObjects, pause, backfillCounter(observer, &total), &total)
	return adminapi.BackfillChecksumsResponse{
		IntegrityOutcome: adminapi.IntegrityOutcome{Status: "ok"},
		Processed:        total,
		Done:             done,
	}
}

// backfillCounter wraps observer so each successfully hashed object bumps total,
// keeping the cumulative count in step with the per-object steps the caller
// renders. The wrapped observer may be nil.
func backfillCounter(observer progress.Observer, total *int) progress.Observer {
	return func(s progress.Step) {
		if s.Phase == progress.PhaseEnd && s.Status == progress.StatusOK {
			*total++
		}
		if observer != nil {
			observer(s)
		}
	}
}

// drainBackfill runs backfill passes until the backlog drains, the max-objects
// cap is hit, or the context is cancelled. Returns true only when the backlog
// was fully drained.
func (h *Handler) drainBackfill(ctx context.Context, batchSize, maxObjects int, pause time.Duration, observer progress.Observer, total *int) bool {
	for offset := 0; ; {
		_, nextOffset := h.scrubber.Backfill(ctx, batchSize, offset, observer)
		if nextOffset == 0 {
			return true
		}
		offset = nextOffset
		if maxObjects > 0 && *total >= maxObjects {
			return false
		}
		if ctx.Err() != nil {
			return false
		}
		if pause > 0 && !sleepOrCancel(ctx, pause) {
			return false
		}
	}
}

// sleepOrCancel waits for d or for ctx to be cancelled, returning false when
// cancellation wins so the caller stops early.
func sleepOrCancel(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// handleBackfillChecksums triggers a checksum backfill pass. Optional query
// parameters: batch_size (objects per pass), max (cap objects this request,
// 0 = drain all), delay_ms (pause between passes to rate-limit backend reads).
// When the client accepts the NDJSON stream content type, progress is streamed
// line by line; otherwise a single JSON result is returned.
func (h *Handler) handleBackfillChecksums(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	batchSize := queryPositiveInt(q.Get("batch_size"))
	maxObjects := queryPositiveInt(q.Get("max"))
	pause := time.Duration(queryPositiveInt(q.Get("delay_ms"))) * time.Millisecond

	if acceptsStream(r) {
		h.streamBackfillChecksums(w, r, batchSize, maxObjects, pause)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, h.BackfillChecksums(r.Context(), batchSize, maxObjects, pause, nil))
}

// streamBackfillChecksums runs a backfill as an NDJSON step stream, one
// "hashing <key>" line per object plus a terminal result.
func (h *Handler) streamBackfillChecksums(w http.ResponseWriter, r *http.Request, batchSize, maxObjects int, pause time.Duration) {
	h.streamSteps(w, "backfill-checksums", "hashing", true, func(obs progress.Observer) (stepResult, error) {
		res := h.BackfillChecksums(r.Context(), batchSize, maxObjects, pause, obs)
		if res.Status == "skipped" {
			return stepResult{Skipped: res.Reason}, nil
		}
		return stepResult{Processed: res.Processed, Fields: map[string]any{"done": res.Done}}, nil
	})
}

// queryPositiveInt parses a positive integer query parameter, returning 0
// when the value is absent, malformed, or non-positive.
func queryPositiveInt(v string) int {
	if n, err := strconv.ParseInt(v, 10, 32); err == nil && n > 0 {
		return int(n)
	}
	return 0
}

// handleReconcile triggers an on-demand reconciliation. Lists objects on
// each backend, diffs against DB entries, imports untracked objects, and
// removes stale entries. Use ?backend=name to scope to a single backend.
func (h *Handler) handleReconcile(w http.ResponseWriter, r *http.Request) {
	backendName := r.URL.Query().Get("backend")

	if h.reconciler == nil {
		httputil.WriteJSONError(w, http.StatusServiceUnavailable, "reconciler not configured")
		return
	}

	h.log.InfoContext(r.Context(), "reconcile triggered", "backend", backendName)

	if acceptsStream(r) {
		h.streamReconcile(w, r, backendName)
		return
	}

	result, err := h.reconciler.Reconcile(r.Context(), backendName)
	if err != nil {
		h.log.ErrorContext(r.Context(), "reconcile failed", "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	h.log.InfoContext(r.Context(), "reconcile complete",
		"imported", result.Imported, "removed", result.Removed,
		"backends_scanned", result.BackendsScanned)

	httputil.WriteJSON(w, http.StatusOK, adminapi.ReconcileResponse{
		IntegrityOutcome: adminapi.IntegrityOutcome{Status: "ok"},
		Imported:         result.Imported,
		Removed:          result.Removed,
		BackendsScanned:  result.BackendsScanned,
	})
}

// streamReconcile runs a reconcile as an NDJSON step stream, one
// "reconciling <backend>" line per backend plus a terminal summary.
func (h *Handler) streamReconcile(w http.ResponseWriter, r *http.Request, backendName string) {
	h.streamSteps(w, "reconcile", "reconciling", true, func(obs progress.Observer) (stepResult, error) {
		result, err := h.reconciler.ReconcileStreaming(r.Context(), backendName, obs)
		if err != nil {
			h.log.ErrorContext(r.Context(), "reconcile failed", "error", err)
			return stepResult{}, err
		}
		return stepResult{
			Summary: fmt.Sprintf("imported %d, removed %d across %d backend(s)",
				result.Imported, result.Removed, result.BackendsScanned),
			Fields: map[string]any{
				"imported":         result.Imported,
				"removed":          result.Removed,
				"backends_scanned": result.BackendsScanned,
			},
		}, nil
	})
}
