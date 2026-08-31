// -------------------------------------------------------------------------------
// Admin API - Encryption Key Rotation and Bulk Encrypt/Decrypt
//
// Author: Alex Freidah
//
// Three fleet-wide encryption endpoints: re-wrap every DEK under the current
// primary key, encrypt everything still stored as plaintext, and reverse that.
// Each handler parses the request and renders the counts the matching
// operation reports; the passes themselves live in internal/ops.
// -------------------------------------------------------------------------------

package admin

import (
	"errors"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// handleRotateEncryptionKey re-wraps all encrypted objects' DEKs with the
// current primary key. The old key must remain in previous_keys for
// unwrapping.
func (h *Handler) handleRotateEncryptionKey(w http.ResponseWriter, r *http.Request) {
	var req adminapi.RotateEncryptionKeyRequest
	if !httputil.DecodeJSONBody(w, r, &req, 1<<20) {
		return
	}

	res, err := h.encryption.RotateKey(r.Context(), req.OldKeyID)
	if errors.Is(err, ops.ErrKeyIDRequired) {
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if reason, skipped := skipReason(err); skipped {
		httputil.WriteJSONError(w, http.StatusBadRequest, reason)
		return
	}
	if err != nil {
		h.internalError(r.Context(), w, "failed to list encrypted objects", err)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.RotateEncryptionKeyResponse{
		Status:  statusComplete,
		Failed:  res.Failed,
		Total:   res.Total,
		Rotated: res.Rotated,
	})
}
