// -------------------------------------------------------------------------------
// CircuitBreakerBackend Tests
//
// Author: Alex Freidah
//
// Tests for the circuit breaker wrapper: calls forward while closed and return
// breaker.ErrBackendUnavailable while open, a failure to reach the backend
// takes out every call while a refusal takes out only its own, the served set
// and the backend-level transitions follow the breakers, each breaker's probe
// passes or fails on the right answers, and client requests never probe.
// -------------------------------------------------------------------------------

package backend

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// newTestCBBackend constructs a new test cbbackend.
func newTestCBBackend(mock *mockBackend, threshold int, timeout time.Duration) *CircuitBreakerBackend {
	return NewCircuitBreakerBackend(mock, CircuitBreakerConfig{Name: "test-backend", Threshold: threshold, Timeout: timeout})
}

// servesAll reports whether cb serves every call.
func servesAll(cb *CircuitBreakerBackend) bool { return cb.Served() == core.AllCalls }

// probeFor returns cb's probe charged as op.
func probeFor(t *testing.T, cb *CircuitBreakerBackend, op s3op.Operation) Probe {
	t.Helper()
	for _, p := range cb.Probes() {
		if p.Op == op {
			return p
		}
	}
	t.Fatalf("no probe for %s", op)
	return Probe{}
}

// -------------------------------------------------------------------------
// Forwarding when closed
// -------------------------------------------------------------------------

// TestCBBackend_PutObject_Forwards verifies the cbbackend put object forwards contract.
// Asserts that PutObject:.
func TestCBBackend_PutObject_Forwards(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	cb := newTestCBBackend(mock, 3, time.Minute)

	etag, err := cb.PutObject(context.Background(), "key", strings.NewReader("data"), 4, "text/plain", nil)
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if etag == "" {
		t.Fatal("expected non-empty etag")
	}
}

// TestCBBackend_GetObject_Forwards verifies the cbbackend get object forwards contract.
// Asserts that GetObject:.
func TestCBBackend_GetObject_Forwards(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	cb := newTestCBBackend(mock, 3, time.Minute)

	_, _ = cb.PutObject(context.Background(), "key", strings.NewReader("data"), 4, "text/plain", nil)

	result, err := cb.GetObject(context.Background(), "key", "")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	_ = result.Body.Close()
	if result.Size != 4 {
		t.Fatalf("expected size 4, got %d", result.Size)
	}
}

// TestCBBackend_HeadObject_Forwards verifies the cbbackend head object forwards contract.
// Asserts that HeadObject:.
func TestCBBackend_HeadObject_Forwards(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	cb := newTestCBBackend(mock, 3, time.Minute)

	_, _ = cb.PutObject(context.Background(), "key", strings.NewReader("data"), 4, "text/plain", nil)

	result, err := cb.HeadObject(context.Background(), "key")
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if result.Size != 4 {
		t.Fatalf("expected size 4, got %d", result.Size)
	}
}

// TestCBBackend_DeleteObject_Forwards verifies the cbbackend delete object forwards contract.
// Asserts that DeleteObject:.
func TestCBBackend_DeleteObject_Forwards(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	cb := newTestCBBackend(mock, 3, time.Minute)

	_, _ = cb.PutObject(context.Background(), "key", strings.NewReader("data"), 4, "text/plain", nil)

	if err := cb.DeleteObject(context.Background(), "key"); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	// Verify it's deleted
	mock.mu.Lock()
	_, exists := mock.objects["key"]
	mock.mu.Unlock()
	if exists {
		t.Fatal("object should be deleted")
	}
}

// -------------------------------------------------------------------------
// Circuit open
// -------------------------------------------------------------------------

// TestCBBackend_PutObject_CircuitOpen verifies the cbbackend put object circuit open contract.
// Asserts that expected breaker.ErrBackendUnavailable, got.
func TestCBBackend_PutObject_CircuitOpen(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	mock.putErr = errors.New("connection refused")
	cb := newTestCBBackend(mock, 1, time.Minute)

	// Trip the circuit
	_, _ = cb.PutObject(context.Background(), "key", strings.NewReader("data"), 4, "text/plain", nil)

	// Next call should return breaker.ErrBackendUnavailable without hitting mock
	_, err := cb.PutObject(context.Background(), "key2", strings.NewReader("data"), 4, "text/plain", nil)
	if !errors.Is(err, breaker.ErrBackendUnavailable) {
		t.Fatalf("expected breaker.ErrBackendUnavailable, got %v", err)
	}
}

// TestCBBackend_GetObject_CircuitOpen verifies the cbbackend get object circuit open contract.
// Asserts that expected breaker.ErrBackendUnavailable, got.
func TestCBBackend_GetObject_CircuitOpen(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	mock.getErr = errors.New("connection refused")
	cb := newTestCBBackend(mock, 1, time.Minute)

	// Trip the circuit
	_, _ = cb.GetObject(context.Background(), "key", "")

	_, err := cb.GetObject(context.Background(), "key", "")
	if !errors.Is(err, breaker.ErrBackendUnavailable) {
		t.Fatalf("expected breaker.ErrBackendUnavailable, got %v", err)
	}
}

// TestCBBackend_HeadObject_CircuitOpen verifies the cbbackend head object circuit open contract.
// Asserts that expected breaker.ErrBackendUnavailable, got.
func TestCBBackend_HeadObject_CircuitOpen(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	mock.headErr = errors.New("connection refused")
	cb := newTestCBBackend(mock, 1, time.Minute)

	_, _ = cb.HeadObject(context.Background(), "key")

	_, err := cb.HeadObject(context.Background(), "key")
	if !errors.Is(err, breaker.ErrBackendUnavailable) {
		t.Fatalf("expected breaker.ErrBackendUnavailable, got %v", err)
	}
}

// TestCBBackend_DeleteObject_CircuitOpen verifies the cbbackend delete object circuit open contract.
// Asserts that expected breaker.ErrBackendUnavailable, got.
func TestCBBackend_DeleteObject_CircuitOpen(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	mock.delErr = errors.New("connection refused")
	cb := newTestCBBackend(mock, 1, time.Minute)

	_ = cb.DeleteObject(context.Background(), "key")

	err := cb.DeleteObject(context.Background(), "key")
	if !errors.Is(err, breaker.ErrBackendUnavailable) {
		t.Fatalf("expected breaker.ErrBackendUnavailable, got %v", err)
	}
}

// -------------------------------------------------------------------------
// Recovery
// -------------------------------------------------------------------------

// TestCBBackend_ClientRequestsNeverProbe verifies that once open, the breaker
// refuses every client request even after the open timeout, and closes only
// through Recover.
func TestCBBackend_ClientRequestsNeverProbe(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		mock := newMockBackend()
		mock.putErr = errors.New("connection refused")
		cb := newTestCBBackend(mock, 1, 10*time.Millisecond)

		_, _ = cb.PutObject(context.Background(), "key", strings.NewReader("data"), 4, "text/plain", nil)
		time.Sleep(time.Second)

		mock.mu.Lock()
		mock.putErr = nil
		mock.mu.Unlock()

		_, err := cb.PutObject(context.Background(), "key", strings.NewReader("data"), 4, "text/plain", nil)
		if !errors.Is(err, breaker.ErrBackendUnavailable) {
			t.Fatalf("PutObject after the open timeout = %v, want ErrBackendUnavailable", err)
		}
		if servesAll(cb) {
			t.Error("every call served after a client request: client requests are never probes")
		}

		probeFor(t, cb, s3op.HeadBucket).Breaker.Recover()
		if _, err := cb.PutObject(context.Background(), "key", strings.NewReader("data"), 4, "text/plain", nil); err != nil {
			t.Fatalf("PutObject after Recover: %v", err)
		}
	})
}

// TestCBBackend_OpeningErrorKeepsCause verifies the error that opens the
// circuit still matches ErrBackendUnavailable and carries the backend's own
// error, so logs show what failed.
func TestCBBackend_OpeningErrorKeepsCause(t *testing.T) {
	t.Parallel()
	cause := &httpError{code: 503, msg: "SlowDown"}
	mock := newMockBackend()
	mock.getErr = cause
	cb := newTestCBBackend(mock, 1, time.Minute)

	_, err := cb.GetObject(context.Background(), "key", "")
	if !errors.Is(err, breaker.ErrBackendUnavailable) {
		t.Errorf("err = %v, want it to match ErrBackendUnavailable", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("err = %v, want it to wrap the backend error", err)
	}
}

// TestCBBackend_RequestErrorsDoNotOpen verifies that a healthy backend
// answering request-specific 4xx errors - a range past the end of a zero-byte
// object, a failed precondition - stays closed however many arrive.
func TestCBBackend_RequestErrorsDoNotOpen(t *testing.T) {
	t.Parallel()
	for _, code := range []int{400, 409, 412, 416} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			t.Parallel()
			mock := newMockBackend()
			mock.getErr = &httpError{code: code, msg: "request error"}
			cb := newTestCBBackend(mock, 3, time.Minute)

			for range 10 {
				_, _ = cb.GetObject(context.Background(), "key", "bytes=0-31")
			}
			if !servesAll(cb) {
				t.Errorf("served after ten %d responses = %s, want every call", code, cb.Served())
			}
		})
	}
}

// -------------------------------------------------------------------------
// ListObjects
// -------------------------------------------------------------------------

// TestCBBackend_ListObjectsRespectsTheCircuit verifies a listing reaches the
// backend while the circuit is closed and is refused once it opens.
func TestCBBackend_ListObjectsRespectsTheCircuit(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	mock.getErr = errors.New("connection refused")
	cb := newTestCBBackend(mock, 1, time.Minute)
	noop := func([]ListedObject) error { return nil }

	if err := cb.ListObjects(context.Background(), "", noop); err != nil {
		t.Fatalf("ListObjects on a closed circuit = %v, want nil", err)
	}
	_, _ = cb.GetObject(context.Background(), "key", "")
	if err := cb.ListObjects(context.Background(), "", noop); !errors.Is(err, breaker.ErrBackendUnavailable) {
		t.Errorf("ListObjects on an open circuit = %v, want ErrBackendUnavailable", err)
	}
}

// TestCBBackend_BackendProbeBypassesOpenCircuit verifies the backend
// breaker's check reaches the bucket while the circuit is open.
func TestCBBackend_BackendProbeBypassesOpenCircuit(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	mock.getErr = errors.New("connection refused")
	cb := newTestCBBackend(mock, 1, time.Minute)
	_, _ = cb.GetObject(context.Background(), "key", "")
	check := probeFor(t, cb, s3op.HeadBucket).Check

	if err := check(context.Background()); err != nil {
		t.Errorf("check on an open circuit = %v, want the backend's nil", err)
	}
	mock.mu.Lock()
	mock.bucketErr = errors.New("still down")
	mock.mu.Unlock()
	if err := check(context.Background()); err == nil {
		t.Error("check = nil, want the backend's failure")
	}
}

// -------------------------------------------------------------------------
// Served calls
// -------------------------------------------------------------------------

// TestCBBackend_RefusalTakesOutOnlyItsCall verifies a 403 on GET, as a
// download quota answers, opens GET alone: uploads and deletes keep reaching
// the backend.
func TestCBBackend_RefusalTakesOutOnlyItsCall(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	mock.getErr = &httpError{code: 403, msg: "DownloadTrafficExceeded"}
	cb := newTestCBBackend(mock, 1, time.Minute)

	_, _ = cb.GetObject(context.Background(), "key", "")
	if got, want := cb.Served(), core.AllCalls&^core.CallGetObject; got != want {
		t.Fatalf("Served = %s, want %s", got, want)
	}
	if _, err := cb.GetObject(context.Background(), "key", ""); !errors.Is(err, breaker.ErrBackendUnavailable) {
		t.Errorf("GetObject = %v, want ErrBackendUnavailable", err)
	}
	if _, err := cb.PutObject(context.Background(), "key", strings.NewReader("data"), 4, "text/plain", nil); err != nil {
		t.Errorf("PutObject = %v, want it to reach the backend", err)
	}
	if err := cb.DeleteObject(context.Background(), "key"); err != nil {
		t.Errorf("DeleteObject = %v, want it to reach the backend", err)
	}
}

// TestCBBackend_UnreachableTakesOutEveryCall verifies a failure with no HTTP
// status opens the backend breaker, serving nothing, and its recovery brings
// every call back at once.
func TestCBBackend_UnreachableTakesOutEveryCall(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	mock.getErr = errors.New("connection refused")
	cb := newTestCBBackend(mock, 1, time.Minute)

	_, _ = cb.GetObject(context.Background(), "key", "")
	if got := cb.Served(); got != 0 {
		t.Fatalf("Served = %s, want nothing", got)
	}
	if _, err := cb.PutObject(context.Background(), "key", strings.NewReader("data"), 4, "text/plain", nil); !errors.Is(err, breaker.ErrBackendUnavailable) {
		t.Errorf("PutObject = %v, want ErrBackendUnavailable", err)
	}

	probeFor(t, cb, s3op.HeadBucket).Breaker.Recover()
	if got := cb.Served(); got != core.AllCalls {
		t.Errorf("Served after recovery = %s, want every call", got)
	}
}

// TestCBBackend_401TakesOutEveryCall verifies a credential rejection counts
// against the whole backend.
func TestCBBackend_401TakesOutEveryCall(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	mock.headErr = &httpError{code: 401, msg: "Unauthorized"}
	cb := newTestCBBackend(mock, 1, time.Minute)

	_, _ = cb.HeadObject(context.Background(), "key")
	if got := cb.Served(); got != 0 {
		t.Errorf("Served = %s, want nothing", got)
	}
}

// TestCBBackend_BackendLevelTransitions verifies the backend-level hook
// reports open when the first call stops being served and closed only when
// the last one comes back.
func TestCBBackend_BackendLevelTransitions(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	mock.getErr = &httpError{code: 403}
	mock.putErr = &httpError{code: 503}
	cb := newTestCBBackend(mock, 1, time.Minute)
	var seen []breaker.State
	cb.onStateChange = func(info breaker.StateChangeInfo) {
		if info.Name != "test-backend" {
			t.Errorf("transition named %q, want the backend", info.Name)
		}
		seen = append(seen, info.To)
	}

	_, _ = cb.GetObject(context.Background(), "key", "")
	_, _ = cb.PutObject(context.Background(), "key", strings.NewReader("data"), 4, "text/plain", nil)
	probeFor(t, cb, s3op.GetObject).Breaker.Recover()
	if cb.RefusedFor(core.RequiredForWrite) == 0 {
		t.Error("RefusedFor(write) = 0 while PUT is not served")
	}
	if d := cb.RefusedFor(core.RequiredForRead); d != 0 {
		t.Errorf("RefusedFor(read) = %v once GET is served again, want 0", d)
	}
	probeFor(t, cb, s3op.PutObject).Breaker.Recover()

	if want := []breaker.State{breaker.StateOpen, breaker.StateClosed}; !slices.Equal(seen, want) {
		t.Errorf("transitions = %v, want %v", seen, want)
	}
	if d := cb.RefusedFor(core.AllCalls); d != 0 {
		t.Errorf("RefusedFor(all) = %v with every call served, want 0", d)
	}
}

// TestServedCalls_ReadsTheBreaker verifies the package-level helpers read a
// wrapped backend's breakers and treat an unwrapped one as serving every call.
func TestServedCalls_ReadsTheBreaker(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	if got := ServedCalls(mock); got != core.AllCalls {
		t.Errorf("ServedCalls(unwrapped) = %s, want every call", got)
	}
	if d := RefusedFor(mock, core.AllCalls); d != 0 {
		t.Errorf("RefusedFor(unwrapped) = %v, want 0", d)
	}

	mock.getErr = &httpError{code: 403}
	cb := newTestCBBackend(mock, 1, time.Minute)
	_, _ = cb.GetObject(context.Background(), "key", "")
	if got, want := ServedCalls(cb), core.AllCalls&^core.CallGetObject; got != want {
		t.Errorf("ServedCalls(wrapped) = %s, want %s", got, want)
	}
	if RefusedFor(cb, core.RequiredForRead) == 0 {
		t.Error("RefusedFor(wrapped, read) = 0 while GET is refused")
	}
}

// TestCBBackend_RefusedGaugeFollowsServed verifies BackendCallRefused reports
// 0 for every call at startup, 1 for GET alone while GET is refused, and 0
// again once it recovers.
func TestCBBackend_RefusedGaugeFollowsServed(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	mock.getErr = &httpError{code: 403}
	cb := NewCircuitBreakerBackend(mock, CircuitBreakerConfig{Name: "gauge-backend", Threshold: 1, Timeout: time.Minute})
	refused := func(call core.BackendCalls) float64 {
		return promtest.ToFloat64(telemetry.BackendCallRefused.WithLabelValues("gauge-backend", call.String()))
	}

	if refused(core.CallGetObject) != 0 || refused(core.CallPutObject) != 0 {
		t.Fatal("calls reported refused before any failure")
	}
	_, _ = cb.GetObject(context.Background(), "key", "")
	if refused(core.CallGetObject) != 1 || refused(core.CallPutObject) != 0 {
		t.Errorf("after a GET refusal: GET = %v, PUT = %v; want 1 and 0", refused(core.CallGetObject), refused(core.CallPutObject))
	}
	probeFor(t, cb, s3op.GetObject).Breaker.Recover()
	if refused(core.CallGetObject) != 0 {
		t.Errorf("GET = %v after recovery, want 0", refused(core.CallGetObject))
	}
}

// TestCBBackend_EventsNameTheCall verifies each breaker publishes its own
// event: a GET refusal names GetObject, and an unreachable backend names all.
// Not parallel, since the event emitter is process-wide.
func TestCBBackend_EventsNameTheCall(t *testing.T) {
	var calls []any
	event.SetEmitter(func(ev event.Event) {
		if ev.Subject == "event-backend" && ev.Type == event.BackendCircuitOpened {
			calls = append(calls, ev.Data["call"])
		}
	})
	defer event.SetEmitter(nil)

	mock := newMockBackend()
	mock.getErr = &httpError{code: 403}
	mock.headErr = errors.New("connection refused")
	cb := NewCircuitBreakerBackend(mock, CircuitBreakerConfig{Name: "event-backend", Threshold: 1, Timeout: time.Minute})
	_, _ = cb.GetObject(context.Background(), "key", "")
	_, _ = cb.HeadObject(context.Background(), "key")

	if want := []any{"GetObject", backendBreakerCall}; !slices.Equal(calls, want) {
		t.Errorf("opened events named %v, want %v", calls, want)
	}
}

// TestCBBackend_RefusedForCountsTheBackendBreaker verifies an unreachable
// backend refuses every mask, however narrow.
func TestCBBackend_RefusedForCountsTheBackendBreaker(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	mock.headErr = errors.New("connection refused")
	cb := newTestCBBackend(mock, 1, time.Minute)

	_, _ = cb.HeadObject(context.Background(), "key")
	if cb.RefusedFor(core.RequiredForRead) == 0 {
		t.Error("RefusedFor(read) = 0 while the backend breaker is open")
	}
}

// -------------------------------------------------------------------------
// Call probes
// -------------------------------------------------------------------------

// TestCBBackend_EveryBreakerHasAProbe verifies Probes covers the backend
// breaker and each call, each with its own breaker and a check.
func TestCBBackend_EveryBreakerHasAProbe(t *testing.T) {
	t.Parallel()
	cb := newTestCBBackend(newMockBackend(), 1, time.Minute)
	probes := cb.Probes()
	if want := len(core.AllCalls.Names()) + 1; len(probes) != want {
		t.Fatalf("probes = %d, want %d", len(probes), want)
	}
	breakers := make(map[*breaker.CircuitBreaker]bool)
	for _, p := range probes {
		if p.Check == nil {
			t.Errorf("%s has no check", p.Op)
		}
		breakers[p.Breaker] = true
	}
	if len(breakers) != len(probes) {
		t.Errorf("probes share breakers: %d breakers for %d probes", len(breakers), len(probes))
	}
}

// TestCBBackend_GetProbeRetriesTheRefusedKey verifies the GET probe asks for
// the key the backend refused, fails while it is still refused, and passes on
// a 404 once the key is gone.
func TestCBBackend_GetProbeRetriesTheRefusedKey(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	mock.getErr = &httpError{code: 403}
	cb := newTestCBBackend(mock, 1, time.Minute)
	_, _ = cb.GetObject(context.Background(), "refused", "")
	check := probeFor(t, cb, s3op.GetObject).Check

	if err := check(context.Background()); err == nil {
		t.Error("probe passed while GET is still refused")
	}
	if got := cb.calls[core.CallGetObject].probeTarget(); got != "refused" {
		t.Errorf("probe target = %q, want the refused key", got)
	}
	mock.mu.Lock()
	mock.getErr = &httpError{code: 404}
	mock.mu.Unlock()
	if err := check(context.Background()); err != nil {
		t.Errorf("probe on a 404 = %v, want it to pass", err)
	}
}

// TestCBBackend_PutProbeWritesTheProbeKey verifies the PUT probe writes
// probeKey and fails while uploads are refused.
func TestCBBackend_PutProbeWritesTheProbeKey(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	cb := newTestCBBackend(mock, 1, time.Minute)
	check := probeFor(t, cb, s3op.PutObject).Check

	if err := check(context.Background()); err != nil {
		t.Fatalf("probe = %v", err)
	}
	mock.mu.Lock()
	_, wrote := mock.objects[probeKey]
	mock.putErr = &httpError{code: 403}
	mock.mu.Unlock()
	if !wrote {
		t.Error("probe did not write probeKey")
	}
	if err := check(context.Background()); err == nil {
		t.Error("probe passed while uploads are refused")
	}
}

// TestCBBackend_CallProbesPassOnAHealthyBackend verifies every call probe
// passes against a backend that answers, with GET and HEAD asking for probeKey
// before any key was refused. The mock has no batch delete, so that probe
// reports it unsupported.
func TestCBBackend_CallProbesPassOnAHealthyBackend(t *testing.T) {
	t.Parallel()
	cb := newTestCBBackend(newMockBackend(), 1, time.Minute)
	for _, op := range []s3op.Operation{s3op.PutObject, s3op.GetObject, s3op.HeadObject, s3op.ListObjects, s3op.CopyObject, s3op.DeleteObject} {
		if err := probeFor(t, cb, op).Check(context.Background()); err != nil {
			t.Errorf("%s probe = %v, want it to pass", op, err)
		}
	}
	if err := probeFor(t, cb, s3op.DeleteObjects).Check(context.Background()); !errors.Is(err, ErrBatchDeleteNotSupported) {
		t.Errorf("DeleteObjects probe = %v, want ErrBatchDeleteNotSupported", err)
	}
}

// TestProbeResult verifies a probe passes on any answer its breaker would not
// count and fails on a refusal, an unreachable backend or a timeout.
func TestProbeResult(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		pass bool
	}{
		{"success", nil, true},
		{"404", &httpError{code: 404}, true},
		{"416", &httpError{code: 416}, true},
		{"403", &httpError{code: 403}, false},
		{"503", &httpError{code: 503}, false},
		{"unreachable", errors.New("connection refused"), false},
		{"deadline", context.DeadlineExceeded, false},
	} {
		if got := probeResult(tc.err) == nil; got != tc.pass {
			t.Errorf("%s: passed = %t, want %t", tc.name, got, tc.pass)
		}
	}
}

// httpError is a test helper that satisfies the HTTPStatusCode() interface.
type httpError struct {
	code int
	msg  string
}

// Error returns the error message.
func (e *httpError) Error() string { return e.msg }

// HTTPStatusCode satisfies the smithy http.ResponseError
// interface used by the awserr-style typed-error tests below.
func (e *httpError) HTTPStatusCode() int { return e.code }

// TestCBBackend_404DoesNotTripBreaker verifies the cbbackend 404 does not trip breaker contract.
// Asserts that expected success after 404s, got error:.
func TestCBBackend_404DoesNotTripBreaker(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	cb := newTestCBBackend(mock, 3, time.Minute)

	// 3 consecutive 404 errors should NOT trip the circuit breaker
	mock.getErr = &httpError{code: 404, msg: "NoSuchKey"}
	for range 3 {
		_, _ = cb.GetObject(context.Background(), "missing-key", "")
	}

	// Circuit should still be closed  -  next call should reach the backend
	mock.getErr = nil
	mock.objects = map[string]mockObject{"exists": {data: []byte("data")}}
	result, err := cb.GetObject(context.Background(), "exists", "")
	if err != nil {
		t.Fatalf("expected success after 404s, got error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result, got nil")
	}
	if !servesAll(cb) {
		t.Errorf("served = %s, want every call", cb.Served())
	}
}

// TestCBBackend_500DoesTripsBreaker verifies the cbbackend 500 does trips breaker contract.
// Asserts that expected circuit open after 500s, got.
func TestCBBackend_500DoesTripsBreaker(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	cb := newTestCBBackend(mock, 3, time.Minute)

	// 3 consecutive 500 errors SHOULD trip the circuit breaker
	mock.getErr = &httpError{code: 500, msg: "InternalServerError"}
	for range 3 {
		_, _ = cb.GetObject(context.Background(), "key", "")
	}

	if cb.Served().Has(core.CallGetObject) {
		t.Errorf("served after 500s = %s, want GET refused", cb.Served())
	}

	// Next call should return ErrBackendUnavailable without reaching backend
	_, err := cb.GetObject(context.Background(), "key", "")
	if !errors.Is(err, breaker.ErrBackendUnavailable) {
		t.Errorf("expected ErrBackendUnavailable, got %v", err)
	}
}

// TestClassifyFailure verifies which breaker a failure counts against: no
// status and 401 against the backend; 5xx, 429 and 403 against the call; any
// other status is a request-specific answer from a working backend and counts
// nowhere, and neither does a refusal from the wrapper's own breakers.
func TestClassifyFailure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want failureScope
	}{
		{"nil", nil, scopeNone},
		{"no status", errors.New("connection refused"), scopeBackend},
		{"401", &httpError{code: 401}, scopeBackend},
		{"500", &httpError{code: 500}, scopeCall},
		{"503 wrapped", fmt.Errorf("get object failed: %w", &httpError{code: 503}), scopeCall},
		{"429", &httpError{code: 429}, scopeCall},
		{"403", &httpError{code: 403}, scopeCall},
		{"400", &httpError{code: 400}, scopeNone},
		{"404", &httpError{code: 404}, scopeNone},
		{"409", &httpError{code: 409}, scopeNone},
		{"412", &httpError{code: 412}, scopeNone},
		{"416 wrapped", fmt.Errorf("get object failed: %w", &httpError{code: 416}), scopeNone},
		{"breaker refusal", fmt.Errorf("%w: %w", breaker.ErrBackendUnavailable, &httpError{code: 403}), scopeNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyFailure(tc.err); got != tc.want {
				t.Errorf("classifyFailure(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyFailure_ContextErrors verifies caller-side cancellation and
// deadline (including wrapped) never count toward a breaker: they signal a
// timeout/shutdown, not the backend's health, and counting them is what let a
// slow source cascade-trip healthy backends.
func TestClassifyFailure_ContextErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
	}{
		{"canceled", context.Canceled},
		{"deadline", context.DeadlineExceeded},
		{"wrapped canceled", fmt.Errorf("put object failed: %w", context.Canceled)},
		{"wrapped deadline", fmt.Errorf("write to target: %w", context.DeadlineExceeded)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyFailure(tc.err); got != scopeNone {
				t.Errorf("%v counts as %v, want none", tc.err, got)
			}
		})
	}
}
