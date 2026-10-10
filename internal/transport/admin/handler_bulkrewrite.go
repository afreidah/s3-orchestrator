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

// bulkRewriteEndpoint is one rewrite direction as the transport sees it: the
// pass to run, how to word it, and how to render what it reported. body
// renders the endpoint's own response type, since each names its success
// count differently on the wire.
type bulkRewriteEndpoint struct {
	op         string
	verb       string
	listErrMsg string
	run        bulkRewritePass
	body       func(ops.BulkRewriteResult) any
}

// streamBulkRewrite runs one pass as an NDJSON step stream, reporting each
// object as it is rewritten. Skipped objects are counted apart from failures.
// The pass runs under the request context, so a disconnecting caller stops it.
func (h *Handler) streamBulkRewrite(w http.ResponseWriter, r *http.Request, ep bulkRewriteEndpoint, maxObjects int, backend string) {
	h.streamSteps(w, ep.op, ep.verb, true, func(obs progress.Observer) (stepResult, error) {
		res, err := ep.run(r.Context(), obs, maxObjects, backend)
		if err != nil {
			return stepResult{}, err
		}
		return stepResult{
			Processed: res.Succeeded,
			Summary: fmt.Sprintf("rewrote %d, skipped %d, changed %d, failed %d, of %d",
				res.Succeeded, res.Skipped, res.Changed, res.Failed, res.Total),
			Fields: map[string]any{
				"rewritten": res.Succeeded,
				"skipped":   res.Skipped,
				"changed":   res.Changed,
				"failed":    res.Failed,
				"total":     res.Total,
			},
		}, nil
	})
}

// handleBulkRewrite serves one rewrite endpoint. Streams per-object NDJSON
// progress when the client accepts the stream content type; otherwise returns a
// single JSON result. The optional max parameter caps how many copies are
// rewritten (0 or absent means all); a repeated capped request resumes where
// the last stopped, because converted and ratio-declined copies leave the
// selection. The optional backend parameter limits the pass to one backend.
func (h *Handler) handleBulkRewrite(w http.ResponseWriter, r *http.Request, ep bulkRewriteEndpoint) {
	maxObjects := httputil.QueryPositiveInt(r.URL.Query().Get(paramMax))
	backend, ok := h.backendParam(w, r)
	if !ok {
		return
	}

	if acceptsStream(r) {
		h.streamBulkRewrite(w, r, ep, maxObjects, backend)
		return
	}

	res, err := ep.run(r.Context(), nil, maxObjects, backend)
	if !h.writeBulkRewriteError(w, r, err, ep.listErrMsg) {
		return
	}

	httputil.WriteJSON(w, http.StatusOK, ep.body(res))
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
	h.handleBulkRewrite(w, r, bulkRewriteEndpoint{
		op:         "compress-existing",
		verb:       "compressing",
		listErrMsg: "failed to list uncompressed objects",
		run:        h.compression.CompressExisting,
		body: func(res ops.BulkRewriteResult) any {
			return adminapi.CompressExistingResponse{
				BulkRewriteOutcome: bulkRewriteOutcome(res),
				Compressed:         res.Succeeded,
			}
		},
	})
}

// handleDecompressExisting rewrites every encoded copy back to the bytes the
// client wrote.
func (h *Handler) handleDecompressExisting(w http.ResponseWriter, r *http.Request) {
	h.handleBulkRewrite(w, r, bulkRewriteEndpoint{
		op:         "decompress-existing",
		verb:       "decompressing",
		listErrMsg: "failed to list compressed objects",
		run:        h.compression.DecompressExisting,
		body: func(res ops.BulkRewriteResult) any {
			return adminapi.DecompressExistingResponse{
				BulkRewriteOutcome: bulkRewriteOutcome(res),
				Decompressed:       res.Succeeded,
			}
		},
	})
}

// handleEncryptExisting rewrites every plaintext copy as ciphertext.
func (h *Handler) handleEncryptExisting(w http.ResponseWriter, r *http.Request) {
	h.handleBulkRewrite(w, r, bulkRewriteEndpoint{
		op:         "encrypt-existing",
		verb:       "encrypting",
		listErrMsg: "failed to list unencrypted objects",
		run:        h.encryption.EncryptExisting,
		body: func(res ops.BulkRewriteResult) any {
			return adminapi.EncryptExistingResponse{
				BulkRewriteOutcome: bulkRewriteOutcome(res),
				Encrypted:          res.Succeeded,
			}
		},
	})
}

// handleDecryptExisting rewrites every encrypted copy as plaintext. Encryption
// must still be configured, since the key provider is what unwraps each DEK.
func (h *Handler) handleDecryptExisting(w http.ResponseWriter, r *http.Request) {
	h.handleBulkRewrite(w, r, bulkRewriteEndpoint{
		op:         "decrypt-existing",
		verb:       "decrypting",
		listErrMsg: "failed to list encrypted objects",
		run:        h.encryption.DecryptExisting,
		body: func(res ops.BulkRewriteResult) any {
			return adminapi.DecryptExistingResponse{
				BulkRewriteOutcome: bulkRewriteOutcome(res),
				Decrypted:          res.Succeeded,
			}
		},
	})
}

// writeBulkRewriteError renders whatever went wrong with a bulk rewrite and
// reports whether the caller should go on to write the success body. An
// unavailable encryptor or codec is the caller's problem to fix in config; a
// failed listing is the server's.
func (h *Handler) writeBulkRewriteError(w http.ResponseWriter, r *http.Request, err error, listErrMsg string) bool {
	if err == nil {
		return true
	}
	if reason, skipped := ops.SkipReason(err); skipped {
		httputil.WriteJSONError(w, http.StatusBadRequest, reason)
	} else {
		h.internalError(r.Context(), w, listErrMsg, err)
	}
	return false
}
