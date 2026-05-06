// -------------------------------------------------------------------------------
// Circuit Breaker Tests
//
// Author: Alex Freidah
//
// Tests for database circuit breaker state transitions: closed to open on
// consecutive failures, half-open probe after timeout, and recovery back to
// closed. Validates degraded-mode read cache behavior during open state.
// -------------------------------------------------------------------------------

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/testutil/testx"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// testCBBundle exercises the shared *breaker.CircuitBreaker through every
// per-role CB wrapper. Tests call any method they need; all wrappers share
// the same breaker, so failures on one method transition the same circuit
// state seen by another. Production code never composes roles  -  this
// aggregation exists only to keep the circuit-breaker state-machine tests
// compact.
type testCBBundle struct {
	core.ObjectStore
	core.MultipartStore
	core.ReplicationStore
	core.CleanupStore
	core.PendingStore
	core.IntegrityStore
	core.ExpiredObjectsLister
	core.BackendLifecycleStore
	core.DashboardStore
	core.UsageFlusher
	core.AdvisoryLocker
	quota core.QuotaStore
	*breaker.CircuitBreaker
}

// GetBackendWithSpace forwards to the embedded QuotaStore  -  given by name
// to avoid ambiguous method promotion with DashboardStore.GetQuotaStats.
func (t *testCBBundle) GetBackendWithSpace(ctx context.Context, size int64, backendOrder []string) (string, error) {
	return t.quota.GetBackendWithSpace(ctx, size, backendOrder)
}

// GetLeastUtilizedBackend forwards to the embedded QuotaStore.
func (t *testCBBundle) GetLeastUtilizedBackend(ctx context.Context, size int64, eligible []string) (string, error) {
	return t.quota.GetLeastUtilizedBackend(ctx, size, eligible)
}

// newTestCB constructs a new test cb.
func newTestCB(mock *mockStore, threshold int, timeout time.Duration) *testCBBundle {
	_ = config.CircuitBreakerConfig{}
	cb := breaker.NewCircuitBreaker("database", threshold, timeout, isDBError, core.ErrDBUnavailable)
	return &testCBBundle{
		ObjectStore:           NewCBObjectStore(mock, cb),
		MultipartStore:        NewCBMultipartStore(mock, cb),
		ReplicationStore:      NewCBReplicationStore(mock, cb),
		CleanupStore:          NewCBCleanupStore(mock, cb),
		PendingStore:          NewCBPendingStore(mock, cb),
		IntegrityStore:        NewCBIntegrityStore(mock, cb),
		ExpiredObjectsLister:  NewCBExpiredObjectsLister(mock, cb),
		BackendLifecycleStore: NewCBBackendLifecycleStore(mock, cb),
		DashboardStore:        NewCBDashboardStore(mock, cb),
		UsageFlusher:          NewCBUsageFlusher(mock, cb),
		AdvisoryLocker:        NewAdvisoryLocker(mock),
		quota:                 NewCBQuotaStore(mock, cb),
		CircuitBreaker:        cb,
	}
}

// TestCircuitBreaker_ClosedPassesThrough verifies the circuit breaker closed passes through contract.
// Asserts that unexpected error:.
func TestCircuitBreaker_ClosedPassesThrough(t *testing.T) {
	t.Parallel()
	mock := &mockStore{
		getAllLocationsResp: []core.ObjectLocation{{ObjectKey: "test", BackendName: "b1"}},
	}
	cb := newTestCB(mock, 3, time.Minute)

	result, err := cb.GetAllObjectLocations(context.Background(), "key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 || result[0].BackendName != "b1" {
		t.Fatalf("unexpected result: %v", result)
	}
	if mock.callCount != 1 {
		t.Fatalf("expected 1 call, got %d", mock.callCount)
	}
}

// TestCircuitBreaker_OpensAfterThreshold verifies the circuit breaker opens after threshold contract.
// Asserts that call : expected dbErr, got.
func TestCircuitBreaker_OpensAfterThreshold(t *testing.T) {
	t.Parallel()
	dbErr := errors.New("connection refused")
	mock := &mockStore{getAllLocationsErr: dbErr}
	cb := newTestCB(mock, 3, time.Minute)

	ctx := context.Background()

	// First 2 calls should pass through and return the raw DB error
	for i := range 2 {
		_, err := cb.GetAllObjectLocations(ctx, "key")
		if err != dbErr {
			t.Fatalf("call %d: expected dbErr, got %v", i, err)
		}
	}

	// 3rd call trips the threshold  -  circuit opens, returns ErrDBUnavailable
	_, err := cb.GetAllObjectLocations(ctx, "key")
	if !errors.Is(err, core.ErrDBUnavailable) {
		t.Fatalf("call 2: expected ErrDBUnavailable, got %v", err)
	}
	if mock.callCount != 3 {
		t.Fatalf("expected 3 calls, got %d", mock.callCount)
	}

	// 4th call should return ErrDBUnavailable without hitting mock
	_, err = cb.GetAllObjectLocations(ctx, "key")
	if !errors.Is(err, core.ErrDBUnavailable) {
		t.Fatalf("expected ErrDBUnavailable, got %v", err)
	}
	if mock.callCount != 3 {
		t.Fatalf("expected mock not called again, got %d", mock.callCount)
	}
}

// TestCircuitBreaker_HalfOpenAfterTimeout verifies the circuit breaker half open after timeout contract.
// Asserts that expected ErrDBUnavailable, got.
func TestCircuitBreaker_HalfOpenAfterTimeout(t *testing.T) {
	t.Parallel()
	dbErr := errors.New("connection refused")
	mock := &mockStore{getAllLocationsErr: dbErr}
	cb := newTestCB(mock, 1, 10*time.Millisecond)

	ctx := context.Background()

	// Trip the circuit
	_, _ = cb.GetAllObjectLocations(ctx, "key")

	// Should be open
	_, err := cb.GetAllObjectLocations(ctx, "key")
	if !errors.Is(err, core.ErrDBUnavailable) {
		t.Fatalf("expected ErrDBUnavailable, got %v", err)
	}

	// Wait for timeout
	testx.Eventually(t, 500*time.Millisecond, cb.ProbeEligible, "circuit breaker never became probe-eligible")

	// Next call should probe (pass through to mock)
	mock.mu.Lock()
	mock.getAllLocationsErr = nil
	mock.getAllLocationsResp = []core.ObjectLocation{{ObjectKey: "test", BackendName: "b1"}}
	mock.mu.Unlock()

	result, err := cb.GetAllObjectLocations(ctx, "key")
	if err != nil {
		t.Fatalf("probe should succeed: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected result from probe, got %v", result)
	}

	// Circuit should be closed again
	if !cb.IsHealthy() {
		t.Fatal("expected circuit to be closed after successful probe")
	}
}

// TestCircuitBreaker_HalfOpenFailureReopens verifies the circuit breaker half open failure reopens contract.
// Asserts that expected ErrDBUnavailable on failed probe, got.
func TestCircuitBreaker_HalfOpenFailureReopens(t *testing.T) {
	t.Parallel()
	dbErr := errors.New("connection refused")
	mock := &mockStore{getAllLocationsErr: dbErr}
	cb := newTestCB(mock, 1, 10*time.Millisecond)

	ctx := context.Background()

	// Trip the circuit
	_, _ = cb.GetAllObjectLocations(ctx, "key")

	// Wait for timeout
	testx.Eventually(t, 500*time.Millisecond, cb.ProbeEligible, "circuit breaker never became probe-eligible")

	// Probe should fail  -  circuit reopens, returns ErrDBUnavailable
	_, err := cb.GetAllObjectLocations(ctx, "key")
	if !errors.Is(err, core.ErrDBUnavailable) {
		t.Fatalf("expected ErrDBUnavailable on failed probe, got %v", err)
	}

	// Circuit should be open again
	_, err = cb.GetAllObjectLocations(ctx, "key")
	if !errors.Is(err, core.ErrDBUnavailable) {
		t.Fatalf("expected ErrDBUnavailable after failed probe, got %v", err)
	}
}

// TestCircuitBreaker_AppErrorsDontTrip verifies the circuit breaker app errors dont trip contract.
// Asserts that expected ErrObjectNotFound, got.
func TestCircuitBreaker_AppErrorsDontTrip(t *testing.T) {
	t.Parallel()
	mock := &mockStore{getAllLocationsErr: core.ErrObjectNotFound}
	cb := newTestCB(mock, 1, time.Minute)

	ctx := context.Background()

	// Application errors should not trip the circuit
	for range 5 {
		_, err := cb.GetAllObjectLocations(ctx, "key")
		if !errors.Is(err, core.ErrObjectNotFound) {
			t.Fatalf("expected ErrObjectNotFound, got %v", err)
		}
	}

	// Circuit should still be closed
	if !cb.IsHealthy() {
		t.Fatal("circuit should remain closed for application errors")
	}
	if mock.callCount != 5 {
		t.Fatalf("all 5 calls should have passed through, got %d", mock.callCount)
	}
}

// TestCircuitBreaker_IsHealthy verifies the circuit breaker is healthy path by exercising errors.New, cb.IsHealthy, cb.GetAllObjectLocations.
func TestCircuitBreaker_IsHealthy(t *testing.T) {
	t.Parallel()
	mock := &mockStore{getAllLocationsErr: errors.New("down")}
	cb := newTestCB(mock, 1, time.Minute)

	if !cb.IsHealthy() {
		t.Fatal("should start healthy")
	}

	_, _ = cb.GetAllObjectLocations(context.Background(), "key")

	if cb.IsHealthy() {
		t.Fatal("should be unhealthy after tripping")
	}
}

// TestCircuitBreaker_SuccessResetsFailures verifies the circuit breaker success resets failures path by exercising context.Background, errors.New, cb.GetAllObjectLocations.
func TestCircuitBreaker_SuccessResetsFailures(t *testing.T) {
	t.Parallel()
	mock := &mockStore{
		getAllLocationsResp: []core.ObjectLocation{{ObjectKey: "test", BackendName: "b1"}},
	}
	cb := newTestCB(mock, 3, time.Minute)

	ctx := context.Background()
	dbErr := errors.New("temporary")

	// 2 failures (below threshold)
	mock.mu.Lock()
	mock.getAllLocationsErr = dbErr
	mock.mu.Unlock()
	_, _ = cb.GetAllObjectLocations(ctx, "key")
	_, _ = cb.GetAllObjectLocations(ctx, "key")

	// 1 success resets the counter
	mock.mu.Lock()
	mock.getAllLocationsErr = nil
	mock.mu.Unlock()
	_, _ = cb.GetAllObjectLocations(ctx, "key")

	// 2 more failures should not trip (counter was reset)
	mock.mu.Lock()
	mock.getAllLocationsErr = dbErr
	mock.mu.Unlock()
	_, _ = cb.GetAllObjectLocations(ctx, "key")
	_, _ = cb.GetAllObjectLocations(ctx, "key")

	if !cb.IsHealthy() {
		t.Fatal("circuit should still be closed after reset + 2 failures")
	}
}

// -------------------------------------------------------------------------
// breaker.State.String
// -------------------------------------------------------------------------

// TestCircuitState_String verifies the circuit state string contract.
// Asserts that breaker.State().String() = , want.
func TestCircuitState_String(t *testing.T) {
	t.Parallel()
	tests := []struct {
		state breaker.State
		want  string
	}{
		{breaker.StateClosed, "closed"},
		{breaker.StateOpen, "open"},
		{breaker.StateHalfOpen, "half-open"},
		{breaker.State(99), "unknown"},
	}
	for _, tt := range tests {
		got := tt.state.String()
		if got != tt.want {
			t.Errorf("breaker.State(%d).String() = %q, want %q", tt.state, got, tt.want)
		}
	}
}

// -------------------------------------------------------------------------
// isDBError
// -------------------------------------------------------------------------

// TestIsDBError_NilIsNotDBError verifies the is dberror nil is not dberror behaviour described by the test name.
func TestIsDBError_NilIsNotDBError(t *testing.T) {
	t.Parallel()
	if isDBError(nil) {
		t.Error("nil should not be a DB error")
	}
}

// TestIsDBError_S3ErrorIsNotDBError verifies the is dberror s3 error is not dberror behaviour described by the test name.
func TestIsDBError_S3ErrorIsNotDBError(t *testing.T) {
	t.Parallel()
	err := &core.S3Error{StatusCode: 404, Code: "NoSuchKey", Message: "not found"}
	if isDBError(err) {
		t.Error("S3Error should not be a DB error")
	}
}

// TestIsDBError_ErrNoSpaceIsNotDBError verifies the is dberror err no space is not dberror behaviour described by the test name.
func TestIsDBError_ErrNoSpaceIsNotDBError(t *testing.T) {
	t.Parallel()
	if isDBError(core.ErrNoSpaceAvailable) {
		t.Error("ErrNoSpaceAvailable should not be a DB error")
	}
}

// TestIsDBError_GenericErrorIsDBError verifies the is dberror generic error is dberror path by exercising errors.New.
func TestIsDBError_GenericErrorIsDBError(t *testing.T) {
	t.Parallel()
	if !isDBError(errors.New("connection refused")) {
		t.Error("generic error should be a DB error")
	}
}

// TestIsDBError_WrappedS3Error verifies the is dberror wrapped s3 error path by exercising fmt.Errorf.
func TestIsDBError_WrappedS3Error(t *testing.T) {
	t.Parallel()
	inner := &core.S3Error{StatusCode: 507, Code: "InsufficientStorage", Message: "full"}
	wrapped := fmt.Errorf("wrapped: %w", inner)
	if isDBError(wrapped) {
		t.Error("wrapped S3Error should not be a DB error")
	}
}

// -------------------------------------------------------------------------
// postCheck edge cases
// -------------------------------------------------------------------------

// TestCBObjectStore_ListObjectsByBackendKeyAsc_DelegatesAndReturnsRows
// covers the closed-circuit happy path: the call passes through to the
// inner store and returns its rows.
func TestCBObjectStore_ListObjectsByBackendKeyAsc_DelegatesAndReturnsRows(t *testing.T) {
	t.Parallel()
	want := []core.ObjectLocation{{ObjectKey: "vb/a", BackendName: "be1"}, {ObjectKey: "vb/b", BackendName: "be1"}}
	mock := &mockStore{listObjectsByBackendKeyAscResp: want}
	cb := newTestCB(mock, 3, time.Minute)

	got, err := cb.ListObjectsByBackendKeyAsc(context.Background(), "be1", "", 100)
	if err != nil {
		t.Fatalf("ListObjectsByBackendKeyAsc: %v", err)
	}
	if len(got) != 2 || got[0].ObjectKey != "vb/a" || got[1].ObjectKey != "vb/b" {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestCBObjectStore_ListObjectsByBackendKeyAsc_OpenCircuitReturnsSentinel
// trips the breaker, then verifies the next call returns ErrDBUnavailable
// without reaching the inner store.
func TestCBObjectStore_ListObjectsByBackendKeyAsc_OpenCircuitReturnsSentinel(t *testing.T) {
	t.Parallel()
	dbErr := errors.New("conn refused")
	mock := &mockStore{listObjectsByBackendKeyAscErr: dbErr}
	cb := newTestCB(mock, 1, time.Minute)

	// First call: real DB error trips the breaker.
	if _, err := cb.ListObjectsByBackendKeyAsc(context.Background(), "be1", "", 100); err == nil {
		t.Fatal("expected DB error on first call")
	}

	// Second call: circuit open -> sentinel.
	_, err := cb.ListObjectsByBackendKeyAsc(context.Background(), "be1", "", 100)
	if !errors.Is(err, core.ErrDBUnavailable) {
		t.Errorf("err = %v, want ErrDBUnavailable", err)
	}
}

// TestCircuitBreaker_PostCheck_NonDBErrorPassesThrough verifies the circuit breaker post check non dberror passes through contract.
// Asserts that expected ErrObjectNotFound passthrough, got.
func TestCircuitBreaker_PostCheck_NonDBErrorPassesThrough(t *testing.T) {
	t.Parallel()
	mock := &mockStore{getAllLocationsErr: core.ErrObjectNotFound}
	cb := newTestCB(mock, 3, time.Minute)

	// ErrObjectNotFound is not a DB error, so it should pass through unchanged
	_, err := cb.GetAllObjectLocations(context.Background(), "key")
	if !errors.Is(err, core.ErrObjectNotFound) {
		t.Fatalf("expected ErrObjectNotFound passthrough, got %v", err)
	}

	// Should still be healthy
	if !cb.IsHealthy() {
		t.Fatal("circuit should remain closed for non-DB errors")
	}
}

// TestCircuitBreaker_PostCheck_DBErrorBelowThresholdPassesRawError verifies the circuit breaker post check dberror below threshold passes raw error contract.
// Asserts that expected raw DB error below threshold, got.
func TestCircuitBreaker_PostCheck_DBErrorBelowThresholdPassesRawError(t *testing.T) {
	t.Parallel()
	dbErr := errors.New("connection refused")
	mock := &mockStore{getAllLocationsErr: dbErr}
	cb := newTestCB(mock, 3, time.Minute)

	// First failure (below threshold): raw error returned, circuit stays closed
	_, err := cb.GetAllObjectLocations(context.Background(), "key")
	if err != dbErr {
		t.Fatalf("expected raw DB error below threshold, got %v", err)
	}
	if !cb.IsHealthy() {
		t.Fatal("circuit should remain closed below threshold")
	}
}

// -------------------------------------------------------------------------
// cbCallNoResult
// -------------------------------------------------------------------------

// TestCircuitBreaker_RecordObject_Success verifies the circuit breaker record object success contract.
// Asserts that RecordObject:.
func TestCircuitBreaker_RecordObject_Success(t *testing.T) {
	t.Parallel()
	mock := &mockStore{}
	cb := newTestCB(mock, 3, time.Minute)

	_, err := cb.RecordObject(context.Background(), "key", "b1", 100, nil)
	if err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
}

// TestCircuitBreaker_RecordObject_CircuitOpen verifies the circuit breaker record object circuit open contract.
// Asserts that expected ErrDBUnavailable, got.
func TestCircuitBreaker_RecordObject_CircuitOpen(t *testing.T) {
	t.Parallel()
	dbErr := errors.New("connection refused")
	mock := &mockStore{getAllLocationsErr: dbErr, recordObjectErr: dbErr}
	cb := newTestCB(mock, 1, time.Minute)

	// Trip the circuit
	_, _ = cb.GetAllObjectLocations(context.Background(), "key")

	// RecordObject should return ErrDBUnavailable
	_, err := cb.RecordObject(context.Background(), "key", "b1", 100, nil)
	if !errors.Is(err, core.ErrDBUnavailable) {
		t.Fatalf("expected ErrDBUnavailable, got %v", err)
	}
}

// -------------------------------------------------------------------------
// CreateMultipartUpload
// -------------------------------------------------------------------------

// TestCircuitBreaker_CreateMultipartUpload_Success verifies the circuit breaker create multipart upload success contract.
// Asserts that CreateMultipartUpload:.
func TestCircuitBreaker_CreateMultipartUpload_Success(t *testing.T) {
	t.Parallel()
	mock := &mockStore{}
	cb := newTestCB(mock, 3, time.Minute)

	err := cb.CreateMultipartUpload(context.Background(), &core.CreateMultipartUploadParams{
		UploadID:    "upload-1",
		ObjectKey:   "key",
		BackendName: "b1",
		ContentType: "application/octet-stream",
		Metadata:    map[string]string{"project": "acme"},
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
}

// TestCircuitBreaker_CreateMultipartUpload_CircuitOpen verifies the circuit breaker create multipart upload circuit open contract.
// Asserts that expected ErrDBUnavailable, got.
func TestCircuitBreaker_CreateMultipartUpload_CircuitOpen(t *testing.T) {
	t.Parallel()
	dbErr := errors.New("connection refused")
	mock := &mockStore{getAllLocationsErr: dbErr}
	cb := newTestCB(mock, 1, time.Minute)

	// Trip the circuit
	_, _ = cb.GetAllObjectLocations(context.Background(), "key")

	err := cb.CreateMultipartUpload(context.Background(), &core.CreateMultipartUploadParams{
		UploadID:    "upload-1",
		ObjectKey:   "key",
		BackendName: "b1",
		ContentType: "application/octet-stream",
	})
	if !errors.Is(err, core.ErrDBUnavailable) {
		t.Fatalf("expected ErrDBUnavailable, got %v", err)
	}
}

// -------------------------------------------------------------------------
// ListMultipartUploads
// -------------------------------------------------------------------------

// TestCircuitBreaker_ListMultipartUploads_Success verifies the circuit breaker list multipart uploads success contract.
// Asserts that ListMultipartUploads:.
func TestCircuitBreaker_ListMultipartUploads_Success(t *testing.T) {
	t.Parallel()
	mock := &mockStore{}
	cb := newTestCB(mock, 3, time.Minute)

	uploads, err := cb.ListMultipartUploads(context.Background(), "prefix/", 100)
	if err != nil {
		t.Fatalf("ListMultipartUploads: %v", err)
	}
	if uploads != nil {
		t.Fatalf("expected nil uploads from empty mock, got %v", uploads)
	}
}

// TestCircuitBreaker_ListMultipartUploads_CircuitOpen verifies the circuit breaker list multipart uploads circuit open contract.
// Asserts that expected ErrDBUnavailable, got.
func TestCircuitBreaker_ListMultipartUploads_CircuitOpen(t *testing.T) {
	t.Parallel()
	dbErr := errors.New("connection refused")
	mock := &mockStore{getAllLocationsErr: dbErr}
	cb := newTestCB(mock, 1, time.Minute)

	// Trip the circuit
	_, _ = cb.GetAllObjectLocations(context.Background(), "key")

	_, err := cb.ListMultipartUploads(context.Background(), "prefix/", 100)
	if !errors.Is(err, core.ErrDBUnavailable) {
		t.Fatalf("expected ErrDBUnavailable, got %v", err)
	}
}

// -------------------------------------------------------------------------
// WithAdvisoryLock (bypasses circuit breaker)
// -------------------------------------------------------------------------

// TestCircuitBreaker_WithAdvisoryLock_BypassesCircuit verifies the circuit breaker with advisory lock bypasses circuit contract.
// Asserts that WithAdvisoryLock:.
func TestCircuitBreaker_WithAdvisoryLock_BypassesCircuit(t *testing.T) {
	t.Parallel()
	dbErr := errors.New("connection refused")
	mock := &mockStore{getAllLocationsErr: dbErr}
	cb := newTestCB(mock, 1, time.Minute)

	// Trip the circuit
	_, _ = cb.GetAllObjectLocations(context.Background(), "key")
	if cb.IsHealthy() {
		t.Fatal("circuit should be open")
	}

	// WithAdvisoryLock should still work (bypasses circuit)
	called := false
	acquired, err := cb.WithAdvisoryLock(context.Background(), 1, func(ctx context.Context) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("WithAdvisoryLock: %v", err)
	}
	if !acquired {
		t.Fatal("expected lock to be acquired")
	}
	if !called {
		t.Fatal("expected fn to be called")
	}
}

// captureLogs redirects slog output to a buffer for the duration of f,
// then restores the original default logger.
func captureLogs(f func()) string {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)
	f()
	return buf.String()
}

// TestCircuitBreaker_TransitionLogs_ClosedToOpen verifies the circuit breaker transition logs closed to open contract.
// Asserts that closed->open log missing \nlog output:.
func TestCircuitBreaker_TransitionLogs_ClosedToOpen(t *testing.T) {
	dbErr := errors.New("connection refused")
	mock := &mockStore{getAllLocationsErr: dbErr}
	cb := newTestCB(mock, 2, time.Minute)

	ctx := context.Background()

	output := captureLogs(func() {
		_, _ = cb.GetAllObjectLocations(ctx, "key") // failure 1
		_, _ = cb.GetAllObjectLocations(ctx, "key") // failure 2, trips circuit
	})

	for _, want := range []string{
		"failure threshold reached",
		"name=database",
		"from=closed",
		"to=open",
		"failures=2",
		"threshold=2",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("closed->open log missing %q\nlog output: %s", want, output)
		}
	}
}

// TestCircuitBreaker_TransitionLogs_OpenToHalfOpen verifies the circuit breaker transition logs open to half open contract.
// Asserts that open->half-open log missing \nlog output:.
func TestCircuitBreaker_TransitionLogs_OpenToHalfOpen(t *testing.T) {
	dbErr := errors.New("connection refused")
	mock := &mockStore{getAllLocationsErr: dbErr}
	cb := newTestCB(mock, 1, 10*time.Millisecond)

	ctx := context.Background()
	_, _ = cb.GetAllObjectLocations(ctx, "key") // trip circuit
	testx.Eventually(t, 500*time.Millisecond, cb.ProbeEligible, "circuit breaker never became probe-eligible")

	output := captureLogs(func() {
		_, _ = cb.GetAllObjectLocations(ctx, "key") // triggers half-open probe
	})

	for _, want := range []string{
		"probing",
		"name=database",
		"from=open",
		"to=half-open",
		"open_duration=",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("open->half-open log missing %q\nlog output: %s", want, output)
		}
	}
}

// Not t.Parallel(): captureLogs mutates slog.SetDefault (process-global),
// which races with other parallel tests emitting slog calls. The three
// sibling TransitionLogs_* tests are all serial for the same reason.
// TestCircuitBreaker_TransitionLogs_HalfOpenToClosed verifies circuit breaker_transition logs_half open to closed.
// TestCircuitBreaker_TransitionLogs_HalfOpenToClosed verifies circuit breaker_transition logs_half open to closed.
func TestCircuitBreaker_TransitionLogs_HalfOpenToClosed(t *testing.T) {
	dbErr := errors.New("connection refused")
	mock := &mockStore{getAllLocationsErr: dbErr}
	cb := newTestCB(mock, 1, 10*time.Millisecond)

	ctx := context.Background()
	_, _ = cb.GetAllObjectLocations(ctx, "key") // trip circuit
	testx.Eventually(t, 500*time.Millisecond, cb.ProbeEligible, "circuit breaker never became probe-eligible")

	// Fix the mock so the probe succeeds
	mock.mu.Lock()
	mock.getAllLocationsErr = nil
	mock.getAllLocationsResp = []core.ObjectLocation{{ObjectKey: "test", BackendName: "b1"}}
	mock.mu.Unlock()

	output := captureLogs(func() {
		_, _ = cb.GetAllObjectLocations(ctx, "key") // probe succeeds, closes circuit
	})

	for _, want := range []string{
		"recovered",
		"name=database",
		"from=half-open",
		"to=closed",
		"degraded_duration=",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("half-open->closed log missing %q\nlog output: %s", want, output)
		}
	}
}

// TestCircuitBreaker_TransitionLogs_HalfOpenToOpen verifies the circuit breaker transition logs half open to open contract.
// Asserts that half-open->open log missing \nlog output:.
func TestCircuitBreaker_TransitionLogs_HalfOpenToOpen(t *testing.T) {
	dbErr := errors.New("connection refused")
	mock := &mockStore{getAllLocationsErr: dbErr}
	cb := newTestCB(mock, 1, 10*time.Millisecond)

	ctx := context.Background()
	_, _ = cb.GetAllObjectLocations(ctx, "key") // trip circuit
	testx.Eventually(t, 500*time.Millisecond, cb.ProbeEligible, "circuit breaker never became probe-eligible")

	output := captureLogs(func() {
		_, _ = cb.GetAllObjectLocations(ctx, "key") // probe fails, reopens
	})

	for _, want := range []string{
		"probe failed",
		"name=database",
		"from=half-open",
		"to=open",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("half-open->open log missing %q\nlog output: %s", want, output)
		}
	}
}

// TestCircuitBreaker_DegradedDurationIsPositive verifies the circuit breaker degraded duration is positive contract.
// Asserts that state = , want open after threshold breach.
func TestCircuitBreaker_DegradedDurationIsPositive(t *testing.T) {
	t.Parallel()
	dbErr := errors.New("connection refused")
	mock := &mockStore{getAllLocationsErr: dbErr}
	const openTimeout = 10 * time.Millisecond
	cb := newTestCB(mock, 1, openTimeout)

	ctx := context.Background()
	_, _ = cb.GetAllObjectLocations(ctx, "key") // trip circuit

	// Verify the trip happened. State must be open synchronously with the
	// failed call returning  -  that is what guarantees openedAt was set
	// inside transition()'s critical section. Asserting OpenDuration != 0
	// here would be flaky: time.Since on a monotonic clock that resolves
	// both reads to the same nanosecond returns 0.
	if cb.State() != breaker.StateOpen {
		t.Fatalf("state = %v, want open after threshold breach", cb.State())
	}

	testx.Eventually(t, 500*time.Millisecond, cb.ProbeEligible, "circuit breaker never became probe-eligible")

	// By the time the breaker is probe-eligible, real wall-clock time has
	// provably elapsed beyond openTimeout. OpenDuration must therefore be
	// strictly positive. The upper bound catches the bug a fragile
	// equality-with-zero check actually misses: if openedAt were ever the
	// zero value, time.Since(zero) would return roughly two millennia.
	if d := cb.OpenDuration(); d < openTimeout || d > time.Second {
		t.Fatalf("OpenDuration = %v, want between %v and 1s", d, openTimeout)
	}

	// Fix mock, recover
	mock.mu.Lock()
	mock.getAllLocationsErr = nil
	mock.getAllLocationsResp = []core.ObjectLocation{{ObjectKey: "test", BackendName: "b1"}}
	mock.mu.Unlock()

	_, _ = cb.GetAllObjectLocations(ctx, "key") // probe + close

	if !cb.IsHealthy() {
		t.Fatal("circuit should be closed after recovery")
	}
}

// -------------------------------------------------------------------------
// Forwarding method coverage  -  exercises untested CBCall/CBCallNoResult
// wrappers to ensure each method delegates correctly through the circuit.
// -------------------------------------------------------------------------

// TestCircuitBreaker_ForwardingMethods_Closed exercises every CBCall and
// CBCallNoResult wrapper through a closed circuit and asserts each one
// delegates to the underlying store without surfacing an error. Split
// into per-method-family helpers so cognitive complexity stays low.
func TestCircuitBreaker_ForwardingMethods_Closed(t *testing.T) {
	t.Parallel()
	mock := &mockStore{
		getQuotaStatsResp:        map[string]core.QuotaStat{"b1": {BackendName: "b1", BytesUsed: 100}},
		getObjectCountsResp:      map[string]int64{"b1": 42},
		getActiveMultipartResp:   map[string]int64{"b1": 5},
		getUsageForPeriodResp:    map[string]core.UsageStat{"b1": {APIRequests: 10}},
		listObjectsByBackendResp: []core.ObjectLocation{{ObjectKey: "k", BackendName: "b1"}},
		getUnderReplicatedResp:   []core.ObjectLocation{{ObjectKey: "k", BackendName: "b1"}},
		recordReplicaInserted:    true,
		cleanupQueueDepthVal:     7,
		listDirChildrenResp:      &core.DirectoryListResult{},
	}
	cb := newTestCB(mock, 3, time.Minute)
	ctx := context.Background()

	checkCBCallObjectMethods(t, cb, ctx)
	checkCBCallQuotaMethods(t, cb, ctx)
	checkCBCallMultipartReadMethods(t, cb, ctx)
	checkCBCallReplicaMethods(t, cb, ctx)
	checkCBCallCleanupReadMethods(t, cb, ctx)
	checkCBCallNoResultMethods(t, cb, ctx)
}

// checkCBCallObjectMethods covers the object/listing CBCall wrappers.
func checkCBCallObjectMethods(t *testing.T, cb *testCBBundle, ctx context.Context) {
	t.Helper()
	if _, err := cb.DeleteObject(ctx, "key"); err != nil {
		t.Errorf("DeleteObject: %v", err)
	}
	if _, err := cb.ListObjects(ctx, "p/", "", 100); err != nil {
		t.Errorf("ListObjects: %v", err)
	}
	if _, err := cb.ListDirectoryChildren(ctx, "p/", "", 100); err != nil {
		t.Errorf("ListDirectoryChildren: %v", err)
	}
	if _, err := cb.ListExpiredObjects(ctx, "", time.Now(), 100); err != nil {
		t.Errorf("ListExpiredObjects: %v", err)
	}
	if _, err := cb.ListObjectsByBackend(ctx, "b1", 100); err != nil {
		t.Errorf("ListObjectsByBackend: %v", err)
	}
	if _, err := cb.MoveObjectLocation(ctx, "k", "b1", "b2"); err != nil {
		t.Errorf("MoveObjectLocation: %v", err)
	}
	if _, err := cb.ImportObject(ctx, "k", "b1", 100); err != nil {
		t.Errorf("ImportObject: %v", err)
	}
}

// checkCBCallQuotaMethods covers the backend-selection and stat CBCall
// wrappers.
func checkCBCallQuotaMethods(t *testing.T, cb *testCBBundle, ctx context.Context) {
	t.Helper()
	if _, err := cb.GetBackendWithSpace(ctx, 100, []string{"b1"}); err != nil {
		t.Errorf("GetBackendWithSpace: %v", err)
	}
	if _, err := cb.GetLeastUtilizedBackend(ctx, 100, []string{"b1"}); err != nil {
		t.Errorf("GetLeastUtilizedBackend: %v", err)
	}
	if _, err := cb.GetQuotaStats(ctx); err != nil {
		t.Errorf("GetQuotaStats: %v", err)
	}
	if _, err := cb.GetObjectCounts(ctx); err != nil {
		t.Errorf("GetObjectCounts: %v", err)
	}
	if _, err := cb.GetActiveMultipartCounts(ctx); err != nil {
		t.Errorf("GetActiveMultipartCounts: %v", err)
	}
	if _, err := cb.GetUsageForPeriod(ctx, "2024-01"); err != nil {
		t.Errorf("GetUsageForPeriod: %v", err)
	}
}

// checkCBCallMultipartReadMethods covers the multipart-read CBCall
// wrappers.
func checkCBCallMultipartReadMethods(t *testing.T, cb *testCBBundle, ctx context.Context) {
	t.Helper()
	if _, err := cb.GetMultipartUpload(ctx, "u1"); err != nil {
		t.Errorf("GetMultipartUpload: %v", err)
	}
	if _, err := cb.GetParts(ctx, "u1"); err != nil {
		t.Errorf("GetParts: %v", err)
	}
	if _, err := cb.GetStaleMultipartUploads(ctx, time.Hour); err != nil {
		t.Errorf("GetStaleMultipartUploads: %v", err)
	}
	if _, err := cb.ListMultipartUploads(ctx, "", 100); err != nil {
		t.Errorf("ListMultipartUploads: %v", err)
	}
}

// checkCBCallReplicaMethods covers the replication CBCall wrappers.
func checkCBCallReplicaMethods(t *testing.T, cb *testCBBundle, ctx context.Context) {
	t.Helper()
	if _, err := cb.GetUnderReplicatedObjects(ctx, 2, 100); err != nil {
		t.Errorf("GetUnderReplicatedObjects: %v", err)
	}
	if _, _, err := cb.RecordReplica(ctx, "k", "b2", "b1"); err != nil {
		t.Errorf("RecordReplica: %v", err)
	}
}

// checkCBCallCleanupReadMethods covers the cleanup-queue read CBCall
// wrappers.
func checkCBCallCleanupReadMethods(t *testing.T, cb *testCBBundle, ctx context.Context) {
	t.Helper()
	if _, err := cb.GetPendingCleanups(ctx, 10); err != nil {
		t.Errorf("GetPendingCleanups: %v", err)
	}
	if _, err := cb.CleanupQueueDepth(ctx); err != nil {
		t.Errorf("CleanupQueueDepth: %v", err)
	}
	if _, err := cb.CleanupDLQDepth(ctx); err != nil {
		t.Errorf("CleanupDLQDepth: %v", err)
	}
	if _, err := cb.MoveCleanupToDLQ(ctx, 1, "test"); err != nil {
		t.Errorf("MoveCleanupToDLQ: %v", err)
	}
}

// checkCBCallNoResultMethods covers the void-returning wrappers that go
// through CBCallNoResult.
func checkCBCallNoResultMethods(t *testing.T, cb *testCBBundle, ctx context.Context) {
	t.Helper()
	if err := cb.DeleteMultipartUpload(ctx, "u1"); err != nil {
		t.Errorf("DeleteMultipartUpload: %v", err)
	}
	if err := cb.RecordPart(ctx, "u1", 1, "etag", 100, nil); err != nil {
		t.Errorf("RecordPart: %v", err)
	}
	if err := cb.FlushUsageDeltas(ctx, "b1", "2024-01", 1, 2, 3); err != nil {
		t.Errorf("FlushUsageDeltas: %v", err)
	}
	if err := cb.EnqueueCleanup(ctx, "b1", "k", "test", 1024); err != nil {
		t.Errorf("EnqueueCleanup: %v", err)
	}
	if err := cb.CompleteCleanupItem(ctx, 1); err != nil {
		t.Errorf("CompleteCleanupItem: %v", err)
	}
	if err := cb.RetryCleanupItem(ctx, 1, time.Minute, "err"); err != nil {
		t.Errorf("RetryCleanupItem: %v", err)
	}
	if err := cb.DeleteBackendData(ctx, "b1"); err != nil {
		t.Errorf("DeleteBackendData: %v", err)
	}
	if err := cb.DeleteObjectLocation(ctx, "k", "b1"); err != nil {
		t.Errorf("DeleteObjectLocation: %v", err)
	}
	if _, err := cb.SweepStaleCleanupQueueRows(ctx, "k", "b1"); err != nil {
		t.Errorf("SweepStaleCleanupQueueRows: %v", err)
	}
}

// TestCircuitBreaker_ForwardingMethods_Open verifies the circuit breaker forwarding methods open contract.
// Asserts that GetQuotaStats: got , want ErrDBUnavailable.
func TestCircuitBreaker_ForwardingMethods_Open(t *testing.T) {
	t.Parallel()
	dbErr := errors.New("connection refused")
	mock := &mockStore{getAllLocationsErr: dbErr}
	cb := newTestCB(mock, 1, time.Minute)
	ctx := context.Background()

	// Trip the circuit
	_, _ = cb.GetAllObjectLocations(ctx, "key")

	// Verify a sample of forwarding methods return ErrDBUnavailable
	if _, err := cb.GetQuotaStats(ctx); !errors.Is(err, core.ErrDBUnavailable) {
		t.Errorf("GetQuotaStats: got %v, want ErrDBUnavailable", err)
	}
	if _, err := cb.ListObjectsByBackend(ctx, "b1", 100); !errors.Is(err, core.ErrDBUnavailable) {
		t.Errorf("ListObjectsByBackend: got %v, want ErrDBUnavailable", err)
	}
	if err := cb.FlushUsageDeltas(ctx, "b1", "2024-01", 1, 2, 3); !errors.Is(err, core.ErrDBUnavailable) {
		t.Errorf("FlushUsageDeltas: got %v, want ErrDBUnavailable", err)
	}
	if err := cb.DeleteBackendData(ctx, "b1"); !errors.Is(err, core.ErrDBUnavailable) {
		t.Errorf("DeleteBackendData: got %v, want ErrDBUnavailable", err)
	}
	if _, err := cb.SweepStaleCleanupQueueRows(ctx, "k", "b1"); !errors.Is(err, core.ErrDBUnavailable) {
		t.Errorf("SweepStaleCleanupQueueRows: got %v, want ErrDBUnavailable", err)
	}
}

// TestCircuitBreaker_BackendObjectStats_PreCheckBlocks verifies the circuit breaker backend object stats pre check blocks contract.
// Asserts that BackendObjectStats: got , want ErrDBUnavailable.
func TestCircuitBreaker_BackendObjectStats_PreCheckBlocks(t *testing.T) {
	t.Parallel()
	dbErr := errors.New("connection refused")
	mock := &mockStore{getAllLocationsErr: dbErr}
	cb := newTestCB(mock, 1, time.Minute)
	ctx := context.Background()

	// Trip the circuit
	_, _ = cb.GetAllObjectLocations(ctx, "key")

	count, bytes, err := cb.BackendObjectStats(ctx, "b1")
	if !errors.Is(err, core.ErrDBUnavailable) {
		t.Errorf("BackendObjectStats: got %v, want ErrDBUnavailable", err)
	}
	if count != 0 || bytes != 0 {
		t.Errorf("expected zero values, got count=%d bytes=%d", count, bytes)
	}
}

// TestCircuitBreaker_BackendObjectStats_Success verifies the circuit breaker backend object stats success contract.
// Asserts that BackendObjectStats:.
func TestCircuitBreaker_BackendObjectStats_Success(t *testing.T) {
	t.Parallel()
	mock := &mockStore{}
	cb := newTestCB(mock, 3, time.Minute)

	count, bytes, err := cb.BackendObjectStats(context.Background(), "b1")
	if err != nil {
		t.Fatalf("BackendObjectStats: %v", err)
	}
	if count != 0 || bytes != 0 {
		t.Errorf("expected zero values from mock, got count=%d bytes=%d", count, bytes)
	}
}

// TestCircuitBreaker_ConcurrentProbeSerializedByAtomicFlag verifies the circuit breaker concurrent probe serialized by atomic flag contract.
// Asserts that mock received calls during burst, want exactly 1 probe.
func TestCircuitBreaker_ConcurrentProbeSerializedByAtomicFlag(t *testing.T) {
	t.Parallel()
	dbErr := errors.New("connection refused")
	mock := &mockStore{getAllLocationsErr: dbErr}
	cb := newTestCB(mock, 1, 10*time.Millisecond)

	ctx := context.Background()

	// Trip the circuit (1 call to mock)
	_, _ = cb.GetAllObjectLocations(ctx, "key")

	// Wait for timeout so probe is eligible
	testx.Eventually(t, 500*time.Millisecond, cb.ProbeEligible, "circuit breaker never became probe-eligible")

	// Record call count before the burst
	mock.mu.Lock()
	callsBefore := mock.callCount
	mock.mu.Unlock()

	// Fire many concurrent requests  -  exactly one should reach the mock (the probe)
	const goroutines = 50
	var wg sync.WaitGroup

	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			_, _ = cb.GetAllObjectLocations(ctx, "key")
		}()
	}
	wg.Wait()

	mock.mu.Lock()
	callsAfter := mock.callCount
	mock.mu.Unlock()

	// Exactly one probe should have reached the real store
	probes := callsAfter - callsBefore
	if probes != 1 {
		t.Errorf("mock received %d calls during burst, want exactly 1 probe", probes)
	}
}

// -------------------------------------------------------------------------
// Forwarder coverage  -  each narrow CB wrapper's thin pass-through methods.
//
// These tests don't exercise state-machine edges (the big tests above do);
// they prove each forwarder returns the inner store's error/value so the
// per-role CB wrappers can't quietly drift out of sync with the role
// interface contract.
// -------------------------------------------------------------------------

// TestCBForwarders_CleanupStore verifies the cbforwarders cleanup store contract.
// Asserts that IncrementOrphanBytes:.
func TestCBForwarders_CleanupStore(t *testing.T) {
	t.Parallel()
	cb := newTestCB(&mockStore{}, 3, time.Minute)
	ctx := context.Background()
	if err := cb.IncrementOrphanBytes(ctx, "b1", 10); err != nil {
		t.Errorf("IncrementOrphanBytes: %v", err)
	}
	if err := cb.DecrementOrphanBytes(ctx, "b1", 10); err != nil {
		t.Errorf("DecrementOrphanBytes: %v", err)
	}
}

// TestCBForwarders_IntegrityStore verifies the cbforwarders integrity store contract.
// Asserts that GetRandomHashedObjects:.
func TestCBForwarders_IntegrityStore(t *testing.T) {
	t.Parallel()
	cb := newTestCB(&mockStore{}, 3, time.Minute)
	ctx := context.Background()
	if _, err := cb.GetRandomHashedObjects(ctx, 5); err != nil {
		t.Errorf("GetRandomHashedObjects: %v", err)
	}
	if _, err := cb.GetObjectsWithoutHash(ctx, 5, 0); err != nil {
		t.Errorf("GetObjectsWithoutHash: %v", err)
	}
	if err := cb.UpdateContentHash(ctx, "k", "b1", "h"); err != nil {
		t.Errorf("UpdateContentHash: %v", err)
	}
}

// TestCBForwarders_MultipartStore verifies the cbforwarders multipart store contract.
// Asserts that CountActiveMultipartUploads:.
func TestCBForwarders_MultipartStore(t *testing.T) {
	t.Parallel()
	cb := newTestCB(&mockStore{}, 3, time.Minute)
	ctx := context.Background()
	if _, err := cb.CountActiveMultipartUploads(ctx, "prefix/"); err != nil {
		t.Errorf("CountActiveMultipartUploads: %v", err)
	}
	if _, err := cb.GetMultipartUploadsByBackend(ctx, "b1"); err != nil {
		t.Errorf("GetMultipartUploadsByBackend: %v", err)
	}
}

// TestCBForwarders_QuotaStore verifies the cbforwarders quota store contract.
// Asserts that GetQuotaStats:.
func TestCBForwarders_QuotaStore(t *testing.T) {
	t.Parallel()
	mock := &mockStore{getQuotaStatsResp: map[string]core.QuotaStat{"b1": {BytesUsed: 10}}}
	cb := newTestCB(mock, 3, time.Minute)

	// Exercise QuotaStore.GetQuotaStats directly. cb.GetQuotaStats would
	// resolve to the embedded DashboardStore's forwarder  -  the one on
	// cbQuotaStore needs its own hit to count toward coverage.
	stats, err := cb.quota.GetQuotaStats(context.Background())
	if err != nil {
		t.Fatalf("GetQuotaStats: %v", err)
	}
	if stats["b1"].BytesUsed != 10 {
		t.Errorf("GetQuotaStats: got %+v", stats)
	}
}

// TestCBForwarders_ReplicationStore verifies the cbforwarders replication store contract.
// Asserts that GetUnderReplicatedObjectsExcluding:.
func TestCBForwarders_ReplicationStore(t *testing.T) {
	t.Parallel()
	cb := newTestCB(&mockStore{}, 3, time.Minute)
	ctx := context.Background()
	if _, err := cb.GetUnderReplicatedObjectsExcluding(ctx, 2, 10, []string{"b1"}); err != nil {
		t.Errorf("GetUnderReplicatedObjectsExcluding: %v", err)
	}
	if _, err := cb.GetOverReplicatedObjects(ctx, 2, 10); err != nil {
		t.Errorf("GetOverReplicatedObjects: %v", err)
	}
	if _, err := cb.CountOverReplicatedObjects(ctx, 2); err != nil {
		t.Errorf("CountOverReplicatedObjects: %v", err)
	}
	if err := cb.RemoveExcessCopy(ctx, "k", "b1", 100); err != nil {
		t.Errorf("RemoveExcessCopy: %v", err)
	}
}

// -------------------------------------------------------------------------
// Database breaker factory + error filter wrappers
// -------------------------------------------------------------------------

// TestNewDatabaseBreaker_ReturnsHealthy verifies the new database breaker returns healthy path by exercising cb.IsHealthy.
func TestNewDatabaseBreaker_ReturnsHealthy(t *testing.T) {
	t.Parallel()
	cb := NewDatabaseBreaker(config.CircuitBreakerConfig{FailureThreshold: 3, OpenTimeout: time.Second})
	if cb == nil {
		t.Fatal("NewDatabaseBreaker returned nil")
	}
	if !cb.IsHealthy() {
		t.Error("freshly-constructed breaker should be healthy")
	}
}

// TestIsDBError_WrappedErrNoSpace verifies the is dberror wrapped err no space path by exercising fmt.Errorf.
func TestIsDBError_WrappedErrNoSpace(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("outer: %w", core.ErrNoSpaceAvailable)
	if isDBError(wrapped) {
		t.Error("wrapped ErrNoSpaceAvailable should not be a DB error")
	}
}

// TestCircuitBreaker_ListLegacyMultipartUploads exercises the CB
// wrapper added in #650 so the breaker-protected forward through
// the inner store is counted in coverage.
func TestCircuitBreaker_ListLegacyMultipartUploads(t *testing.T) {
	t.Parallel()
	mock := &mockStore{}
	cb := newTestCB(mock, 3, time.Minute)
	if _, err := cb.ListLegacyMultipartUploads(context.Background(), 50); err != nil {
		t.Fatalf("ListLegacyMultipartUploads: %v", err)
	}
}

// TestCircuitBreaker_UpdateUploadEncryption exercises the CB
// wrapper for the upload-row encryption stamp.
func TestCircuitBreaker_UpdateUploadEncryption(t *testing.T) {
	t.Parallel()
	mock := &mockStore{}
	cb := newTestCB(mock, 3, time.Minute)
	if err := cb.UpdateUploadEncryption(context.Background(), "u1", []byte("k"), "kid"); err != nil {
		t.Fatalf("UpdateUploadEncryption: %v", err)
	}
}

// TestCircuitBreaker_UpdatePartEncryption exercises the CB
// wrapper for the part-row encryption stamp.
func TestCircuitBreaker_UpdatePartEncryption(t *testing.T) {
	t.Parallel()
	mock := &mockStore{}
	cb := newTestCB(mock, 3, time.Minute)
	enc := &core.EncryptionMeta{Encrypted: true, EncryptionKey: []byte("k"), KeyID: "kid"}
	if err := cb.UpdatePartEncryption(context.Background(), "u1", 1, 100, enc); err != nil {
		t.Fatalf("UpdatePartEncryption: %v", err)
	}
}
