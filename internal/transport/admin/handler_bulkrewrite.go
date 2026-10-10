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
// All four drive the same ops driver over the same ledger and differ only in
// which pass they call and what they name their success count, so they are one
// handler parameterised by that rather than four copies of the plumbing.
//
// Each is synchronous and walks the whole ledger, which is why the web UI drives
// them through its own background-job wrapper rather than from a request the
// browser waits on.
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"fmt"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// bulkRewritePass is any direction of any rewrite, which differ only in which
// listing they walk and what they do to each object. Naming the shape lets one
// handler serve all four rather than each carrying its own copy of the plumbing.
type bulkRewritePass func(context.Context, progress.Observer, int, string) (ops.BulkRewriteResult, error)

// handleBulkRewrite serves one rewrite endpoint. The optional max parameter
// caps how many copies are rewritten (0 or absent means all); a repeated capped
// request resumes where the last stopped, because converted and ratio-declined
// copies leave the selection. The optional backend parameter limits the pass to
// one backend. A skip means the encryptor or codec is not configured, which is
// the caller's to fix, so it is answered as a bad request. body renders the
// endpoint's own response type, since each names its success count differently
// on the wire.
func (h *Handler) handleBulkRewrite(w http.ResponseWriter, r *http.Request, op, verb, listErrMsg string, run bulkRewritePass, body func(ops.BulkRewriteResult) any) {
	maxObjects := httputil.QueryPositiveInt(r.URL.Query().Get(paramMax))
	backend, ok := h.backendParam(w, r)
	if !ok {
		return
	}

	h.servePass(w, r, passEndpoint[ops.BulkRewriteResult]{
		op:          op,
		verb:        verb,
		sequential:  true,
		failMsg:     listErrMsg,
		skipIsError: true,
		run: func(ctx context.Context, obs progress.Observer) (ops.BulkRewriteResult, error) {
			return run(ctx, obs, maxObjects, backend)
		},
		body: func(_ adminapi.Outcome, res ops.BulkRewriteResult) any { return body(res) },
		summary: func(res ops.BulkRewriteResult) (int, string) {
			return res.Succeeded, fmt.Sprintf("rewrote %d, skipped %d, changed %d, failed %d, of %d",
				res.Succeeded, res.Skipped, res.Changed, res.Failed, res.Total)
		},
	})
}

// bulkRewriteOutcome is the part of the response that does not vary between the
// four passes.
func bulkRewriteOutcome(res ops.BulkRewriteResult) adminapi.BulkRewriteOutcome {
	return adminapi.BulkRewriteOutcome{
		Status:  statusComplete,
		Skipped: res.Skipped,
		Failed:  res.Failed,
		Total:   res.Total,
	}
}

// handleCompressExisting encodes every copy currently stored verbatim.
func (h *Handler) handleCompressExisting(w http.ResponseWriter, r *http.Request) {
	h.handleBulkRewrite(w, r, "compress-existing", "compressing", "failed to list uncompressed objects",
		h.compression.CompressExisting, func(res ops.BulkRewriteResult) any {
			return adminapi.CompressExistingResponse{
				BulkRewriteOutcome: bulkRewriteOutcome(res),
				Compressed:         res.Succeeded,
			}
		})
}

// handleDecompressExisting rewrites every encoded copy back to the bytes the
// client wrote.
func (h *Handler) handleDecompressExisting(w http.ResponseWriter, r *http.Request) {
	h.handleBulkRewrite(w, r, "decompress-existing", "decompressing", "failed to list compressed objects",
		h.compression.DecompressExisting, func(res ops.BulkRewriteResult) any {
			return adminapi.DecompressExistingResponse{
				BulkRewriteOutcome: bulkRewriteOutcome(res),
				Decompressed:       res.Succeeded,
			}
		})
}

// handleEncryptExisting rewrites every plaintext copy as ciphertext.
func (h *Handler) handleEncryptExisting(w http.ResponseWriter, r *http.Request) {
	h.handleBulkRewrite(w, r, "encrypt-existing", "encrypting", "failed to list unencrypted objects",
		h.encryption.EncryptExisting, func(res ops.BulkRewriteResult) any {
			return adminapi.EncryptExistingResponse{
				BulkRewriteOutcome: bulkRewriteOutcome(res),
				Encrypted:          res.Succeeded,
			}
		})
}

// handleDecryptExisting rewrites every encrypted copy as plaintext. Encryption
// must still be configured, since the key provider is what unwraps each DEK.
func (h *Handler) handleDecryptExisting(w http.ResponseWriter, r *http.Request) {
	h.handleBulkRewrite(w, r, "decrypt-existing", "decrypting", "failed to list encrypted objects",
		h.encryption.DecryptExisting, func(res ops.BulkRewriteResult) any {
			return adminapi.DecryptExistingResponse{
				BulkRewriteOutcome: bulkRewriteOutcome(res),
				Decrypted:          res.Succeeded,
			}
		})
}
