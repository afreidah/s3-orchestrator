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
type bulkRewritePass func(context.Context, progress.Observer, int) (ops.BulkRewriteResult, error)

// bulkRewriteEndpoint is one rewrite direction as the transport sees it: the
// pass to run, how to word it, and how to render what it reported.
//
// body exists because the four responses name their success count differently
// on the wire, which is the only way they differ. Those names are already
// published, so each endpoint keeps its own published type and the shared
// outcome carries everything else.
type bulkRewriteEndpoint struct {
	op         string
	verb       string
	listErrMsg string
	run        bulkRewritePass
	body       func(ops.BulkRewriteResult) any
}

// streamBulkRewrite runs one pass as an NDJSON step stream, reporting each
// object as it is rewritten.
//
// These passes read and rewrite every object in a fleet, so they are the
// longest-running thing the admin API offers. A caller watching one needs to
// see it move: a single JSON summary at the end is indistinguishable from a
// hung request until it arrives.
//
// The summary names skipped objects separately, because a pass over media
// declines almost everything and a count that folded those into failures would
// read as a broken run.
//
// The pass runs under the request context, so a caller that disconnects stops
// the work rather than leaving a fleet-wide rewrite running unwatched. That
// matches every other streaming pass; the web UI wraps these in its own
// background job when it wants them to outlive the request.
func (h *Handler) streamBulkRewrite(w http.ResponseWriter, r *http.Request, ep bulkRewriteEndpoint, maxObjects int) {
	h.streamSteps(w, ep.op, ep.verb, true, func(obs progress.Observer) (stepResult, error) {
		res, err := ep.run(r.Context(), obs, maxObjects)
		if reason, skipped := skipReason(err); skipped {
			return stepResult{Skipped: reason}, nil
		}
		if err != nil {
			return stepResult{}, err
		}
		return stepResult{
			Processed: res.Succeeded,
			Summary: fmt.Sprintf("rewrote %d, skipped %d, failed %d, of %d",
				res.Succeeded, res.Skipped, res.Failed, res.Total),
			Fields: map[string]any{
				"rewritten": res.Succeeded,
				"skipped":   res.Skipped,
				"failed":    res.Failed,
				"total":     res.Total,
			},
		}, nil
	})
}

// handleBulkRewrite serves one rewrite endpoint. Streams per-object NDJSON
// progress when the client accepts the stream content type; otherwise returns a
// single JSON result.
//
// The optional max query parameter caps how many copies this request rewrites,
// 0 or absent meaning the whole fleet. A capped request needs nothing carried
// back for the next one: the copies it converts leave the listing that selected
// them, and the ones a compression pass declines on ratio are recorded so they
// leave it too.
func (h *Handler) handleBulkRewrite(w http.ResponseWriter, r *http.Request, ep bulkRewriteEndpoint) {
	maxObjects := httputil.QueryPositiveInt(r.URL.Query().Get(paramMax))

	if acceptsStream(r) {
		h.streamBulkRewrite(w, r, ep, maxObjects)
		return
	}

	res, err := ep.run(r.Context(), nil, maxObjects)
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
