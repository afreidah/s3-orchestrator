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
	"fmt"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// -------------------------------------------------------------------------
// REPLICATION
// -------------------------------------------------------------------------

// handleReplicate triggers one replication cycle. Streams per-object NDJSON
// progress when the client accepts the stream content type; otherwise returns a
// single JSON result.
func (h *Handler) handleReplicate(w http.ResponseWriter, r *http.Request) {
	if acceptsStream(r) {
		h.streamReplicate(w, r)
		return
	}

	res, err := h.replication.Replicate(r.Context(), nil)
	if reason, skipped := skipReason(err); skipped {
		httputil.WriteJSON(w, http.StatusOK, adminapi.ReplicateResponse{
			Status: statusSkipped, Reason: reason,
		})
		return
	}
	if err != nil {
		h.internalError(r.Context(), w, "replication failed", err)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.ReplicateResponse{
		Status:        statusOK,
		CopiesCreated: res.CopiesCreated,
		Failed:        res.Failed,
	})
}

// streamReplicate runs a replication cycle as an NDJSON step stream, one
// "replicating <key>" line per object plus a terminal summary. Replication fans
// objects out across a worker pool, so steps render as complete labeled lines
// (sequential=false) to avoid interleaved output.
func (h *Handler) streamReplicate(w http.ResponseWriter, r *http.Request) {
	h.streamSteps(w, "replicate", "replicating", false, func(obs progress.Observer) (stepResult, error) {
		res, err := h.replication.Replicate(r.Context(), obs)
		if reason, skipped := skipReason(err); skipped {
			return stepResult{Skipped: reason}, nil
		}
		if err != nil {
			return stepResult{}, err
		}
		return stepResult{
			Processed: res.CopiesCreated,
			Summary:   fmt.Sprintf("created %d copies", res.CopiesCreated),
			Fields:    map[string]any{"copies_created": res.CopiesCreated, "failed": res.Failed},
		}, nil
	})
}

// -------------------------------------------------------------------------
// OVER-REPLICATION
// -------------------------------------------------------------------------

// handleOverReplicationStatus returns the count of over-replicated objects.
func (h *Handler) handleOverReplicationStatus(w http.ResponseWriter, r *http.Request) {
	res, err := h.replication.CountSurplus(r.Context())
	if reason, skipped := skipReason(err); skipped {
		httputil.WriteJSON(w, http.StatusOK, adminapi.OverReplicationStatusResponse{
			Status: statusSkipped, Reason: reason,
		})
		return
	}
	if err != nil {
		h.internalError(r.Context(), w, "failed to count over-replicated objects", err)
		return
	}

	telemetry.OverReplicationPending.Set(float64(res.Pending))
	httputil.WriteJSON(w, http.StatusOK, adminapi.OverReplicationStatusResponse{
		Status:  statusOK,
		Factor:  res.Factor,
		Pending: res.Pending,
	})
}

// handleOverReplicationClean triggers an immediate over-replication cleanup
// pass. Accepts an optional batch_size query parameter.
func (h *Handler) handleOverReplicationClean(w http.ResponseWriter, r *http.Request) {
	batchSize := httputil.QueryPositiveInt(r.URL.Query().Get("batch_size"))

	if acceptsStream(r) {
		h.streamOverReplication(w, r, batchSize)
		return
	}

	res, err := h.replication.CleanExcess(r.Context(), batchSize, nil)
	if reason, skipped := skipReason(err); skipped {
		httputil.WriteJSON(w, http.StatusOK, adminapi.OverReplicationCleanResponse{
			Status: statusSkipped, Reason: reason,
		})
		return
	}
	if err != nil {
		h.internalError(r.Context(), w, "over-replication cleanup failed", err)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.OverReplicationCleanResponse{
		Status:        statusOK,
		CopiesRemoved: res.CopiesRemoved,
		Failed:        res.Failed,
	})
}

// streamOverReplication runs an over-replication cleanup as an NDJSON step
// stream, one "removing <key>" line per object plus a terminal summary. The
// cleaner fans objects out across a worker pool, so steps render as complete
// labeled lines (sequential=false) to avoid interleaved output.
func (h *Handler) streamOverReplication(w http.ResponseWriter, r *http.Request, batchSize int) {
	h.streamSteps(w, "over-replication", "removing", false, func(obs progress.Observer) (stepResult, error) {
		res, err := h.replication.CleanExcess(r.Context(), batchSize, obs)
		if reason, skipped := skipReason(err); skipped {
			return stepResult{Skipped: reason}, nil
		}
		if err != nil {
			return stepResult{}, err
		}
		return stepResult{
			Processed: res.CopiesRemoved,
			Summary:   fmt.Sprintf("removed %d copies", res.CopiesRemoved),
			Fields:    map[string]any{"copies_removed": res.CopiesRemoved, "failed": res.Failed},
		}, nil
	})
}

// -------------------------------------------------------------------------
// CONSUMER INTERFACE
// -------------------------------------------------------------------------

// replicationSnapshotter is the narrow view of the metrics collector the
// replication-status endpoint needs; *metrics.Collector satisfies it.
type replicationSnapshotter interface {
	ReplicationSnapshot() metrics.ReplicationSnapshot
}

// handleReplicationStatus returns the last-computed replication backlog
// (under-replicated and over-replicated counts plus the factor), served from
// the metrics collector's snapshot so it can be polled cheaply. Returns 503
// when the collector is not wired or has not computed a snapshot yet.
func (h *Handler) handleReplicationStatus(w http.ResponseWriter, r *http.Request) {
	if h.replMetrics == nil {
		httputil.WriteJSONError(w, http.StatusServiceUnavailable, "replication status not available")
		return
	}
	snap := h.replMetrics.ReplicationSnapshot()
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
