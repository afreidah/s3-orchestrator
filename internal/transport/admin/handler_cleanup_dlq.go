// -------------------------------------------------------------------------------
// Admin API - Cleanup Dead-Letter Queue
//
// Author: Alex Freidah
//
// Read and recovery endpoints for cleanup_dlq: list the dead-lettered orphans
// an operator needs to investigate, and requeue them back into cleanup_queue
// once the backing backend has recovered so the worker retries the deletes and
// orphan_bytes settles on its own.
// -------------------------------------------------------------------------------

package admin

import (
	"net/http"
	"strconv"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"

	"log/slog"
)

// defaultCleanupDLQLimit bounds a cleanup-dlq listing when the caller does not
// supply an explicit limit; maxCleanupDLQLimit caps it so a single request
// cannot pull an unbounded page.
const (
	defaultCleanupDLQLimit = 50
	maxCleanupDLQLimit     = 500
)

// handleCleanupDLQ lists dead-lettered cleanups plus the total DLQ depth,
// optionally scoped to one backend via ?backend=NAME.
func (h *Handler) handleCleanupDLQ(w http.ResponseWriter, r *http.Request) {
	backend := r.URL.Query().Get("backend")
	limit := parseLimit(r.URL.Query().Get("limit"), defaultCleanupDLQLimit, maxCleanupDLQLimit)

	depth, err := h.cleanup.CleanupDLQDepth(r.Context())
	if err != nil {
		h.internalError(r.Context(), w, "failed to fetch cleanup dlq depth", err)
		return
	}
	items, err := h.cleanup.ListCleanupDLQ(r.Context(), backend, limit)
	if err != nil {
		h.internalError(r.Context(), w, "failed to list cleanup dlq", err)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.CleanupDLQResponse{
		Depth: depth,
		Items: cleanupDLQItems(items),
	})
}

// handleCleanupDLQRequeue moves dead-lettered rows back into cleanup_queue so
// the worker retries them, optionally scoped to one backend via ?backend=NAME.
func (h *Handler) handleCleanupDLQRequeue(w http.ResponseWriter, r *http.Request) {
	backend := r.URL.Query().Get("backend")

	n, err := h.cleanup.RequeueCleanupDLQ(r.Context(), backend)
	if err != nil {
		h.internalError(r.Context(), w, "failed to requeue cleanup dlq", err)
		return
	}

	audit.Log(r.Context(), "cleanup_dlq.requeue",
		slog.String("backend", backend),
		slog.Int64("requeued", n))
	httputil.WriteJSON(w, http.StatusOK, adminapi.CleanupDLQRequeueResponse{
		Backend:  backend,
		Requeued: n,
	})
}

// cleanupDLQItems maps the store rows onto the shared wire type.
func cleanupDLQItems(items []core.CleanupDLQItem) []adminapi.CleanupDLQItem {
	out := make([]adminapi.CleanupDLQItem, 0, len(items))
	for i := range items {
		out = append(out, adminapi.CleanupDLQItem{
			Backend:       items[i].BackendName,
			ObjectKey:     items[i].ObjectKey,
			Reason:        items[i].Reason,
			SizeBytes:     items[i].SizeBytes,
			Attempts:      items[i].Attempts,
			FirstEnqueued: items[i].FirstEnqueued,
			MovedAt:       items[i].MovedAt,
			LastError:     items[i].LastError,
		})
	}
	return out
}

// parseLimit returns a positive page limit from the raw query value, falling
// back to def when unset or unparseable and clamping to max.
func parseLimit(raw string, def, maxLimit int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	if n > maxLimit {
		return maxLimit
	}
	return n
}
