// -------------------------------------------------------------------------------
// Admin API - Integrity (Scrub, Checksum Backfill, Reconcile)
//
// Author: Alex Freidah
//
// On-demand counterparts to the scheduled integrity workers: scrub kicks
// one verification pass, backfill-checksums fills SHA-256 columns for
// objects predating integrity verification, and reconcile lists each
// backend, diffs against DB, and imports/removes drift. Each handler parses
// the request, calls the matching operation, and renders the outcome; the
// passes themselves live in internal/ops.
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// -------------------------------------------------------------------------
// SCRUB
// -------------------------------------------------------------------------

// handleScrub triggers an on-demand scrub cycle. Accepts optional batch_size
// and backend query parameters. A pass counts the copies it read as checked and
// the ones it could not decode as unreadable.
func (h *Handler) handleScrub(w http.ResponseWriter, r *http.Request) {
	batchSize := httputil.QueryPositiveInt(r.URL.Query().Get("batch_size"))
	backend, ok := h.backendParam(w, r)
	if !ok {
		return
	}

	h.servePass(w, r, passEndpoint[batch.Summary]{
		op:         "scrub",
		verb:       "verifying",
		sequential: true,
		failMsg:    "scrub failed",
		run: func(ctx context.Context, obs progress.Observer) (batch.Summary, error) {
			return h.integrity.Scrub(ctx, batchSize, backend, obs)
		},
		body: func(o adminapi.Outcome, res batch.Summary) any {
			return adminapi.ScrubResponse{
				Outcome:    o,
				Checked:    res.Attempted,
				Failed:     res.Failed,
				Unreadable: res.Skipped,
				Deferred:   res.Deferred,
			}
		},
		summary: func(res batch.Summary) (int, string) {
			return res.Attempted, fmt.Sprintf("checked %d, failed %d, unreadable %d, deferred %d",
				res.Attempted, res.Failed, res.Skipped, res.Deferred)
		},
	})
}

// handleScrubKey verifies every copy of one object immediately.
//
// Separate from handleScrub because the answers differ in kind: a pass reports
// counts, this reports a verdict per copy. Folding both onto one endpoint would
// mean a response whose shape depends on whether a parameter was supplied.
func (h *Handler) handleScrubKey(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")

	copies, err := h.integrity.VerifyKey(r.Context(), key)
	switch {
	case errors.Is(err, ops.ErrKeyRequired):
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, ops.ErrNotFound):
		httputil.WriteJSONError(w, http.StatusNotFound, "no copies of that key are recorded")
		return
	case err != nil:
		if reason, skipped := ops.SkipReason(err); skipped {
			httputil.WriteJSONError(w, http.StatusConflict, reason)
			return
		}
		h.internalError(r.Context(), w, "failed to verify object", err, slog.String("key", key))
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

// -------------------------------------------------------------------------
// CHECKSUM BACKFILL
// -------------------------------------------------------------------------

// handleBackfillChecksums triggers a checksum backfill pass. Optional query
// parameters: batch_size (objects per pass), max (cap objects this request,
// 0 = drain all), delay_ms (pause between passes to rate-limit backend reads),
// and backend.
func (h *Handler) handleBackfillChecksums(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	batchSize := httputil.QueryPositiveInt(q.Get("batch_size"))
	maxObjects := httputil.QueryPositiveInt(q.Get("max"))
	pause := time.Duration(httputil.QueryPositiveInt(q.Get("delay_ms"))) * time.Millisecond
	backend, ok := h.backendParam(w, r)
	if !ok {
		return
	}

	h.servePass(w, r, passEndpoint[ops.BackfillResult]{
		op:         "backfill-checksums",
		verb:       "hashing",
		sequential: true,
		failMsg:    "backfill failed",
		run: func(ctx context.Context, obs progress.Observer) (ops.BackfillResult, error) {
			return h.integrity.BackfillChecksums(ctx, batchSize, maxObjects, pause, backend, obs)
		},
		body: func(o adminapi.Outcome, res ops.BackfillResult) any {
			return adminapi.BackfillChecksumsResponse{
				Outcome:    o,
				Processed:  res.Succeeded,
				Unreadable: res.Skipped,
				Done:       res.Done,
			}
		},
		summary: func(res ops.BackfillResult) (int, string) {
			return res.Succeeded, fmt.Sprintf("hashed %d, unreadable %d", res.Succeeded, res.Skipped)
		},
	})
}

// -------------------------------------------------------------------------
// UNREADABLE COPIES
// -------------------------------------------------------------------------

// handleListUnreadable lists copies that are encrypted with no key. Accepts an
// optional limit query parameter.
func (h *Handler) handleListUnreadable(w http.ResponseWriter, r *http.Request) {
	limit := httputil.QueryPositiveInt(r.URL.Query().Get("limit"))
	res, err := h.integrity.ListUnreadable(r.Context(), limit)
	if err != nil {
		h.internalError(r.Context(), w, "failed to list unreadable copies", err)
		return
	}
	resp := adminapi.UnreadableListResponse{Total: res.Total, Copies: make([]adminapi.UnreadableCopy, 0, len(res.Copies))}
	for i := range res.Copies {
		c := &res.Copies[i]
		resp.Copies = append(resp.Copies, adminapi.UnreadableCopy{
			Key: c.ObjectKey, Backend: c.BackendName, SizeBytes: c.SizeBytes, CreatedAt: c.CreatedAt,
		})
	}
	httputil.WriteJSON(w, http.StatusOK, resp)
}

// handlePurgeUnreadable deletes every copy that is encrypted with no key.
// Accepts an optional batch_size query parameter.
func (h *Handler) handlePurgeUnreadable(w http.ResponseWriter, r *http.Request) {
	batchSize := httputil.QueryPositiveInt(r.URL.Query().Get("batch_size"))

	h.servePass(w, r, passEndpoint[batch.Summary]{
		op:         "purge-unreadable",
		verb:       "purging",
		sequential: true,
		run: func(ctx context.Context, obs progress.Observer) (batch.Summary, error) {
			return h.integrity.PurgeUnreadable(ctx, batchSize, obs), nil
		},
		body: func(o adminapi.Outcome, res batch.Summary) any {
			return adminapi.UnreadablePurgeResponse{Outcome: o, Purged: res.Succeeded, Failed: res.Failed}
		},
		summary: func(res batch.Summary) (int, string) {
			return res.Succeeded, fmt.Sprintf("purged %d, failed %d", res.Succeeded, res.Failed)
		},
	})
}

// -------------------------------------------------------------------------
// RECONCILE
// -------------------------------------------------------------------------

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

	h.servePass(w, r, passEndpoint[*worker.ReconcileResult]{
		op:         "reconcile",
		verb:       "reconciling",
		sequential: true,
		failMsg:    "reconcile failed",
		run: func(ctx context.Context, obs progress.Observer) (*worker.ReconcileResult, error) {
			return h.reconciler.Reconcile(ctx, backendName, obs)
		},
		body: func(o adminapi.Outcome, res *worker.ReconcileResult) any {
			resp := adminapi.ReconcileResponse{Outcome: o}
			if res != nil {
				resp.Imported, resp.Removed, resp.BackendsScanned = res.Imported, res.Removed, res.BackendsScanned
			}
			return resp
		},
		summary: func(res *worker.ReconcileResult) (int, string) {
			return res.Imported, fmt.Sprintf("imported %d, removed %d across %d backend(s)",
				res.Imported, res.Removed, res.BackendsScanned)
		},
	})
}
