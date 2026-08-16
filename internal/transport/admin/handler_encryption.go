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
		BulkEncryptionOutcome: adminapi.BulkEncryptionOutcome{
			Status: statusComplete,
			Failed: res.Failed,
			Total:  res.Total,
		},
		Rotated: res.Rotated,
	})
}

// handleEncryptExisting rewrites every plaintext copy as ciphertext.
func (h *Handler) handleEncryptExisting(w http.ResponseWriter, r *http.Request) {
	res, err := h.encryption.EncryptExisting(r.Context())
	if !h.writeBulkRewriteError(w, r, err, "failed to list unencrypted objects") {
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.EncryptExistingResponse{
		BulkEncryptionOutcome: adminapi.BulkEncryptionOutcome{
			Status: statusComplete,
			Failed: res.Failed,
			Total:  res.Total,
		},
		Encrypted: res.Succeeded,
	})
}

// handleDecryptExisting rewrites every encrypted copy as plaintext. Encryption
// must still be configured, since the key provider is what unwraps each DEK.
func (h *Handler) handleDecryptExisting(w http.ResponseWriter, r *http.Request) {
	res, err := h.encryption.DecryptExisting(r.Context())
	if !h.writeBulkRewriteError(w, r, err, "failed to list encrypted objects") {
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.DecryptExistingResponse{
		BulkEncryptionOutcome: adminapi.BulkEncryptionOutcome{
			Status: statusComplete,
			Failed: res.Failed,
			Total:  res.Total,
		},
		Decrypted: res.Succeeded,
	})
}

// writeBulkRewriteError renders whatever went wrong with a bulk rewrite and
// reports whether the caller should go on to write the success body. An
// unavailable encryptor is the caller's problem to fix in config; a failed
// listing is the server's.
func (h *Handler) writeBulkRewriteError(w http.ResponseWriter, r *http.Request, err error, listErrMsg string) bool {
	switch {
	case err == nil:
		return true
	case isSkip(err):
		reason, _ := skipReason(err)
		httputil.WriteJSONError(w, http.StatusBadRequest, reason)
	default:
		h.internalError(r.Context(), w, listErrMsg, err)
	}
	return false
}

// isSkip reports whether err is an operation declining to run.
func isSkip(err error) bool {
	_, skipped := skipReason(err)
	return skipped
}
