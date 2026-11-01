// -------------------------------------------------------------------------------
// Admin API - Backend Lifecycle (Drain + Remove)
//
// Author: Alex Freidah
//
// Drain start/progress/cancel and the two-phase backend remove flow. The
// purge variant of remove requires an HMAC-signed confirmation token
// (generateRemoveToken/validRemoveToken) so a single curl cannot
// accidentally destroy data: the first call returns a preview + signed
// token, the second call presents the token and executes.
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// removeConfirmTTL is how long a purge confirmation token is valid.
const (
	removeConfirmTTL         = 60 * time.Second
	errDrainOperationFailed  = "drain operation failed"
	errCordonOperationFailed = "cordon operation failed"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// backendChange is one operator change to a backend's state: the manager call
// that makes it, what is logged and returned when it fails, and the status the
// response reports when it succeeds.
type backendChange struct {
	apply    func(ctx context.Context, name string) error
	failLog  string
	failMsg  string
	code     int
	okStatus string
}

// serveBackendChange applies one state change to the backend the path names.
// A refused change, such as cordoning a backend that is already cordoned, is a
// 400 with the generic failure message; the reason is logged.
func (h *Handler) serveBackendChange(w http.ResponseWriter, r *http.Request, c *backendChange) {
	name := r.PathValue(paramName)
	if err := c.apply(r.Context(), name); err != nil {
		h.log.ErrorContext(r.Context(), c.failLog, slog.String("backend", name), "error", err)
		httputil.WriteJSONError(w, http.StatusBadRequest, c.failMsg)
		return
	}
	httputil.WriteJSON(w, c.code, adminapi.BackendOperationResponse{Status: c.okStatus, Backend: name})
}

// handleStartDrain begins draining a backend.
func (h *Handler) handleStartDrain(w http.ResponseWriter, r *http.Request) {
	h.serveBackendChange(w, r, &backendChange{
		apply: h.d.States.StartDrain, failLog: "drain start failed", failMsg: errDrainOperationFailed,
		code: http.StatusAccepted, okStatus: "drain started",
	})
}

// handleCordon stops new copies landing on a backend. It takes effect at once.
func (h *Handler) handleCordon(w http.ResponseWriter, r *http.Request) {
	h.serveBackendChange(w, r, &backendChange{
		apply: h.d.States.Cordon, failLog: "cordon failed", failMsg: errCordonOperationFailed,
		code: http.StatusOK, okStatus: "backend cordoned",
	})
}

// handleUncordon lets new copies land on a backend again. A drain in progress
// carries on.
func (h *Handler) handleUncordon(w http.ResponseWriter, r *http.Request) {
	h.serveBackendChange(w, r, &backendChange{
		apply: h.d.States.Uncordon, failLog: "uncordon failed", failMsg: errCordonOperationFailed,
		code: http.StatusOK, okStatus: "backend uncordoned",
	})
}

// handleDrainProgress returns the current state of a drain operation.
func (h *Handler) handleDrainProgress(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue(paramName)
	progress, err := h.d.States.GetDrainProgress(r.Context(), name)
	if err != nil {
		httputil.InternalError(r.Context(), h.log, w, errDrainOperationFailed, err, slog.String("backend", name))
		return
	}
	httputil.WriteJSON(w, http.StatusOK, adminapi.DrainProgressResponse(*progress))
}

// handleCancelDrain clears a backend's drain flags, stopping a drain in
// progress and making a drained or failed backend writable again unless it is
// cordoned.
func (h *Handler) handleCancelDrain(w http.ResponseWriter, r *http.Request) {
	h.serveBackendChange(w, r, &backendChange{
		apply: h.d.States.CancelDrain, failLog: "drain cancel failed", failMsg: errDrainOperationFailed,
		code: http.StatusOK, okStatus: "drain cancelled",
	})
}

// handleRemoveBackend deletes all DB records for a backend. When purge=true,
// requires two-phase confirmation: first call returns a preview with a signed
// token, second call with confirm=<token> executes the purge.
// Without purge, executes immediately (DB records only, S3 objects preserved).
func (h *Handler) handleRemoveBackend(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue(paramName)
	purge := r.URL.Query().Get("purge") == "true"
	confirmToken := r.URL.Query().Get("confirm")

	// Non-purge removal: drop DB records immediately (reversible via sync)
	if !purge {
		h.serveBackendChange(w, r, &backendChange{
			apply: h.removeBackend(false), failLog: "remove backend failed", failMsg: "remove failed",
			code: http.StatusOK, okStatus: "backend removed",
		})
		return
	}

	// Purge phase 2: validate token and execute
	if confirmToken != "" {
		if !h.validRemoveToken(confirmToken, name) {
			httputil.WriteJSONError(w, http.StatusForbidden, "invalid or expired confirmation token")
			return
		}
		if acceptsStream(r) {
			h.streamRemovePurge(w, r, name)
			return
		}
		h.serveBackendChange(w, r, &backendChange{
			apply: h.removeBackend(true), failLog: "purge backend failed", failMsg: "purge failed",
			code: http.StatusOK, okStatus: "backend purged",
		})
		return
	}

	// Purge phase 1: preview what will be destroyed, return confirmation token
	objectCount, totalBytes, err := h.d.StateStore.BackendObjectStats(r.Context(), name)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, "backend not found or stats unavailable")
		return
	}

	token := h.generateRemoveToken(name)
	httputil.WriteJSON(w, http.StatusOK, adminapi.RemoveBackendPreview{
		Status:       "confirmation required",
		Backend:      name,
		ObjectCount:  objectCount,
		TotalBytes:   totalBytes,
		ConfirmToken: token,
		ExpiresIn:    int(removeConfirmTTL.Seconds()),
	})
}

// removeBackend is the backendChange that drops a backend's records, deleting
// its objects too when purge is set.
func (h *Handler) removeBackend(purge bool) func(context.Context, string) error {
	return func(ctx context.Context, name string) error {
		return h.d.States.RemoveBackend(ctx, name, purge, nil)
	}
}

// streamRemovePurge runs a backend purge as an NDJSON step stream, one
// "deleting <key>" line per object plus a terminal summary. Purge deletes
// objects one at a time, so each step renders live (sequential=true): a prefix
// when the object starts, the status when it finishes.
func (h *Handler) streamRemovePurge(w http.ResponseWriter, r *http.Request, name string) {
	h.streamPass(w, r, passEndpoint[int]{
		op: "remove-backend", verb: "deleting", sequential: true, failMsg: "purge failed",
		run: func(ctx context.Context, obs progress.Observer) (int, error) {
			var purged int
			counting := func(s progress.Step) {
				if s.Phase == progress.PhaseEnd && s.Status == progress.StatusOK {
					purged++
				}
				obs(s)
			}
			if err := h.d.States.RemoveBackend(ctx, name, true, counting); err != nil {
				return purged, fmt.Errorf("purge backend %s: %w", name, err)
			}
			return purged, nil
		},
		body: func(_ adminapi.Outcome, purged int) any {
			return adminapi.BackendPurgeResult{Backend: name, Purged: purged}
		},
		summary: func(purged int) (int, string) {
			return purged, fmt.Sprintf("purged %d objects from backend %q", purged, name)
		},
	})
}

// generateRemoveToken signs a purge confirmation for one backend under the
// handler's per-process confirm key.
func (h *Handler) generateRemoveToken(name string) string {
	return httputil.SignToken(h.confirmKey, "purge|"+name, time.Now().Add(removeConfirmTTL))
}

// validRemoveToken verifies a purge confirmation token was issued for
// expectedName and has not expired.
func (h *Handler) validRemoveToken(token, expectedName string) bool {
	payload, ok := httputil.VerifyToken(h.confirmKey, token, time.Now())
	return ok && payload == "purge|"+expectedName
}
