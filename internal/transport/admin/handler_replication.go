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
	"fmt"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// -------------------------------------------------------------------------
// REPLICATION
// -------------------------------------------------------------------------

// handleReplicate triggers one replication cycle. Replication fans objects out
// across a worker pool, so streamed steps render as complete labeled lines
// rather than live prefixes, which would interleave.
func (h *Handler) handleReplicate(w http.ResponseWriter, r *http.Request) {
	h.servePass(w, r, passEndpoint[worker.ReplicationSummary]{
		op:      "replicate",
		verb:    "replicating",
		failMsg: "replication failed",
		run:     h.replication.Replicate,
		body: func(o adminapi.Outcome, res worker.ReplicationSummary) any {
			return adminapi.ReplicateResponse{Outcome: o, CopiesCreated: res.CopiesCreated, Failed: res.Failed}
		},
		summary: func(res worker.ReplicationSummary) (int, string) {
			return res.CopiesCreated, fmt.Sprintf("created %d copies", res.CopiesCreated)
		},
	})
}

// -------------------------------------------------------------------------
// OVER-REPLICATION
// -------------------------------------------------------------------------

// handleOverReplicationStatus returns the count of over-replicated objects.
func (h *Handler) handleOverReplicationStatus(w http.ResponseWriter, r *http.Request) {
	res, err := h.replication.CountSurplus(r.Context())
	if reason, skipped := ops.SkipReason(err); skipped {
		httputil.WriteJSON(w, http.StatusOK, adminapi.OverReplicationStatusResponse{
			Outcome: adminapi.Outcome{Status: statusSkipped, Reason: reason},
		})
		return
	}
	if err != nil {
		h.internalError(r.Context(), w, "failed to count over-replicated objects", err)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.OverReplicationStatusResponse{
		Outcome: adminapi.Outcome{Status: statusOK},
		Factor:  res.Factor,
		Pending: res.Pending,
	})
}

// handleOverReplicationClean triggers an immediate over-replication cleanup
// pass. Accepts an optional batch_size query parameter. The cleaner fans
// objects out across a worker pool, so streamed steps render as complete
// labeled lines.
func (h *Handler) handleOverReplicationClean(w http.ResponseWriter, r *http.Request) {
	batchSize := httputil.QueryPositiveInt(r.URL.Query().Get("batch_size"))

	h.servePass(w, r, passEndpoint[worker.OverReplicationSummary]{
		op:      "over-replication",
		verb:    "removing",
		failMsg: "over-replication cleanup failed",
		run: func(ctx context.Context, obs progress.Observer) (worker.OverReplicationSummary, error) {
			return h.replication.CleanExcess(ctx, batchSize, obs)
		},
		body: func(o adminapi.Outcome, res worker.OverReplicationSummary) any {
			return adminapi.OverReplicationCleanResponse{Outcome: o, CopiesRemoved: res.CopiesRemoved, Failed: res.Failed}
		},
		summary: func(res worker.OverReplicationSummary) (int, string) {
			return res.CopiesRemoved, fmt.Sprintf("removed %d copies", res.CopiesRemoved)
		},
	})
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
	if h.replMetrics == nil {
		httputil.WriteJSONError(w, http.StatusServiceUnavailable, "replication status not available")
		return
	}
	snap := h.replMetrics.ReplicationSnapshot(r.Context())
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
