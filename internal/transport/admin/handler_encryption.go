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
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// handleRotateEncryptionKey re-wraps encrypted objects' DEKs with the current
// primary key: those under one old key ID, or every one under another ID. The
// old keys must stay configured for unwrapping.
func (h *Handler) handleRotateEncryptionKey(w http.ResponseWriter, r *http.Request) {
	var req adminapi.RotateEncryptionKeyRequest
	if !httputil.DecodeJSONBody(w, r, &req, httputil.MaxJSONBody) {
		return
	}

	res, err := h.d.Ops.Encryption.RotateKey(r.Context(), req.OldKeyID, req.AllStale)
	if reason, skipped := ops.SkipReason(err); skipped {
		httputil.WriteJSONError(w, http.StatusBadRequest, reason)
		return
	}
	if err != nil {
		httputil.WriteOpError(w, r, h.log, err, "failed to list encrypted objects")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.RotateEncryptionKeyResponse{
		Outcome: adminapi.Outcome{Status: adminapi.StatusOK},
		Failed:  res.Failed,
		Total:   res.Attempted + res.Skipped,
		Rotated: res.Succeeded,
	})
}
