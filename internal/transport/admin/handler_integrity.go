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
	batchSize := 0
	if bs := r.URL.Query().Get("batch_size"); bs != "" {
		if v, err := strconv.ParseInt(bs, 10, 32); err == nil && v > 0 {
			batchSize = int(v)
		}
	}

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
}

// BackfillChecksums computes and stores content hashes for objects that
// don't have one, paginating internally until all objects are processed
// or the context is cancelled. batchSize <= 0 means use 100. Skips when
// integrity verification is not enabled.
func (h *Handler) BackfillChecksums(ctx context.Context, batchSize int) BackfillChecksumsResult {
	icfg := h.backendOps.IntegrityConfig()
	if icfg == nil || !icfg.Enabled {
		return BackfillChecksumsResult{Status: "skipped", Reason: "integrity verification is not enabled"}
	}
	if batchSize <= 0 {
		batchSize = 100
	}

	h.log.InfoContext(ctx, "Backfill-checksums started", "batch_size", batchSize)
	var total int
	for offset := 0; ; {
		processed, nextOffset := h.scrubber.Backfill(ctx, batchSize, offset)
		total += processed
		if nextOffset == 0 {
			break
		}
		offset = nextOffset
	}
	return BackfillChecksumsResult{Status: "ok", Processed: total}
}

// handleBackfillChecksums triggers a checksum backfill pass.
func (h *Handler) handleBackfillChecksums(w http.ResponseWriter, r *http.Request) {
	batchSize := 0
	if bs := r.URL.Query().Get("batch_size"); bs != "" {
		if v, err := strconv.ParseInt(bs, 10, 32); err == nil && v > 0 {
			batchSize = int(v)
		}
	}

	res := h.BackfillChecksums(r.Context(), batchSize)
	if res.Status == "skipped" {
		httputil.WriteJSON(w, http.StatusOK, map[string]any{"status": "skipped", "reason": res.Reason})
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "processed": res.Processed})
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
