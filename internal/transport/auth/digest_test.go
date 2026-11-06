// -------------------------------------------------------------------------------
// Declared Digest Tests
//
// Author: Alex Freidah
//
// The cases the transport tests cannot reach: bodies of unknown length, bodies
// cut short, and requests the reader should leave alone. The refusals themselves
// are covered end to end in the s3api package.
// -------------------------------------------------------------------------------

package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
)

// digestRequest builds a request whose body is body, declaring the SHA-256 of
// declared as its payload hash.
func digestRequest(t *testing.T, body io.Reader, length int64, declared string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "http://example.invalid/b/k", body)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = length
	sum := sha256.Sum256([]byte(declared))
	req.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(sum[:]))
	return req
}

// TestVerifyDigests_UnknownLengthComparesAtEOF verifies a body with no declared
// length is still checked, once it ends.
func TestVerifyDigests_UnknownLengthComparesAtEOF(t *testing.T) {
	t.Parallel()
	req := digestRequest(t, strings.NewReader("sent"), -1, "declared")
	VerifyDigests(req)
	if _, err := io.ReadAll(req.Body); !errors.Is(err, ErrPayloadHashMismatch) {
		t.Errorf("err = %v, want ErrPayloadHashMismatch", err)
	}
}

// TestVerifyDigests_ComparesOnTheLastByte verifies a reader stopping at the
// declared length sees the failure without reading on to EOF.
func TestVerifyDigests_ComparesOnTheLastByte(t *testing.T) {
	t.Parallel()
	req := digestRequest(t, strings.NewReader("sent"), 4, "declared")
	VerifyDigests(req)
	if n, err := req.Body.Read(make([]byte, 4)); n != 4 || !errors.Is(err, ErrPayloadHashMismatch) {
		t.Errorf("Read = %d, %v; want 4 and ErrPayloadHashMismatch", n, err)
	}
}

// TestVerifyDigests_ShortBodyKeepsItsError verifies a body cut off partway reports
// that failure rather than a digest mismatch, which would blame the client.
func TestVerifyDigests_ShortBodyKeepsItsError(t *testing.T) {
	t.Parallel()
	cut := errors.New("connection reset")
	req := digestRequest(t, iotest.ErrReader(cut), 4, "declared")
	VerifyDigests(req)
	if _, err := io.ReadAll(req.Body); !errors.Is(err, cut) {
		t.Errorf("err = %v, want the read error", err)
	}
}

// TestVerifyDigests_LeavesOtherRequestsAlone verifies a request with no body, or
// one declaring only a sentinel, keeps the body it came with.
func TestVerifyDigests_LeavesOtherRequestsAlone(t *testing.T) {
	t.Parallel()
	empty := digestRequest(t, http.NoBody, 0, "declared")
	VerifyDigests(empty)
	if empty.Body != http.NoBody {
		t.Error("wrapped a request with no body")
	}

	body := io.NopCloser(strings.NewReader("sent"))
	sentinel := digestRequest(t, body, 4, "")
	sentinel.Header.Set("X-Amz-Content-Sha256", unsignedPayload)
	VerifyDigests(sentinel)
	if sentinel.Body != body {
		t.Error("wrapped a request declaring no digest")
	}
}
