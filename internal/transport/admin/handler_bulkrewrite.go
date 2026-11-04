// -------------------------------------------------------------------------------
// Admin API - Bulk Rewrite Handlers
//
// Author: Alex Freidah
//
// The four fleet-wide rewrite endpoints: compress, decompress, encrypt and
// decrypt every copy the matching listing selects. Enabling either feature only
// affects objects written afterwards, so these are how an operator brings a
// fleet that already holds data under one, and how they take it back out.
//
// All four are RewritePass with a different Rewrite, so they share one handler.
// Each walks the whole ledger, which is why the dashboard runs them in the
// background rather than from a request the browser waits on.
// -------------------------------------------------------------------------------

package admin

import (
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// handleBulkRewrite serves one rewrite endpoint. The optional max parameter
// caps how many copies are rewritten (0 or absent means all); a repeated capped
// request resumes where the last stopped, because converted and ratio-declined
// copies leave the selection. The optional backend parameter limits the pass to
// one backend.
func (h *Handler) handleBulkRewrite(w http.ResponseWriter, r *http.Request, kind Rewrite) {
	maxObjects, ok := httputil.RequireQueryCount(w, r, adminapi.ParamMax, 0, httputil.NoCeiling)
	if !ok {
		return
	}
	backend, ok := h.backendParam(w, r)
	if !ok {
		return
	}
	h.servePass(w, r, RewritePass(h.d.Ops, kind, maxObjects, backend))
}

// handleCompressExisting encodes every copy currently stored verbatim.
func (h *Handler) handleCompressExisting(w http.ResponseWriter, r *http.Request) {
	h.handleBulkRewrite(w, r, CompressExisting)
}

// handleDecompressExisting rewrites every encoded copy back to the bytes the
// client wrote.
func (h *Handler) handleDecompressExisting(w http.ResponseWriter, r *http.Request) {
	h.handleBulkRewrite(w, r, DecompressExisting)
}

// handleEncryptExisting rewrites every plaintext copy as ciphertext.
func (h *Handler) handleEncryptExisting(w http.ResponseWriter, r *http.Request) {
	h.handleBulkRewrite(w, r, EncryptExisting)
}

// handleDecryptExisting rewrites every encrypted copy as plaintext. Encryption
// must still be configured, since the key provider is what unwraps each DEK.
func (h *Handler) handleDecryptExisting(w http.ResponseWriter, r *http.Request) {
	h.handleBulkRewrite(w, r, DecryptExisting)
}
