// -------------------------------------------------------------------------------
// Admin API - Replication and Over-Replication Control
//
// Author: Alex Freidah
//
// /admin/api/replicate triggers a one-shot replication pass to fill in
// under-replicated objects, and /admin/api/replication/rebuild rebuilds the
// queue those passes drain from the ledger. The over-replication endpoints
// expose count + cleanup so operators can drive excess-copy removal from
// outside the scheduled cleaner. Each handler renders what the matching
// operation in internal/ops reports.
// -------------------------------------------------------------------------------

package admin

import (
	"errors"
	"net/http"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// -------------------------------------------------------------------------
// REPLICATION
// -------------------------------------------------------------------------

// handleReplicate triggers one replication cycle.
func (h *Handler) handleReplicate(w http.ResponseWriter, r *http.Request) {
	h.servePass(w, r, ReplicatePass(h.d.Ops))
}

// handleReplicationRebuild rebuilds the replication queue from the ledger and
// reports how many keys it queued, or why it was skipped.
func (h *Handler) handleReplicationRebuild(w http.ResponseWriter, r *http.Request) {
	res, err := h.d.Ops.Replication.RebuildQueue(r.Context())
	if reason, skipped := ops.SkipReason(err); skipped {
		httputil.WriteJSON(w, http.StatusOK, adminapi.ReplicationRebuildResponse{
			Outcome: adminapi.Outcome{Status: adminstream.OutcomeSkipped, Reason: reason},
		})
		return
	}
	if err != nil {
		httputil.InternalError(r.Context(), h.log, w, "failed to rebuild the replication queue", err)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, adminapi.ReplicationRebuildResponse{
		Outcome:            adminapi.Outcome{Status: adminstream.OutcomeOK},
		ReplicationFactors: wireFactors(&res.Factors),
		Queued:             res.Queued,
	})
}

// -------------------------------------------------------------------------
// OVER-REPLICATION
// -------------------------------------------------------------------------

// handleOverReplicationStatus returns the count of over-replicated objects.
func (h *Handler) handleOverReplicationStatus(w http.ResponseWriter, r *http.Request) {
	res, err := h.d.Ops.Replication.CountBacklog(r.Context())
	if reason, skipped := ops.SkipReason(err); skipped {
		httputil.WriteJSON(w, http.StatusOK, adminapi.OverReplicationStatusResponse{
			Outcome: adminapi.Outcome{Status: adminstream.OutcomeSkipped, Reason: reason},
		})
		return
	}
	if err != nil {
		httputil.InternalError(r.Context(), h.log, w, "failed to count over-replicated objects", err)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.OverReplicationStatusResponse{
		Outcome:            adminapi.Outcome{Status: adminstream.OutcomeOK},
		ReplicationFactors: wireFactors(&res.Factors),
		Pending:            res.Over,
	})
}

// handleOverReplicationClean triggers an immediate over-replication cleanup
// pass. Accepts an optional batch_size query parameter.
func (h *Handler) handleOverReplicationClean(w http.ResponseWriter, r *http.Request) {
	batchSize, ok := httputil.RequireQueryCount(w, r, adminapi.ParamBatchSize, 0, httputil.NoCeiling)
	if !ok {
		return
	}
	h.servePass(w, r, CleanExcessPass(h.d.Ops, batchSize))
}

// handleReplicationStatus returns the replication backlog, the under- and
// over-replicated counts plus the factors, read from the replication queue so
// every instance gives the same answer. With replication disabled the counts
// are zero.
func (h *Handler) handleReplicationStatus(w http.ResponseWriter, r *http.Request) {
	backlog, err := h.d.Ops.Replication.CountBacklog(r.Context())
	if err != nil && !errors.Is(err, ops.ErrReplicationDisabled) {
		httputil.InternalError(r.Context(), h.log, w, "failed to count the replication backlog", err)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, adminapi.ReplicationStatusResponse{
		ReplicationFactors: wireFactors(&backlog.Factors),
		Replicating:        backlog.Factors.Replicates(),
		UnderReplicated:    backlog.Under,
		OverReplicated:     backlog.Over,
		ComputedAt:         time.Now(),
	})
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// wireFactors renders the running factors as the replication responses carry
// them.
func wireFactors(f *core.ReplicationFactors) adminapi.ReplicationFactors {
	return adminapi.ReplicationFactors{Factor: f.Default, BucketFactors: f.Buckets}
}
