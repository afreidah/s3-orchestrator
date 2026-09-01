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
	"encoding/json"
	"errors"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// paramMax is the query parameter capping how many objects a bulk pass rewrites
// in one run, named to match the admin API's own.
const paramMax = "max"

// skipReason reports the reason an operation declined to run, and whether it
// declined at all. The dashboard surfaces a skip as a completed action
// carrying the reason rather than as a failure.
func skipReason(err error) (string, bool) {
	if skip, ok := errors.AsType[*ops.SkipError](err); ok {
		return skip.Reason, true
	}
	return "", false
}

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
// this action publishes, which exists only to name its success count.
//
// render is held on the op rather than passed to the status endpoint so the
// trigger and the poll cannot disagree about the shape they report.
type adminActionOp[R any] struct {
	name   string
	run    func(context.Context) (adminActionCounts, string, error)
	render func(adminActionState, adminActionCounts) R
}

// startAdminAction is the common dispatcher: it requires POST, ensures the op
// isn't already running, then fires the work in a goroutine and returns
// 202 Accepted.
//
// A free function rather than a method because Go methods cannot take type
// parameters, and the response type is what makes each action's payload typed.
func startAdminAction[R any](h *Handler, w http.ResponseWriter, r *http.Request, op adminActionOp[R]) {
	setSecurityHeaders(w)
	if !httputil.RequireMethod(w, r, http.MethodPost) {
		return
	}
	if !h.asyncOps.TryStart(op.name) {
		w.Header().Set(headerContentType, contentTypeJSON)
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": op.name + " already running"})
		return
	}

	go func() {
		ctx := context.Background()
		counts, skipped, err := op.run(ctx)
		switch {
		case err != nil:
			h.log.ErrorContext(ctx, op.name+" failed", "error", err)
			h.asyncOps.Complete(op.name, &asyncResult{Error: op.name + " failed"})
		case skipped != "":
			h.log.InfoContext(ctx, op.name+" skipped", "reason", skipped)
			h.asyncOps.Complete(op.name, &asyncResult{OK: true, Counts: counts, Skipped: skipped})
		default:
			h.log.InfoContext(ctx, op.name+" completed", "count", counts.Count)
			h.asyncOps.Complete(op.name, &asyncResult{OK: true, Counts: counts})
		}
	}()

	w.Header().Set(headerContentType, contentTypeJSON)
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "started"})
}

// writeAdminActionStatus serialises the polling response for an admin action.
// The three states that carry no counts render without the action's own type;
// only a finished run reaches render.
func writeAdminActionStatus[R any](h *Handler, w http.ResponseWriter, op adminActionOp[R]) {
	setSecurityHeaders(w)
	w.Header().Set(headerContentType, contentTypeJSON)
	enc := json.NewEncoder(w)

	result, running := h.asyncOps.Status(op.name)
	switch {
	case running:
		_ = enc.Encode(adminActionState{Status: "running"})
	case result == nil:
		_ = enc.Encode(adminActionState{Status: "idle"})
	case result.Error != "":
		_ = enc.Encode(adminActionState{Status: "error", Error: result.Error})
	default:
		state := adminActionState{Status: "done", OK: true}
		if result.Skipped != "" {
			state.Status = "skipped"
			state.Reason = result.Skipped
		}
		_ = enc.Encode(op.render(state, result.Counts))
	}
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

// replicateOp is the replicate action, shared by its trigger and its poll.
func (h *Handler) replicateOp() adminActionOp[replicateStatus] {
	return adminActionOp[replicateStatus]{
		name: "replicate",
		run: func(ctx context.Context) (adminActionCounts, string, error) {
			res, err := h.replication.Replicate(ctx, nil)
			if reason, skipped := skipReason(err); skipped {
				return adminActionCounts{}, reason, nil
			}
			if err != nil {
				return adminActionCounts{}, "", err
			}
			return adminActionCounts{Count: res.CopiesCreated, Failed: res.Failed}, "", nil
		},
		render: func(s adminActionState, c adminActionCounts) replicateStatus {
			return replicateStatus{adminActionState: s, CopiesCreated: c.Count, Failed: c.Failed}
		},
	}
}

// handleAPIReplicate triggers one replication cycle in the background.
func (h *Handler) handleAPIReplicate(w http.ResponseWriter, r *http.Request) {
	startAdminAction(h, w, r, h.replicateOp())
}

// handleAPIReplicateStatus returns the latest progress payload for the
// replicate admin action so the dashboard can poll without re-issuing the
// trigger.
func (h *Handler) handleAPIReplicateStatus(w http.ResponseWriter, _ *http.Request) {
	writeAdminActionStatus(h, w, h.replicateOp())
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

// scrubOp is the scrub action, shared by its trigger and its poll.
func (h *Handler) scrubOp() adminActionOp[scrubStatus] {
	return adminActionOp[scrubStatus]{
		name: "scrub",
		run: func(ctx context.Context) (adminActionCounts, string, error) {
			res, err := h.integrity.Scrub(ctx, 0, nil)
			if reason, skipped := skipReason(err); skipped {
				return adminActionCounts{}, reason, nil
			}
			if err != nil {
				return adminActionCounts{}, "", err
			}
			return adminActionCounts{Count: res.Checked, Failed: res.Failed}, "", nil
		},
		render: func(s adminActionState, c adminActionCounts) scrubStatus {
			return scrubStatus{adminActionState: s, Checked: c.Count, Failed: c.Failed}
		},
	}
}

// handleAPIScrub triggers one integrity-verification scrub pass.
func (h *Handler) handleAPIScrub(w http.ResponseWriter, r *http.Request) {
	startAdminAction(h, w, r, h.scrubOp())
}

// handleAPIScrubStatus returns the latest progress payload for the scrub admin
// action so the dashboard can poll without re-triggering.
func (h *Handler) handleAPIScrubStatus(w http.ResponseWriter, _ *http.Request) {
	writeAdminActionStatus(h, w, h.scrubOp())
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

// backfillOp is the backfill-checksums action, shared by its trigger and poll.
func (h *Handler) backfillOp() adminActionOp[backfillStatus] {
	return adminActionOp[backfillStatus]{
		name: "backfill-checksums",
		run: func(ctx context.Context) (adminActionCounts, string, error) {
			res, err := h.integrity.BackfillChecksums(ctx, 0, 0, 0, nil)
			if reason, skipped := skipReason(err); skipped {
				return adminActionCounts{}, reason, nil
			}
			if err != nil {
				return adminActionCounts{}, "", err
			}
			return adminActionCounts{Count: res.Processed}, "", nil
		},
		render: func(s adminActionState, c adminActionCounts) backfillStatus {
			return backfillStatus{adminActionState: s, Processed: c.Count}
		},
	}
}

// handleAPIBackfillChecksums triggers a checksum backfill pass over every
// object that lacks a content hash.
func (h *Handler) handleAPIBackfillChecksums(w http.ResponseWriter, r *http.Request) {
	startAdminAction(h, w, r, h.backfillOp())
}

// handleAPIBackfillChecksumsStatus returns the latest progress payload for the
// backfill-checksums admin action.
func (h *Handler) handleAPIBackfillChecksumsStatus(w http.ResponseWriter, _ *http.Request) {
	writeAdminActionStatus(h, w, h.backfillOp())
}

// -------------------------------------------------------------------------
// BULK REWRITES
// -------------------------------------------------------------------------

// bulkRewriteCounts folds one rewrite pass into the shared counts. All four
// passes report identically, which is the point of them sharing a driver.
func bulkRewriteCounts(res ops.BulkRewriteResult, err error) (adminActionCounts, string, error) {
	if reason, skipped := skipReason(err); skipped {
		return adminActionCounts{}, reason, nil
	}
	if err != nil {
		return adminActionCounts{}, "", err
	}
	return adminActionCounts{
		Count:   res.Succeeded,
		Skipped: res.Skipped,
		Failed:  res.Failed,
		Total:   res.Total,
	}, "", nil
}

// encryptExistingStatus reports an encrypt-existing pass.
type encryptExistingStatus struct {
	adminActionState
	Encrypted int `json:"encrypted"`
	Skipped   int `json:"skipped"`
	Failed    int `json:"failed"`
	Total     int `json:"total"`
}

// encryptOp is the encrypt-existing action. maxObjects caps how many copies the
// run rewrites, 0 meaning the whole fleet.
func (h *Handler) encryptOp(maxObjects int) adminActionOp[encryptExistingStatus] {
	return adminActionOp[encryptExistingStatus]{
		name: "encrypt-existing",
		run: func(ctx context.Context) (adminActionCounts, string, error) {
			return bulkRewriteCounts(h.encryption.EncryptExisting(ctx, nil, maxObjects))
		},
		render: func(s adminActionState, c adminActionCounts) encryptExistingStatus {
			return encryptExistingStatus{
				adminActionState: s, Encrypted: c.Count,
				Skipped: c.Skipped, Failed: c.Failed, Total: c.Total,
			}
		},
	}
}

// handleAPIEncryptExisting walks every unencrypted object, encrypts it,
// re-uploads the ciphertext, and updates the DB record. Long-running.
//
// The optional max query parameter caps how many objects this run rewrites, 0
// or absent meaning the whole fleet.
func (h *Handler) handleAPIEncryptExisting(w http.ResponseWriter, r *http.Request) {
	startAdminAction(h, w, r, h.encryptOp(httputil.QueryPositiveInt(r.URL.Query().Get(paramMax))))
}

// handleAPIEncryptExistingStatus returns the latest progress payload for the
// encrypt-existing admin action. The cap only shapes the run, so polling passes
// none.
func (h *Handler) handleAPIEncryptExistingStatus(w http.ResponseWriter, _ *http.Request) {
	writeAdminActionStatus(h, w, h.encryptOp(0))
}

// compressExistingStatus reports a compress-existing pass.
//
// Skipped is published alongside failed because this pass declines objects on
// purpose - too incompressible to be worth encoding - and a run that skipped
// most of a fleet is a healthy one.
type compressExistingStatus struct {
	adminActionState
	Compressed int `json:"compressed"`
	Skipped    int `json:"skipped"`
	Failed     int `json:"failed"`
	Total      int `json:"total"`
}

// compressOp is the compress-existing action.
func (h *Handler) compressOp(maxObjects int) adminActionOp[compressExistingStatus] {
	return adminActionOp[compressExistingStatus]{
		name: "compress-existing",
		run: func(ctx context.Context) (adminActionCounts, string, error) {
			return bulkRewriteCounts(h.compression.CompressExisting(ctx, nil, maxObjects))
		},
		render: func(s adminActionState, c adminActionCounts) compressExistingStatus {
			return compressExistingStatus{
				adminActionState: s, Compressed: c.Count,
				Skipped: c.Skipped, Failed: c.Failed, Total: c.Total,
			}
		},
	}
}

// handleAPICompressExisting walks every object stored verbatim, encodes it,
// re-uploads the encoding, and updates the DB record. Long-running.
//
// The optional max query parameter caps how many objects this run rewrites, 0
// or absent meaning the whole fleet.
func (h *Handler) handleAPICompressExisting(w http.ResponseWriter, r *http.Request) {
	startAdminAction(h, w, r, h.compressOp(httputil.QueryPositiveInt(r.URL.Query().Get(paramMax))))
}

// handleAPICompressExistingStatus returns the latest progress payload for the
// compress-existing admin action.
func (h *Handler) handleAPICompressExistingStatus(w http.ResponseWriter, _ *http.Request) {
	writeAdminActionStatus(h, w, h.compressOp(0))
}

// decompressExistingStatus reports a decompress-existing pass.
type decompressExistingStatus struct {
	adminActionState
	Decompressed int `json:"decompressed"`
	Skipped      int `json:"skipped"`
	Failed       int `json:"failed"`
	Total        int `json:"total"`
}

// decompressOp is the decompress-existing action.
func (h *Handler) decompressOp(maxObjects int) adminActionOp[decompressExistingStatus] {
	return adminActionOp[decompressExistingStatus]{
		name: "decompress-existing",
		run: func(ctx context.Context) (adminActionCounts, string, error) {
			return bulkRewriteCounts(h.compression.DecompressExisting(ctx, nil, maxObjects))
		},
		render: func(s adminActionState, c adminActionCounts) decompressExistingStatus {
			return decompressExistingStatus{
				adminActionState: s, Decompressed: c.Count,
				Skipped: c.Skipped, Failed: c.Failed, Total: c.Total,
			}
		},
	}
}

// handleAPIDecompressExisting rewrites every encoded object back to the bytes
// the client wrote, which is how an operator takes the feature back out. Takes
// the same optional max as the forward pass.
func (h *Handler) handleAPIDecompressExisting(w http.ResponseWriter, r *http.Request) {
	startAdminAction(h, w, r, h.decompressOp(httputil.QueryPositiveInt(r.URL.Query().Get(paramMax))))
}

// handleAPIDecompressExistingStatus returns the latest progress payload for the
// decompress-existing admin action.
func (h *Handler) handleAPIDecompressExistingStatus(w http.ResponseWriter, _ *http.Request) {
	writeAdminActionStatus(h, w, h.decompressOp(0))
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

// lifecycleOp is the lifecycle action, shared by its trigger and its poll.
func (h *Handler) lifecycleOp() adminActionOp[lifecycleStatus] {
	return adminActionOp[lifecycleStatus]{
		name: opLifecycle,
		run: func(ctx context.Context) (adminActionCounts, string, error) {
			res, err := h.expiry.Run(ctx)
			if reason, skipped := skipReason(err); skipped {
				return adminActionCounts{}, reason, nil
			}
			if err != nil {
				return adminActionCounts{}, "", err
			}
			return adminActionCounts{Count: res.Deleted, Failed: res.Failed}, "", nil
		},
		render: func(s adminActionState, c adminActionCounts) lifecycleStatus {
			return lifecycleStatus{adminActionState: s, Deleted: c.Count, Failed: c.Failed}
		},
	}
}

// handleAPILifecycle runs one expiration sweep in the background, so an
// operator can confirm a rule they just wrote matches something without
// waiting out the hourly tick.
func (h *Handler) handleAPILifecycle(w http.ResponseWriter, r *http.Request) {
	startAdminAction(h, w, r, h.lifecycleOp())
}

// handleAPILifecycleStatus returns the latest progress payload for the
// lifecycle admin action.
func (h *Handler) handleAPILifecycleStatus(w http.ResponseWriter, _ *http.Request) {
	writeAdminActionStatus(h, w, h.lifecycleOp())
}
