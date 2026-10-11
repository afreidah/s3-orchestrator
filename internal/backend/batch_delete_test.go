// -------------------------------------------------------------------------------
// Backend Tests - Batch Delete
//
// Author: Alex Freidah
//
// Exercises S3Backend.DeleteObjects against an httptest fake S3: chunking at
// the per-request key limit, per-key failures from the response, a provider
// without multi-object delete, and a request that fails outright. Also covers
// the circuit breaker's passthrough of the capability.
// -------------------------------------------------------------------------------

package backend

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/smithy-go"
)

// -------------------------------------------------------------------------
// FIXTURES
// -------------------------------------------------------------------------

// deleteRequest is the body of an S3 DeleteObjects request.
type deleteRequest struct {
	Objects []struct {
		Key string `xml:"Key"`
	} `xml:"Object"`
}

// batchDeleteServer is a fake S3 that answers DeleteObjects, recording the
// size of each request and reporting the keys in keyErrs as failed with the
// given S3 error code.
type batchDeleteServer struct {
	mu       sync.Mutex
	requests []int
	keyErrs  map[string]string
	status   int
}

// handle answers one DeleteObjects request.
func (s *batchDeleteServer) handle(w http.ResponseWriter, r *http.Request) {
	if _, ok := r.URL.Query()["delete"]; !ok || r.Method != http.MethodPost {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req deleteRequest
	_ = xml.Unmarshal(body, &req)

	s.mu.Lock()
	s.requests = append(s.requests, len(req.Objects))
	status := s.status
	s.mu.Unlock()

	if status != 0 {
		code := "InternalError"
		switch status {
		case http.StatusNotImplemented:
			code = "NotImplemented"
		case http.StatusMethodNotAllowed:
			code = "MethodNotAllowed"
		case http.StatusBadRequest:
			code = "InvalidRequest"
		case http.StatusForbidden:
			code = "AccessDenied"
		}
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `<Error><Code>%s</Code><Message>no</Message></Error>`, code)
		return
	}
	var out strings.Builder
	out.WriteString(`<DeleteResult>`)
	for _, o := range req.Objects {
		if code, ok := s.keyErrs[o.Key]; ok {
			fmt.Fprintf(&out, `<Error><Key>%s</Key><Code>%s</Code><Message>failed</Message></Error>`, o.Key, code)
		}
	}
	out.WriteString(`</DeleteResult>`)
	_, _ = io.WriteString(w, out.String())
}

// keys returns n distinct keys.
func keys(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("k-%04d", i)
	}
	return out
}

// -------------------------------------------------------------------------
// S3 BACKEND
// -------------------------------------------------------------------------

// TestDeleteObjects_ChunksAndReportsPerKeyFailures verifies keys are sent in
// requests of at most maxBatchDeleteKeys, and that the keys the response names
// come back failed, with NoSuchKey classified as not found.
func TestDeleteObjects_ChunksAndReportsPerKeyFailures(t *testing.T) {
	t.Parallel()
	srv := &batchDeleteServer{keyErrs: map[string]string{"k-0003": "AccessDenied", "k-1000": "NoSuchKey"}}
	be := newTestBackend(t, srv.handle)

	failed, err := be.DeleteObjects(t.Context(), keys(maxBatchDeleteKeys+1))
	if err != nil {
		t.Fatalf("DeleteObjects: %v", err)
	}
	if len(srv.requests) != 2 || srv.requests[0] != maxBatchDeleteKeys || srv.requests[1] != 1 {
		t.Errorf("request sizes = %v, want [%d 1]", srv.requests, maxBatchDeleteKeys)
	}
	if len(failed) != 2 {
		t.Fatalf("failed = %v, want two keys", failed)
	}
	if IsNotFound(failed["k-0003"]) {
		t.Errorf("AccessDenied classified as not found: %v", failed["k-0003"])
	}
	if !IsNotFound(failed["k-1000"]) {
		t.Errorf("NoSuchKey not classified as not found: %v", failed["k-1000"])
	}
}

// TestDeleteObjects_RejectedRequestIsUnsupported verifies a provider that
// refuses the batch request itself - no multi-object delete, the method not
// allowed, or a header it requires missing - is reported as
// ErrBatchDeleteNotSupported, so the caller deletes singly instead of treating
// every key as failed.
func TestDeleteObjects_RejectedRequestIsUnsupported(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusNotImplemented, http.StatusMethodNotAllowed, http.StatusBadRequest} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			be := newTestBackend(t, (&batchDeleteServer{status: status}).handle)

			if _, err := be.DeleteObjects(t.Context(), keys(3)); !errors.Is(err, ErrBatchDeleteNotSupported) {
				t.Errorf("err = %v, want ErrBatchDeleteNotSupported", err)
			}
		})
	}
}

// TestIsBatchRejected verifies a rejection is recognized by HTTP status or by
// S3 error code alone, and that failures single deletes would share are not.
func TestIsBatchRejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"status not implemented", &httpError{code: http.StatusNotImplemented}, true},
		{"status method not allowed", &httpError{code: http.StatusMethodNotAllowed}, true},
		{"status bad request", &httpError{code: http.StatusBadRequest}, true},
		{"status forbidden", &httpError{code: http.StatusForbidden}, false},
		{"status server error", &httpError{code: http.StatusInternalServerError}, false},
		{"code not implemented", &smithy.GenericAPIError{Code: "NotImplemented"}, true},
		{"code method not allowed", &smithy.GenericAPIError{Code: "MethodNotAllowed"}, true},
		{"code internal error", &smithy.GenericAPIError{Code: "InternalError"}, false},
		{"plain error", errors.New("connection reset"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isBatchRejected(tc.err); got != tc.want {
				t.Errorf("isBatchRejected(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestDeleteObjects_FailedRequestFailsItsKeys verifies a request that fails as
// a whole for a reason single deletes would share reports every key in it
// failed, since none can be assumed deleted.
func TestDeleteObjects_FailedRequestFailsItsKeys(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusInternalServerError, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			be := newTestBackend(t, (&batchDeleteServer{status: status}).handle)

			failed, err := be.DeleteObjects(t.Context(), keys(3))
			if err != nil {
				t.Fatalf("DeleteObjects: %v", err)
			}
			if len(failed) != 3 {
				t.Errorf("failed = %v, want all 3 keys", failed)
			}
		})
	}
}

// -------------------------------------------------------------------------
// CIRCUIT BREAKER
// -------------------------------------------------------------------------

// TestCBBackend_DeleteObjects verifies the breaker forwards a batch delete to a
// backend that supports one, reports ErrBatchDeleteNotSupported for one that
// does not, and does not count that answer as a backend failure.
func TestCBBackend_DeleteObjects(t *testing.T) {
	t.Parallel()
	srv := &batchDeleteServer{}
	supported := NewCircuitBreakerBackend(newTestBackend(t, srv.handle), CircuitBreakerConfig{Name: "s3", Threshold: 1, Timeout: time.Minute})
	if failed, err := supported.DeleteObjects(t.Context(), keys(2)); err != nil || len(failed) != 0 {
		t.Errorf("supported: failed = %v, err = %v; want all deleted", failed, err)
	}
	if len(srv.requests) != 1 {
		t.Errorf("requests = %v, want one batch", srv.requests)
	}

	unsupported := newTestCBBackend(newMockBackend(), 1, time.Minute)
	if _, err := unsupported.DeleteObjects(t.Context(), keys(2)); !errors.Is(err, ErrBatchDeleteNotSupported) {
		t.Errorf("unsupported: err = %v, want ErrBatchDeleteNotSupported", err)
	}
	if isBackendError(ErrBatchDeleteNotSupported) {
		t.Error("a declined batch delete counts against the breaker")
	}
}
