// -------------------------------------------------------------------------------
// Streaming SigV4 Transport Wiring
//
// Author: Alex Freidah
//
// Bridges the auth-layer chunk reader into the request body and maps
// chunk-validation errors to S3 XML error responses. Handler code reads
// from the wrapped body as if it were a normal request body; integrity
// failures surface as typed errors that streamingErrorResponse converts
// to the spec-correct status code (403 SignatureDoesNotMatch, 400
// InvalidRequest, 400 IncompleteBody).
// -------------------------------------------------------------------------------

package s3api

import (
	"errors"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
)

// applyStreamingBody wraps r.Body in a chunk-validating reader and
// adjusts the request envelope so downstream handlers see the decoded
// body. The wrapped reader is what r.Body returns; reads from it deliver
// only verified payload bytes. The original streaming-only headers are
// stripped so backend clients do not re-emit them.
func applyStreamingBody(r *http.Request, mat *auth.StreamingMaterial) {
	r.Body = auth.NewChunkReader(r.Body, mat)
	r.ContentLength = mat.DecodedLen
	r.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	r.Header.Del("Content-Encoding")
	r.Header.Del("X-Amz-Decoded-Content-Length")
	r.Header.Del("X-Amz-Trailer")
	telemetry.AuthStreamingRequestsTotal.WithLabelValues(mat.Variant.Label()).Inc()
}

// streamingErrorResponse maps an error returned during streaming-body
// consumption to an S3 error response. Returns ok=false when the error
// is not a streaming-validation error so the caller can fall through to
// generic error handling.
func streamingErrorResponse(err error) (status int, code, msg, reason string, ok bool) {
	switch {
	case errors.Is(err, auth.ErrChunkSignatureMismatch):
		return http.StatusForbidden, "SignatureDoesNotMatch",
			"The chunk signature does not match the signature derived from the request.",
			"chunk_signature_mismatch", true
	case errors.Is(err, auth.ErrTrailerSignatureMismatch):
		return http.StatusForbidden, "SignatureDoesNotMatch",
			"The trailer signature does not match the signature derived from the request.",
			"trailer_signature_mismatch", true
	case errors.Is(err, auth.ErrDecodedLengthMismatch):
		return http.StatusBadRequest, "IncompleteBody",
			"The decoded body length does not match x-amz-decoded-content-length.",
			"decoded_length_mismatch", true
	case errors.Is(err, auth.ErrChunkTooLarge):
		return http.StatusBadRequest, "InvalidRequest",
			"A streaming chunk exceeded the maximum permitted size.",
			"chunk_too_large", true
	case errors.Is(err, auth.ErrChunkMalformed):
		return http.StatusBadRequest, "InvalidRequest",
			"Streaming chunk framing is malformed.",
			"chunk_malformed", true
	case errors.Is(err, auth.ErrTrailerMalformed):
		return http.StatusBadRequest, "InvalidRequest",
			"Streaming trailer block is malformed.",
			"trailer_malformed", true
	}
	return 0, "", "", "", false
}
