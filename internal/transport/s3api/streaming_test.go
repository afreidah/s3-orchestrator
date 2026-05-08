// -------------------------------------------------------------------------------
// Streaming SigV4 Transport Wiring Tests
//
// Author: Alex Freidah
//
// Unit coverage for the streaming-error mapper, the request body
// adapter, and writeStorageError's streaming branch. Each test
// constructs the smallest synthetic request the helper needs and
// asserts the observable outcome (status code, response body,
// adjusted headers, swapped body) without spinning up a server.
// -------------------------------------------------------------------------------

package s3api

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
)

// TestStreamingErrorResponse covers every typed sentinel plus the
// no-match passthrough case.
func TestStreamingErrorResponse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantReason string
		wantOK     bool
	}{
		{"chunk_sig_mismatch", auth.ErrChunkSignatureMismatch, http.StatusForbidden, "SignatureDoesNotMatch", "chunk_signature_mismatch", true},
		{"trailer_sig_mismatch", auth.ErrTrailerSignatureMismatch, http.StatusForbidden, "SignatureDoesNotMatch", "trailer_signature_mismatch", true},
		{"decoded_length_mismatch", auth.ErrDecodedLengthMismatch, http.StatusBadRequest, "IncompleteBody", "decoded_length_mismatch", true},
		{"chunk_too_large", auth.ErrChunkTooLarge, http.StatusBadRequest, "InvalidRequest", "chunk_too_large", true},
		{"chunk_malformed", auth.ErrChunkMalformed, http.StatusBadRequest, "InvalidRequest", "chunk_malformed", true},
		{"trailer_malformed", auth.ErrTrailerMalformed, http.StatusBadRequest, "InvalidRequest", "trailer_malformed", true},
		{"non_streaming_passthrough", errors.New("something else"), 0, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			status, code, _, reason, ok := streamingErrorResponse(tc.err)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
			if status != tc.wantStatus {
				t.Errorf("status = %d, want %d", status, tc.wantStatus)
			}
			if code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
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

// TestWriteStorageError_StreamingMapped asserts a typed streaming
// sentinel routes through streamingErrorResponse rather than the
// generic InternalError fallback.
func TestWriteStorageError_StreamingMapped(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	status := writeStorageError(rec, auth.ErrChunkSignatureMismatch, "fallback-msg")
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", status)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "SignatureDoesNotMatch") {
		t.Errorf("body should contain SignatureDoesNotMatch, got %q", body)
	}
	if strings.Contains(body, "fallback-msg") {
		t.Error("fallback message leaked through; streaming branch should have fired first")
	}
}
