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
	"net/http"
	"strconv"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// ScrubResult is the outcome of one on-demand scrub cycle.
type ScrubResult struct {
	Status  string // "ok" or "skipped"
	Reason  string // populated when Status is "skipped"
	Checked int
	Failed  int
}

// Scrub runs one integrity-verification scrub pass synchronously and
// returns the per-pass counts. batchSize <= 0 means use the configured
// ScrubberBatchSize. Skips when integrity verification is not enabled.
func (h *Handler) Scrub(ctx context.Context, batchSize int) ScrubResult {
	icfg := h.backendOps.IntegrityConfig()
	if icfg == nil || !icfg.Enabled {
		return ScrubResult{Status: "skipped", Reason: "integrity verification is not enabled"}
	}
	if batchSize <= 0 {
		batchSize = icfg.ScrubberBatchSize
	}
	checked, failed := h.scrubber.Scrub(ctx, batchSize)
	return ScrubResult{Status: "ok", Checked: checked, Failed: failed}
}

// handleScrub triggers an on-demand scrub cycle. Accepts an optional
// batch_size query parameter (defaults to the configured scrubber batch size).
func (h *Handler) handleScrub(w http.ResponseWriter, r *http.Request) {
	batchSize := queryPositiveInt(r.URL.Query().Get("batch_size"))

	res := h.Scrub(r.Context(), batchSize)
	if res.Status == "skipped" {
		httputil.WriteJSON(w, http.StatusOK, map[string]any{"status": "skipped", "reason": res.Reason})
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"checked": res.Checked,
		"failed":  res.Failed,
	})
}

// BackfillChecksumsResult is the outcome of a checksum backfill pass.
type BackfillChecksumsResult struct {
	Status    string // "ok" or "skipped"
	Reason    string // populated when Status is "skipped"
	Processed int
	Done      bool // true when no unhashed objects remained after this pass
}

// BackfillChecksums computes and stores content hashes for objects that
// don't have one. It processes batchSize objects per pass, pausing for
// pause between passes to rate-limit backend reads, and stops after
// maxObjects objects (maxObjects <= 0 drains the whole backlog). batchSize
// <= 0 means use 100. Done reports whether the backlog is fully drained.
// Skips when integrity verification is not enabled.
func (h *Handler) BackfillChecksums(ctx context.Context, batchSize, maxObjects int, pause time.Duration) BackfillChecksumsResult {
	icfg := h.backendOps.IntegrityConfig()
	if icfg == nil || !icfg.Enabled {
		return BackfillChecksumsResult{Status: "skipped", Reason: "integrity verification is not enabled"}
	}
	if batchSize <= 0 {
		batchSize = 100
	}

	h.log.InfoContext(ctx, "Backfill-checksums started",
		"batch_size", batchSize, "max_objects", maxObjects, "pause", pause)
	var total int
	done := false
	for offset := 0; ; {
		processed, nextOffset := h.scrubber.Backfill(ctx, batchSize, offset)
		total += processed
		if nextOffset == 0 {
			done = true
			break
		}
		offset = nextOffset
		if maxObjects > 0 && total >= maxObjects {
			break
		}
		if ctx.Err() != nil {
			break
		}
		if pause > 0 {
			select {
			case <-ctx.Done():
				return BackfillChecksumsResult{Status: "ok", Processed: total}
			case <-time.After(pause):
			}
		}
	}
	return BackfillChecksumsResult{Status: "ok", Processed: total, Done: done}
}

// handleBackfillChecksums triggers a checksum backfill pass. Optional query
// parameters: batch_size (objects per pass), max (cap objects this request,
// 0 = drain all), delay_ms (pause between passes to rate-limit backend reads).
func (h *Handler) handleBackfillChecksums(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	batchSize := queryPositiveInt(q.Get("batch_size"))
	maxObjects := queryPositiveInt(q.Get("max"))
	pause := time.Duration(queryPositiveInt(q.Get("delay_ms"))) * time.Millisecond

	res := h.BackfillChecksums(r.Context(), batchSize, maxObjects, pause)
	if res.Status == "skipped" {
		httputil.WriteJSON(w, http.StatusOK, map[string]any{"status": "skipped", "reason": res.Reason})
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"processed": res.Processed,
		"done":      res.Done,
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

	result, err := h.reconciler.Reconcile(r.Context(), backendName)
	if err != nil {
		h.log.ErrorContext(r.Context(), "reconcile failed", "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	h.log.InfoContext(r.Context(), "reconcile complete",
		"imported", result.Imported, "removed", result.Removed,
		"backends_scanned", result.BackendsScanned)

	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"status":           "ok",
		"imported":         result.Imported,
		"removed":          result.Removed,
		"backends_scanned": result.BackendsScanned,
	})
}
