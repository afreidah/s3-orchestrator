// -------------------------------------------------------------------------------
// Streaming SigV4 Transport Wiring Tests
//
// Author: Alex Freidah
//
// Unit coverage for the error classifier, the request body adapter, and
// fail's streaming branch. Each test constructs the smallest synthetic
// request the helper needs and asserts the observable outcome (status
// code, response body, adjusted headers, swapped body) without spinning
// up a server.
// -------------------------------------------------------------------------------

package s3api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
)

// TestClassifyError covers every streaming sentinel, the tag validation
// errors, a wrapped S3 error, and the fallback for anything else.
func TestClassifyError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantReason string
	}{
		{"chunk_sig_mismatch", auth.ErrChunkSignatureMismatch, http.StatusForbidden, "SignatureDoesNotMatch", "chunk_signature_mismatch"},
		{"trailer_sig_mismatch", auth.ErrTrailerSignatureMismatch, http.StatusForbidden, "SignatureDoesNotMatch", "trailer_signature_mismatch"},
		{"checksum_mismatch", auth.ErrChecksumMismatch, http.StatusBadRequest, "BadDigest", "checksum_mismatch"},
		{"decoded_length_mismatch", auth.ErrDecodedLengthMismatch, http.StatusBadRequest, "IncompleteBody", "decoded_length_mismatch"},
		{"chunk_too_large", auth.ErrChunkTooLarge, http.StatusBadRequest, "InvalidRequest", "chunk_too_large"},
		{"chunk_malformed", auth.ErrChunkMalformed, http.StatusBadRequest, "InvalidRequest", "chunk_malformed"},
		{"trailer_malformed", auth.ErrTrailerMalformed, http.StatusBadRequest, "InvalidRequest", "trailer_malformed"},
		{"too_many_tags", core.ErrTooManyTags, http.StatusBadRequest, "BadRequest", ""},
		{"invalid_tag", core.ErrEmptyTagKey, http.StatusBadRequest, "InvalidTag", ""},
		{"tagging_directive", errInvalidTaggingDirective, http.StatusBadRequest, "InvalidArgument", ""},
		{"wrapped_s3_error", fmt.Errorf("lookup: %w", core.ErrObjectNotFound), http.StatusNotFound, "NoSuchKey", ""},
		{"anything_else", errors.New("something else"), http.StatusBadGateway, "InternalError", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s3e, reason := classifyError(tc.err)
			if s3e.StatusCode != tc.wantStatus || s3e.Code != tc.wantCode || reason != tc.wantReason {
				t.Errorf("classifyError = (%d %s, %q), want (%d %s, %q)",
					s3e.StatusCode, s3e.Code, reason, tc.wantStatus, tc.wantCode, tc.wantReason)
			}
		})
	}
}

// TestApplyStreamingBody asserts the request envelope is rewritten
// (decoded content length, stripped streaming headers, replaced body)
// when streaming material is present.
func TestApplyStreamingBody(t *testing.T) {
	t.Parallel()
	body := strings.NewReader("framed-body-bytes-replaced-after-call")
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "/bucket/key", io.NopCloser(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	r.Header.Set("Content-Encoding", "aws-chunked")
	r.Header.Set("X-Amz-Decoded-Content-Length", "1234")
	r.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc32")
	r.Header.Set("X-Amz-Content-Sha256", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD")
	r.ContentLength = 9999

	mat := &auth.StreamingMaterial{ //nolint:gosec // deterministic test fixture, not a credential
		Variant:    auth.StreamingSigned,
		DecodedLen: 1234,
		SeedSig:    strings.Repeat("a", 64),
		SigningKey: []byte("k"),
		CredScope:  "20260507/us-east-1/s3/aws4_request",
		AmzDate:    "20260507T000000Z",
	}
	originalBody := r.Body
	applyStreamingBody(r, mat)

	if r.ContentLength != 1234 {
		t.Errorf("ContentLength = %d, want 1234", r.ContentLength)
	}
	if got := r.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want empty", got)
	}
	if got := r.Header.Get("X-Amz-Decoded-Content-Length"); got != "" {
		t.Errorf("X-Amz-Decoded-Content-Length = %q, want empty", got)
	}
	if got := r.Header.Get("X-Amz-Trailer"); got != "" {
		t.Errorf("X-Amz-Trailer = %q, want empty", got)
	}
	if got := r.Header.Get("X-Amz-Content-Sha256"); got != "UNSIGNED-PAYLOAD" {
		t.Errorf("X-Amz-Content-Sha256 = %q, want UNSIGNED-PAYLOAD", got)
	}
	if r.Body == originalBody {
		t.Error("body should be replaced with the chunk reader")
	}
}

// TestFail_StreamingMapped asserts a streaming sentinel answers its own S3
// error rather than the generic InternalError, and counts the rejection.
func TestFail_StreamingMapped(t *testing.T) {
	t.Parallel()
	counter := telemetry.AuthStreamingRejectionsTotal.WithLabelValues("chunk_signature_mismatch")
	before := testutil.ToFloat64(counter)
	rec := httptest.NewRecorder()
	if status := fail(rec, fmt.Errorf("read body: %w", auth.ErrChunkSignatureMismatch)); status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", status)
	}
	if body := rec.Body.String(); !strings.Contains(body, "SignatureDoesNotMatch") {
		t.Errorf("body should contain SignatureDoesNotMatch, got %q", body)
	}
	if testutil.ToFloat64(counter) <= before {
		t.Error("the streaming rejection was not counted")
	}
}
