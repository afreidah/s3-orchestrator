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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// removeConfirmTTL is how long a purge confirmation token is valid.
const (
	removeConfirmTTL        = 60 * time.Second
	errDrainOperationFailed = "drain operation failed"
)

// handleStartDrain begins draining a backend.
func (h *Handler) handleStartDrain(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.drain.StartDrain(r.Context(), name); err != nil {
		h.log.ErrorContext(r.Context(), "drain start failed", slog.String("backend", name), "error", err)
		httputil.WriteJSONError(w, http.StatusBadRequest, errDrainOperationFailed)
		return
	}
	httputil.WriteJSON(w, http.StatusAccepted, adminapi.BackendOperationResponse{
		Status:  "drain started",
		Backend: name,
	})
}

// handleDrainProgress returns the current state of a drain operation.
func (h *Handler) handleDrainProgress(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	progress, err := h.drain.GetDrainProgress(r.Context(), name)
	if err != nil {
		h.log.ErrorContext(r.Context(), "drain progress failed", slog.String("backend", name), "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, errDrainOperationFailed)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, adminapi.DrainProgressResponse{
		Active:           progress.Active,
		ObjectsRemaining: progress.ObjectsRemaining,
		BytesRemaining:   progress.BytesRemaining,
		ObjectsMoved:     progress.ObjectsMoved,
		Error:            progress.Error,
	})
}

// handleCancelDrain cancels an active drain operation.
func (h *Handler) handleCancelDrain(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.drain.CancelDrain(name); err != nil {
		h.log.ErrorContext(r.Context(), "drain cancel failed", slog.String("backend", name), "error", err)
		httputil.WriteJSONError(w, http.StatusBadRequest, errDrainOperationFailed)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, adminapi.BackendOperationResponse{
		Status:  "drain cancelled",
		Backend: name,
	})
}

// handleRemoveBackend deletes all DB records for a backend. When purge=true,
// requires two-phase confirmation: first call returns a preview with a signed
// token, second call with confirm=<token> executes the purge.
// Without purge, executes immediately (DB records only, S3 objects preserved).
func (h *Handler) handleRemoveBackend(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	purge := r.URL.Query().Get("purge") == "true"
	confirmToken := r.URL.Query().Get("confirm")

	// Non-purge removal: drop DB records immediately (reversible via sync)
	if !purge {
		if err := h.drain.RemoveBackend(r.Context(), name, false, nil); err != nil {
			h.log.ErrorContext(r.Context(), "remove backend failed", slog.String("backend", name), "error", err)
			httputil.WriteJSONError(w, http.StatusBadRequest, "remove failed")
			return
		}
		httputil.WriteJSON(w, http.StatusOK, adminapi.BackendOperationResponse{
			Status:  "backend removed",
			Backend: name,
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
		if err := h.drain.RemoveBackend(r.Context(), name, true, nil); err != nil {
			h.log.ErrorContext(r.Context(), "purge backend failed", slog.String("backend", name), "error", err)
			httputil.WriteJSONError(w, http.StatusBadRequest, "purge failed")
			return
		}
		httputil.WriteJSON(w, http.StatusOK, adminapi.BackendOperationResponse{
			Status:  "backend purged",
			Backend: name,
		})
		return
	}

	// Purge phase 1: preview what will be destroyed, return confirmation token
	objectCount, totalBytes, err := h.lifecycle.BackendObjectStats(r.Context(), name)
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

// streamRemovePurge runs a backend purge as an NDJSON step stream, one
// "deleting <key>" line per object plus a terminal summary. Purge deletes
// objects one at a time, so each step renders live (sequential=true): a prefix
// when the object starts, the status when it finishes.
func (h *Handler) streamRemovePurge(w http.ResponseWriter, r *http.Request, name string) {
	h.streamSteps(w, "remove-backend", "deleting", true, func(obs progress.Observer) (stepResult, error) {
		var purged int
		counting := func(s progress.Step) {
			if s.Phase == progress.PhaseEnd && s.Status == progress.StatusOK {
				purged++
			}
			obs(s)
		}
		if err := h.drain.RemoveBackend(r.Context(), name, true, counting); err != nil {
			h.log.ErrorContext(r.Context(), "purge backend failed", slog.String("backend", name), "error", err)
			return stepResult{}, err
		}
		return stepResult{
			Processed: purged,
			Summary:   fmt.Sprintf("purged %d objects from backend %q", purged, name),
			Fields:    map[string]any{"backend": name, "purged": purged},
		}, nil
	})
}

// generateRemoveToken creates an HMAC-signed token encoding the backend name
// and expiry. Uses the admin token as the HMAC key.
func (h *Handler) generateRemoveToken(name string) string {
	expiry := time.Now().Add(removeConfirmTTL).Unix()
	payload := fmt.Sprintf("purge|%s|%d", name, expiry)
	mac := hmac.New(sha256.New, []byte(h.token))
	mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + sig
}

// validRemoveToken verifies a purge confirmation token.
func (h *Handler) validRemoveToken(token, expectedName string) bool {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return false
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, []byte(h.token))
	mac.Write(payloadBytes)
	if !hmac.Equal(mac.Sum(nil), sig) {
		return false
	}

	fields := strings.SplitN(string(payloadBytes), "|", 3)
	if len(fields) != 3 || fields[0] != "purge" || fields[1] != expectedName {
		return false
	}
	expiry, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return false
	}
	return time.Now().Unix() < expiry
}
