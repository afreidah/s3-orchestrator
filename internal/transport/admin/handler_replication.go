// -------------------------------------------------------------------------------
// Admin API - Replication and Over-Replication Control
//
// Author: Alex Freidah
//
// /admin/api/replicate triggers a one-shot replication pass to fill in
// under-replicated objects; the over-replication endpoints expose count
// + cleanup so operators can drive excess-copy removal from outside the
// scheduled cleaner. Each handler renders what the matching operation in
// internal/ops reports.
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// -------------------------------------------------------------------------
// REPLICATION
// -------------------------------------------------------------------------

// handleReplicate triggers one replication cycle.
func (h *Handler) handleReplicate(w http.ResponseWriter, r *http.Request) {
	h.servePass(w, r, ReplicatePass(h.d.Ops))
}

// -------------------------------------------------------------------------
// OVER-REPLICATION
// -------------------------------------------------------------------------

// handleOverReplicationStatus returns the count of over-replicated objects.
func (h *Handler) handleOverReplicationStatus(w http.ResponseWriter, r *http.Request) {
	res, err := h.d.Ops.Replication.CountSurplus(r.Context())
	if reason, skipped := ops.SkipReason(err); skipped {
		httputil.WriteJSON(w, http.StatusOK, adminapi.OverReplicationStatusResponse{
			Outcome: adminapi.Outcome{Status: adminapi.StatusSkipped, Reason: reason},
		})
		return
	}
	if err != nil {
		httputil.InternalError(r.Context(), h.log, w, "failed to count over-replicated objects", err)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.OverReplicationStatusResponse{
		Outcome: adminapi.Outcome{Status: adminapi.StatusOK},
		Factor:  res.Factor,
		Pending: res.Pending,
	})
}

// handleOverReplicationClean triggers an immediate over-replication cleanup
// pass. Accepts an optional batch_size query parameter.
func (h *Handler) handleOverReplicationClean(w http.ResponseWriter, r *http.Request) {
	batchSize, ok := httputil.RequireQueryCount(w, r, paramBatchSize, 0, httputil.NoCeiling)
	if !ok {
		return
	}
	h.servePass(w, r, CleanExcessPass(h.d.Ops, batchSize))
}

// -------------------------------------------------------------------------
// CONSUMER INTERFACE
// -------------------------------------------------------------------------

// replicationSnapshotter is the narrow view of the metrics collector the
// replication-status endpoint needs; *metrics.Collector satisfies it.
type replicationSnapshotter interface {
	ReplicationSnapshot(ctx context.Context) metrics.ReplicationSnapshot
}

// handleReplicationStatus returns the last-computed replication backlog
// (under-replicated and over-replicated counts plus the factor), served from
// the fleet snapshot so it can be polled cheaply and every instance gives the
// same answer. Returns 503 when the collector is not wired or no snapshot has
// been computed yet.
func (h *Handler) handleReplicationStatus(w http.ResponseWriter, r *http.Request) {
	if h.d.ReplMetrics == nil {
		httputil.WriteJSONError(w, http.StatusServiceUnavailable, "replication status not available")
		return
	}
	snap := h.d.ReplMetrics.ReplicationSnapshot(r.Context())
	if !snap.Ready {
		httputil.WriteJSONError(w, http.StatusServiceUnavailable, "replication status not yet computed")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, adminapi.ReplicationStatusResponse{
		Factor:          snap.Factor,
		UnderReplicated: snap.UnderReplicated,
		OverReplicated:  snap.OverReplicated,
		ComputedAt:      snap.ComputedAt,
	})
}
