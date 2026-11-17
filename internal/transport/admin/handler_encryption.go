// -------------------------------------------------------------------------------
// Admin API - Encryption Key Rotation
//
// Author: Alex Freidah
//
// Re-wraps every DEK under the current primary key as a pass, streaming each
// copy when the caller asks for the stream and answering the counts otherwise.
// The bulk encrypt and decrypt passes live beside the other rewrites in
// handler_bulkrewrite.go, and the work itself in internal/ops.
// -------------------------------------------------------------------------------

package admin

import (
	"net/http"

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
	h.servePass(w, r, RotatePass(h.d.Ops, req.OldKeyID, req.AllStale))
}
