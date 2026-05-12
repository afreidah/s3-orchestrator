// -------------------------------------------------------------------------------
// UI Admin Actions - Long-Running Operations Triggered from the Dashboard
//
// Author: Alex Freidah
//
// Async wrappers around the admin handler's exported operations (Replicate,
// Scrub, BackfillChecksums, EncryptExisting). Each kicks off the work in a
// background goroutine through asyncOpTracker so the UI can return
// 202 Accepted immediately and poll a /status endpoint for completion.
// -------------------------------------------------------------------------------

package ui

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// -------------------------------------------------------------------------
// SHARED HELPERS
// -------------------------------------------------------------------------

// adminActionOp describes the shape of a one-shot admin action. resultKey is
// the JSON key the status endpoint uses to surface the per-op count
// (e.g. "copies_created", "checked", "processed").
type adminActionOp struct {
	name      string
	resultKey string
	run       func(context.Context) (count int, extra map[string]any, skippedReason string, err error)
}

// startAdminAction is the common dispatcher: it requires POST, an admin
// handler, ensures the op isn't already running, then fires the work in a
// goroutine and returns 202 Accepted.
func (h *Handler) startAdminAction(w http.ResponseWriter, r *http.Request, op adminActionOp) {
	setSecurityHeaders(w)
	if !httputil.RequireMethod(w, r, http.MethodPost) {
		return
	}
	if h.adminHandler == nil {
		httputil.WriteJSONError(w, http.StatusServiceUnavailable, "admin actions not configured")
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
		count, extra, skipped, err := op.run(ctx)
		switch {
		case err != nil:
			h.log.ErrorContext(ctx, op.name+" failed", "error", err)
			h.asyncOps.Complete(op.name, &asyncResult{Error: op.name + " failed"})
		case skipped != "":
			h.log.InfoContext(ctx, op.name+" skipped", "reason", skipped)
			res := &asyncResult{OK: true, Count: count}
			if extra != nil {
				res.Extra = extra
			}
			res.Skipped = skipped
			h.asyncOps.Complete(op.name, res)
		default:
			h.log.InfoContext(ctx, op.name+" completed", "count", count)
			res := &asyncResult{OK: true, Count: count}
			if extra != nil {
				res.Extra = extra
			}
			h.asyncOps.Complete(op.name, res)
		}
	}()

	w.Header().Set(headerContentType, contentTypeJSON)
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "started"})
}

// writeAdminActionStatus serialises the polling response for an admin action.
func (h *Handler) writeAdminActionStatus(w http.ResponseWriter, name, resultKey string) {
	setSecurityHeaders(w)
	w.Header().Set(headerContentType, contentTypeJSON)

	result, running := h.asyncOps.Status(name)
	if running {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "running"})
		return
	}
	if result == nil {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "idle"})
		return
	}
	if result.Error != "" {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "error": result.Error})
		return
	}

	payload := map[string]any{"status": "done", "ok": true, resultKey: result.Count}
	if result.Skipped != "" {
		payload["status"] = "skipped"
		payload["reason"] = result.Skipped
	}
	maps.Copy(payload, result.Extra)
	_ = json.NewEncoder(w).Encode(payload)
}

// -------------------------------------------------------------------------
// REPLICATE
// -------------------------------------------------------------------------

// handleAPIReplicate triggers one replication cycle in the background.
func (h *Handler) handleAPIReplicate(w http.ResponseWriter, r *http.Request) {
	h.startAdminAction(w, r, adminActionOp{
		name:      "replicate",
		resultKey: "copies_created",
		run: func(ctx context.Context) (int, map[string]any, string, error) {
			res, err := h.adminHandler.Replicate(ctx)
			if err != nil {
				return 0, nil, "", err
			}
			if res.Status == "skipped" {
				return 0, nil, res.Reason, nil
			}
			return res.CopiesCreated, nil, "", nil
		},
	})
}

// handleAPIReplicateStatus returns the latest progress payload for the
// replicate admin action so the dashboard can poll without re-issuing
// the trigger. The keyed result is "copies_created".
func (h *Handler) handleAPIReplicateStatus(w http.ResponseWriter, _ *http.Request) {
	h.writeAdminActionStatus(w, "replicate", "copies_created")
}

// -------------------------------------------------------------------------
// SCRUB
// -------------------------------------------------------------------------

// handleAPIScrub triggers one integrity-verification scrub pass.
func (h *Handler) handleAPIScrub(w http.ResponseWriter, r *http.Request) {
	h.startAdminAction(w, r, adminActionOp{
		name:      "scrub",
		resultKey: "checked",
		run: func(ctx context.Context) (int, map[string]any, string, error) {
			res := h.adminHandler.Scrub(ctx, 0)
			if res.Status == "skipped" {
				return 0, nil, res.Reason, nil
			}
			return res.Checked, map[string]any{"failed": res.Failed}, "", nil
		},
	})
}

// handleAPIScrubStatus returns the latest progress payload for the
// scrub admin action so the dashboard can poll without re-triggering.
// The keyed result is "checked".
func (h *Handler) handleAPIScrubStatus(w http.ResponseWriter, _ *http.Request) {
	h.writeAdminActionStatus(w, "scrub", "checked")
}

// -------------------------------------------------------------------------
// BACKFILL CHECKSUMS
// -------------------------------------------------------------------------

// handleAPIBackfillChecksums triggers a checksum backfill pass over every
// object that lacks a content hash.
func (h *Handler) handleAPIBackfillChecksums(w http.ResponseWriter, r *http.Request) {
	h.startAdminAction(w, r, adminActionOp{
		name:      "backfill-checksums",
		resultKey: "processed",
		run: func(ctx context.Context) (int, map[string]any, string, error) {
			res := h.adminHandler.BackfillChecksums(ctx, 0)
			if res.Status == "skipped" {
				return 0, nil, res.Reason, nil
			}
			return res.Processed, nil, "", nil
		},
	})
}

// handleAPIBackfillChecksumsStatus returns the latest progress payload
// for the backfill-checksums admin action. The keyed result is
// "processed" (rows touched in the most recent pass).
func (h *Handler) handleAPIBackfillChecksumsStatus(w http.ResponseWriter, _ *http.Request) {
	h.writeAdminActionStatus(w, "backfill-checksums", "processed")
}

// -------------------------------------------------------------------------
// ENCRYPT EXISTING
// -------------------------------------------------------------------------

// handleAPIEncryptExisting walks every unencrypted object, encrypts it,
// re-uploads the ciphertext, and updates the DB record. Long-running.
func (h *Handler) handleAPIEncryptExisting(w http.ResponseWriter, r *http.Request) {
	h.startAdminAction(w, r, adminActionOp{
		name:      "encrypt-existing",
		resultKey: "encrypted",
		run: func(ctx context.Context) (int, map[string]any, string, error) {
			res := h.adminHandler.EncryptExisting(ctx)
			if res.Status == "skipped" {
				return 0, nil, res.Reason, nil
			}
			return res.Success, map[string]any{"failed": res.Failed, "total": res.Total}, "", nil
		},
	})
}

// handleAPIEncryptExistingStatus returns the latest progress payload
// for the encrypt-existing admin action. The keyed result is
// "encrypted" (objects successfully transitioned to ciphertext).
func (h *Handler) handleAPIEncryptExistingStatus(w http.ResponseWriter, _ *http.Request) {
	h.writeAdminActionStatus(w, "encrypt-existing", "encrypted")
}

// Compile-time assertion: admin package import is used (silences unused
// import lints when the file is regenerated).
var _ = (*admin.Handler)(nil)
