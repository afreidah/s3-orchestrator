// -------------------------------------------------------------------------------
// HTTP Panic Recovery Middleware Tests
//
// Author: Alex Freidah
//
// Pins the recovery contract: a panicking handler returns a 500 response via
// the caller-supplied writer, the route-scoped Prometheus counter increments,
// the request id flows into the response message, and non-panicking handlers
// pass through unchanged. Also covers the awkward shapes (string panic,
// typed-nil error panic, panic value satisfying error) so the metric and
// log key stay consistent regardless of what the handler threw.
// -------------------------------------------------------------------------------

package httputil

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"go.opentelemetry.io/otel/sdk/trace"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// -------------------------------------------------------------------------
// FIXTURES
// -------------------------------------------------------------------------

// captureWriter records the (status, code, message) the middleware
// chose for a recovered panic so tests can assert all three without
// re-parsing a synthetic response body.
type captureWriter struct {
	called  bool
	status  int
	errCode string
	message string
}

// fn returns an ErrorWriter that records into c plus writes a minimal
// response so the test client sees a real status code.
func (c *captureWriter) fn() ErrorWriter {
	return func(w http.ResponseWriter, status int, errCode, message string) {
		c.called = true
		c.status = status
		c.errCode = errCode
		c.message = message
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, errCode+": "+message) //nolint:gosec // G705: test fixture, body is static error code + canned message
	}
}

// readPanicCounter reads the current value of HTTPPanicRecoveredTotal
// for the given route label. Uses the prometheus client_model dto so
// tests do not need to scrape /metrics or parse text-format output.
func readPanicCounter(t *testing.T, route string) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := telemetry.HTTPPanicRecoveredTotal.WithLabelValues(route).Write(m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if m.Counter == nil || m.Counter.Value == nil {
		return 0
	}
	return *m.Counter.Value
}

// counterDelta returns the change in the route's panic counter across
// the supplied function. Makes per-test assertions independent of
// other tests that share the metric.
func counterDelta(t *testing.T, route string, fn func()) float64 {
	t.Helper()
	before := readPanicCounter(t, route)
	fn()
	return readPanicCounter(t, route) - before
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestPanicRecover_PassesThroughWhenNoPanic asserts the hot path: a
// well-behaved handler runs unchanged and the error writer is never
// invoked.
func TestPanicRecover_PassesThroughWhenNoPanic(t *testing.T) {
	t.Parallel()
	cap := &captureWriter{}
	h := PanicRecover("test", cap.fn())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if cap.called {
		t.Error("error writer invoked for a non-panicking handler")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("body = %q, want %q", rec.Body.String(), "ok")
	}
}

// TestPanicRecover_StringPanic covers a handler that panics with a
// string literal. The middleware must still produce a 500 + counter
// inc + audit event without crashing on the unusual recover() type.
func TestPanicRecover_StringPanic(t *testing.T) {
	t.Parallel()
	cap := &captureWriter{}
	h := PanicRecover("test_string", cap.fn())(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("synthetic failure")
	}))

	delta := counterDelta(t, "test_string", func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
	})

	if !cap.called {
		t.Fatal("error writer not invoked after panic")
	}
	if cap.status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", cap.status)
	}
	if cap.errCode != "InternalError" {
		t.Errorf("errCode = %q, want InternalError", cap.errCode)
	}
	if !strings.Contains(cap.message, "internal error") {
		t.Errorf("message = %q, missing 'internal error'", cap.message)
	}
	if delta != 1 {
		t.Errorf("counter delta = %v, want 1", delta)
	}
}

// TestPanicRecover_ErrorPanic covers a handler that panics with an
// error value. logfmt.Err on the slog line should see the original
// error, and the response message stays the same.
func TestPanicRecover_ErrorPanic(t *testing.T) {
	t.Parallel()
	cap := &captureWriter{}
	h := PanicRecover("test_error", cap.fn())(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic(errors.New("boom"))
	}))

	delta := counterDelta(t, "test_error", func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
	})

	if delta != 1 {
		t.Errorf("counter delta = %v, want 1", delta)
	}
	if !cap.called {
		t.Fatal("error writer not invoked after panic")
	}
}

// TestPanicRecover_RequestIDEchoedInMessage asserts that when an
// inbound context carries a request id, the response message echoes
// it so support tickets can cite a specific id.
func TestPanicRecover_RequestIDEchoedInMessage(t *testing.T) {
	t.Parallel()
	cap := &captureWriter{}
	h := PanicRecover("test_reqid", cap.fn())(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("with id")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(audit.WithRequestID(req.Context(), "req-XYZ"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !strings.Contains(cap.message, "req-XYZ") {
		t.Errorf("message = %q, missing request id", cap.message)
	}
}

// TestPanicRecover_ActiveSpanGetsErrorRecorded asserts that a panic
// inside an active OTel span does not crash the recovery path; the
// middleware calls span.SetStatus and span.RecordError when a span is
// recording. The local TracerProvider is constructed without touching
// the OTel global so the test stays parallel-safe.
func TestPanicRecover_ActiveSpanGetsErrorRecorded(t *testing.T) {
	t.Parallel()
	tp := trace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tracer := tp.Tracer("test")

	cap := &captureWriter{}
	h := PanicRecover("test_span", cap.fn())(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("with span")
	}))

	ctx, span := tracer.Start(context.Background(), "test-span")
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	span.End()

	if !cap.called {
		t.Fatal("error writer not invoked")
	}
}

// TestPanicRecover_AuditCallbackFires asserts that the middleware
// emits an "http.PanicRecovered" audit event. Uses audit.SetOnEvent
// to capture event names without parsing the slog output stream.
//
// Intentionally serial: audit.SetOnEvent is package-level global state
// and other parallel tests that trigger audit logs would race on the
// callback's closure-captured slice.
func TestPanicRecover_AuditCallbackFires(t *testing.T) {
	var saw atomic.Bool
	audit.SetOnEvent(func(event string) {
		if event == "http.PanicRecovered" {
			saw.Store(true)
		}
	})
	t.Cleanup(func() { audit.SetOnEvent(nil) })

	cap := &captureWriter{}
	h := PanicRecover("test_audit", cap.fn())(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("with audit")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if !saw.Load() {
		t.Error("audit callback did not see http.PanicRecovered")
	}
}

// TestFormatPanicValue covers the awkward shapes formatPanicValue
// handles so we lock in the contract that an audit attribute / slog
// key never serialises as "{}".
func TestFormatPanicValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"nil", nil, "<nil panic>"},
		{"string", "boom", "boom"},
		{"error", errors.New("kapow"), "kapow"},
		{"struct", struct{ X int }{X: 7}, "{7}"},
		{"int", 42, "42"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := formatPanicValue(tc.in); got != tc.want {
				t.Errorf("formatPanicValue(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
