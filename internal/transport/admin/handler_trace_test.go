// -------------------------------------------------------------------------------
// Admin API - trace snapshot tests
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeWriterTo stands in for *runtime/trace.FlightRecorder so tests stay
// parallel-safe (only one real FlightRecorder may run per process) and
// can drive the error branch.
type fakeWriterTo struct {
	payload []byte
	err     error
}

// WriteTo writes the canned payload or returns the canned error.
func (f *fakeWriterTo) WriteTo(w io.Writer) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	n, err := w.Write(f.payload)
	return int64(n), err
}

// TestTraceSnapshot_Disabled pins the 503 response when no FlightRecorder is wired.
func TestTraceSnapshot_Disabled(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/admin/api/trace/snapshot", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

// TestTraceSnapshot_Success pins the happy path: headers + payload bytes pass through.
func TestTraceSnapshot_Success(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t)
	h.flightRec = &fakeWriterTo{payload: []byte("trace-bytes")}
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/admin/api/trace/snapshot", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", ct)
	}
	if w.Header().Get("Content-Disposition") == "" {
		t.Errorf("Content-Disposition is empty")
	}
	if got := w.Body.String(); got != "trace-bytes" {
		t.Errorf("body = %q, want %q", got, "trace-bytes")
	}
}

// TestTraceSnapshot_WriteToError pins that a WriteTo failure does not
// double-write a JSON error onto the binary stream.
func TestTraceSnapshot_WriteToError(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t)
	h.flightRec = &fakeWriterTo{err: errors.New("simulated WriteTo failure")}
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/admin/api/trace/snapshot", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Body.Len() != 0 {
		t.Errorf("body should be empty on WriteTo error, got %q", w.Body.String())
	}
}
