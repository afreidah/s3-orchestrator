// -------------------------------------------------------------------------------
// UI Admin Actions - Long-Running Operations Triggered from the Dashboard
//
// Author: Alex Freidah
//
// Async wrappers around the operations in internal/ops. Each kicks off the work
// in a background goroutine through asyncOpTracker so the UI can return
// 202 Accepted immediately and poll a /status endpoint for completion.
//
// Every action reports the same four counts and differs only in what it calls
// the one the dashboard keys on. That name is the whole reason each action has
// its own response type: the shape is shared, the noun is published.
// -------------------------------------------------------------------------------

package ui

import (
	"context"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// paramMax is the query parameter capping how many objects a bulk pass rewrites
// in one run, named to match the admin API's own.
const paramMax = "max"

// -------------------------------------------------------------------------
// SHARED SHAPE
// -------------------------------------------------------------------------

// adminActionCounts is what any admin action reports. Count is the figure the
// dashboard keys on; the rest partition what the pass saw. An action with
// nothing to say about a field leaves it zero and its response type omits it.
type adminActionCounts struct {
	Count   int
	Skipped int
	Failed  int
	Total   int
}

// adminActionState is the polling half of a status response, shared by every
// action and by every terminal state. Embedded rather than repeated so the
// dashboard reads one set of names regardless of which action it polled.
type adminActionState struct {
	Status string `json:"status"`
	OK     bool   `json:"ok,omitempty"`
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
}

// adminActionOp describes one one-shot admin action. R is the response type
// this action publishes, which exists only to name its success count. run
// reports a skip as an ops skip error, which the dashboard shows as a
// completed action carrying the reason rather than as a failure.
//
// render is held on the op rather than passed to the status endpoint so the
// trigger and the poll cannot disagree about the shape they report.
type adminActionOp[R any] struct {
	name   string
	run    func(context.Context) (adminActionCounts, error)
	render func(adminActionState, adminActionCounts) R
}

// startAdminAction is the common dispatcher: it requires POST, ensures the op
// isn't already running, then fires the work in a goroutine and returns
// 202 Accepted. R is the action's response type.
func (h *Handler) startAdminAction[R any](w http.ResponseWriter, r *http.Request, op adminActionOp[R]) {
	if !httputil.RequireMethod(w, r, http.MethodPost) {
		return
	}
	if !h.asyncOps.TryStart(op.name) {
		httputil.WriteJSONError(w, http.StatusConflict, op.name+" already running")
		return
	}

	go func() {
		ctx := audit.WithWorkload(context.Background(), audit.WorkloadAdmin)
		counts, err := op.run(ctx)
		reason, skipped := ops.SkipReason(err)
		switch {
		case skipped:
			h.log.InfoContext(ctx, op.name+" skipped", "reason", reason)
			h.asyncOps.Complete(op.name, &asyncResult{Counts: counts, Skipped: reason})
		case err != nil:
			h.log.ErrorContext(ctx, op.name+" failed", "error", err)
			h.asyncOps.Complete(op.name, &asyncResult{Error: op.name + " failed"})
		default:
			h.log.InfoContext(ctx, op.name+" completed", "count", counts.Count)
			h.asyncOps.Complete(op.name, &asyncResult{Counts: counts})
		}
	}()

	httputil.WriteJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

// writeAdminActionStatus serialises the polling response for an admin action.
// The three states that carry no counts render without the action's own type;
// only a finished run reaches render.
func (h *Handler) writeAdminActionStatus[R any](w http.ResponseWriter, op adminActionOp[R]) {
	var body any
	result, running := h.asyncOps.Status(op.name)
	switch {
	case running:
		body = adminActionState{Status: "running"}
	case result == nil:
		body = adminActionState{Status: "idle"}
	case result.Error != "":
		body = adminActionState{Status: "error", Error: result.Error}
	default:
		state := adminActionState{Status: "done", OK: true}
		if result.Skipped != "" {
			state.Status = "skipped"
			state.Reason = result.Skipped
		}
		body = op.render(state, result.Counts)
	}
	httputil.WriteJSON(w, http.StatusOK, body)
}

// -------------------------------------------------------------------------
// REPLICATE
// -------------------------------------------------------------------------

// replicateStatus reports a replication cycle. The dashboard keys on
// copies_created.
type replicateStatus struct {
	adminActionState
	CopiesCreated int `json:"copies_created"`
	Failed        int `json:"failed"`
}

// replicateOp runs one replication cycle.
func (h *Handler) replicateOp(_ *http.Request) adminActionOp[replicateStatus] {
	return adminActionOp[replicateStatus]{
		name: "replicate",
		run: func(ctx context.Context) (adminActionCounts, error) {
			res, err := h.d.Replication.Replicate(ctx, nil)
			if err != nil {
				return adminActionCounts{}, err
			}
			return adminActionCounts{Count: res.CopiesCreated, Failed: res.Failed}, nil
		},
		render: func(s adminActionState, c adminActionCounts) replicateStatus {
			return replicateStatus{adminActionState: s, CopiesCreated: c.Count, Failed: c.Failed}
		},
	}
}

// -------------------------------------------------------------------------
// SCRUB
// -------------------------------------------------------------------------

// scrubStatus reports an integrity scrub pass. The dashboard keys on checked.
type scrubStatus struct {
	adminActionState
	Checked int `json:"checked"`
	Failed  int `json:"failed"`
}

// scrubOp runs one integrity-verification scrub pass.
func (h *Handler) scrubOp(_ *http.Request) adminActionOp[scrubStatus] {
	return adminActionOp[scrubStatus]{
		name: "scrub",
		run: func(ctx context.Context) (adminActionCounts, error) {
			res, err := h.d.Integrity.Scrub(ctx, 0, "", nil)
			if err != nil {
				return adminActionCounts{}, err
			}
			return adminActionCounts{Count: res.Attempted, Failed: res.Failed}, nil
		},
		render: func(s adminActionState, c adminActionCounts) scrubStatus {
			return scrubStatus{adminActionState: s, Checked: c.Count, Failed: c.Failed}
		},
	}
}

// -------------------------------------------------------------------------
// BACKFILL CHECKSUMS
// -------------------------------------------------------------------------

// backfillStatus reports a checksum backfill pass. The dashboard keys on
// processed, and the pass reports nothing else: an object either got a hash or
// the run stopped.
type backfillStatus struct {
	adminActionState
	Processed int `json:"processed"`
}

// backfillOp runs a checksum backfill pass over every object that lacks a
// content hash.
func (h *Handler) backfillOp(_ *http.Request) adminActionOp[backfillStatus] {
	return adminActionOp[backfillStatus]{
		name: "backfill-checksums",
		run: func(ctx context.Context) (adminActionCounts, error) {
			res, err := h.d.Integrity.BackfillChecksums(ctx, 0, 0, 0, "", nil)
			if err != nil {
				return adminActionCounts{}, err
			}
			return adminActionCounts{Count: res.Succeeded}, nil
		},
		render: func(s adminActionState, c adminActionCounts) backfillStatus {
			return backfillStatus{adminActionState: s, Processed: c.Count}
		},
	}
}

// -------------------------------------------------------------------------
// BULK REWRITES
// -------------------------------------------------------------------------

// bulkRewriteTally is the part of a bulk-rewrite status every pass reports
// alike; only the name of the count the dashboard keys on differs.
type bulkRewriteTally struct {
	Skipped int `json:"skipped"`
	Failed  int `json:"failed"`
	Total   int `json:"total"`
}

// bulkRewriteOp builds one bulk-rewrite action. The request's max caps how many
// copies the run rewrites, 0 meaning the whole fleet; it only shapes the run,
// so a poll that carries none renders the same.
func bulkRewriteOp[R any](
	name string,
	r *http.Request,
	pass func(ctx context.Context, maxObjects int) (ops.BulkRewriteResult, error),
	render func(s adminActionState, count int, t bulkRewriteTally) R,
) adminActionOp[R] {
	maxObjects := httputil.QueryPositiveInt(r.URL.Query().Get(paramMax))
	return adminActionOp[R]{
		name: name,
		run: func(ctx context.Context) (adminActionCounts, error) {
			res, err := pass(ctx, maxObjects)
			if err != nil {
				return adminActionCounts{}, err
			}
			return adminActionCounts{Count: res.Succeeded, Skipped: res.Skipped, Failed: res.Failed, Total: res.Total}, nil
		},
		render: func(s adminActionState, c adminActionCounts) R {
			return render(s, c.Count, bulkRewriteTally{Skipped: c.Skipped, Failed: c.Failed, Total: c.Total})
		},
	}
}

// encryptExistingStatus reports an encrypt-existing pass.
type encryptExistingStatus struct {
	adminActionState
	Encrypted int `json:"encrypted"`
	bulkRewriteTally
}

// encryptOp encrypts every unencrypted object.
func (h *Handler) encryptOp(r *http.Request) adminActionOp[encryptExistingStatus] {
	return bulkRewriteOp("encrypt-existing", r,
		func(ctx context.Context, n int) (ops.BulkRewriteResult, error) {
			return h.d.Encryption.EncryptExisting(ctx, nil, n, "")
		},
		func(s adminActionState, count int, t bulkRewriteTally) encryptExistingStatus {
			return encryptExistingStatus{adminActionState: s, Encrypted: count, bulkRewriteTally: t}
		})
}

// compressExistingStatus reports a compress-existing pass.
//
// Skipped is published alongside failed because this pass declines objects on
// purpose - too incompressible to be worth encoding - and a run that skipped
// most of a fleet is a healthy one.
type compressExistingStatus struct {
	adminActionState
	Compressed int `json:"compressed"`
	bulkRewriteTally
}

// compressOp compresses every object stored verbatim.
func (h *Handler) compressOp(r *http.Request) adminActionOp[compressExistingStatus] {
	return bulkRewriteOp("compress-existing", r,
		func(ctx context.Context, n int) (ops.BulkRewriteResult, error) {
			return h.d.Compression.CompressExisting(ctx, nil, n, "")
		},
		func(s adminActionState, count int, t bulkRewriteTally) compressExistingStatus {
			return compressExistingStatus{adminActionState: s, Compressed: count, bulkRewriteTally: t}
		})
}

// decompressExistingStatus reports a decompress-existing pass.
type decompressExistingStatus struct {
	adminActionState
	Decompressed int `json:"decompressed"`
	bulkRewriteTally
}

// decompressOp rewrites every encoded object back to the bytes the client
// wrote, which is how an operator takes the feature back out.
func (h *Handler) decompressOp(r *http.Request) adminActionOp[decompressExistingStatus] {
	return bulkRewriteOp("decompress-existing", r,
		func(ctx context.Context, n int) (ops.BulkRewriteResult, error) {
			return h.d.Compression.DecompressExisting(ctx, nil, n, "")
		},
		func(s adminActionState, count int, t bulkRewriteTally) decompressExistingStatus {
			return decompressExistingStatus{adminActionState: s, Decompressed: count, bulkRewriteTally: t}
		})
}

// -------------------------------------------------------------------------
// LIFECYCLE
// -------------------------------------------------------------------------

// lifecycleStatus reports an expiration sweep. The dashboard keys on deleted.
type lifecycleStatus struct {
	adminActionState
	Deleted int `json:"deleted"`
	Failed  int `json:"failed"`
}

// lifecycleOp runs one expiration sweep, so an operator can confirm a rule they
// just wrote matches something without waiting out the hourly tick.
func (h *Handler) lifecycleOp(_ *http.Request) adminActionOp[lifecycleStatus] {
	return adminActionOp[lifecycleStatus]{
		name: opLifecycle,
		run: func(ctx context.Context) (adminActionCounts, error) {
			res, err := h.d.Expiry.Run(ctx, nil)
			if err != nil {
				return adminActionCounts{}, err
			}
			return adminActionCounts{Count: res.Succeeded, Failed: res.Failed}, nil
		},
		render: func(s adminActionState, c adminActionCounts) lifecycleStatus {
			return lifecycleStatus{adminActionState: s, Deleted: c.Count, Failed: c.Failed}
		},
	}
}
