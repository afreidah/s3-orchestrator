// -------------------------------------------------------------------------------
// Streaming SigV4 Transport Wiring
//
// Author: Alex Freidah
//
// Bridges the auth-layer chunk reader into the request body and maps
// chunk-validation errors to S3 XML error responses. Handler code reads
// from the wrapped body as if it were a normal request body; integrity
// failures surface as typed errors that streamingFaults maps to the
// spec-correct status code (403 SignatureDoesNotMatch, 400 InvalidRequest,
// 400 IncompleteBody).
// -------------------------------------------------------------------------------

package s3api

import (
	"errors"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
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

// streamingFault is one body-validation failure: the auth error that names
// it, the S3 error it answers with, and the reason the rejection metric counts
// it under.
type streamingFault struct {
	err    error
	s3     *core.S3Error
	reason string
}

// streamingFaults maps every body-validation failure onto its S3 error. A
// tampered chunk answers 403 SignatureDoesNotMatch rather than InternalError.
var streamingFaults = []streamingFault{
	{auth.ErrChunkSignatureMismatch, s3Err(http.StatusForbidden, "SignatureDoesNotMatch",
		"The chunk signature does not match the signature derived from the request."), "chunk_signature_mismatch"},
	{auth.ErrTrailerSignatureMismatch, s3Err(http.StatusForbidden, "SignatureDoesNotMatch",
		"The trailer signature does not match the signature derived from the request."), "trailer_signature_mismatch"},
	{auth.ErrChecksumMismatch, s3Err(http.StatusBadRequest, "BadDigest",
		"The checksum you specified did not match what was received."), "checksum_mismatch"},
	{auth.ErrPayloadHashMismatch, s3Err(http.StatusBadRequest, "XAmzContentSHA256Mismatch",
		"The provided x-amz-content-sha256 header does not match what was computed."), "payload_hash_mismatch"},
	{auth.ErrDigestMalformed, s3Err(http.StatusBadRequest, "InvalidDigest",
		"The Content-MD5 you specified is not valid."), "digest_malformed"},
	{auth.ErrChecksumMalformed, s3Err(http.StatusBadRequest, "InvalidRequest",
		"A checksum header value is invalid."), "checksum_malformed"},
	{auth.ErrDecodedLengthMismatch, s3Err(http.StatusBadRequest, "IncompleteBody",
		"The decoded body length does not match x-amz-decoded-content-length."), "decoded_length_mismatch"},
	{auth.ErrChunkTooLarge, s3Err(http.StatusBadRequest, "InvalidRequest",
		"A streaming chunk exceeded the maximum permitted size."), "chunk_too_large"},
	{auth.ErrChunkMalformed, s3Err(http.StatusBadRequest, "InvalidRequest",
		"Streaming chunk framing is malformed."), "chunk_malformed"},
	{auth.ErrTrailerMalformed, s3Err(http.StatusBadRequest, "InvalidRequest",
		"Streaming trailer block is malformed."), "trailer_malformed"},
}

// asStreamingFault reports the body-validation failure err is, if it is one.
func asStreamingFault(err error) (streamingFault, bool) {
	for _, f := range streamingFaults {
		if errors.Is(err, f.err) {
			return f, true
		}
	}
	return streamingFault{}, false
}
