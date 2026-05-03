// -------------------------------------------------------------------------------
// Replicator Tests
//
// Author: Alex Freidah
//
// Covers replication target selection (backend with space, no-space
// case, store-error case), the source-to-replica copy primitive, the
// quota-conscious size accounting that uses the size returned by the
// conditional INSERT, and config round-trip. Pins the contract that
// CopyToReplica returns the actual source size so the caller can
// keep object_locations.size_bytes and backend_quotas.bytes_used in
// sync without reading the row twice.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/mock/gomock"
)

// TestReplicator_SetConfig_RoundTrip verifies the replicator set config round trip contract.
// Asserts that Config().Factor = , want 3.
func TestReplicator_SetConfig_RoundTrip(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	r := NewReplicator(NewMockOps(ctrl), &mockMetadataStore{})
	if r.Config() != nil {
		t.Fatal("expected nil config before set")
	}
	cfg := &config.ReplicationConfig{Factor: 3}
	r.SetConfig(cfg)
	if got := r.Config(); got.Factor != 3 {
		t.Errorf("Config().Factor = %d, want 3", got.Factor)
	}
}

// TestFindReplicaTarget_SelectsBackendWithSpace verifies the find replica target selects backend with space contract.
// Asserts that FindReplicaTarget = , want b2.
func TestFindReplicaTarget_SelectsBackendWithSpace(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	ms := &mockMetadataStore{}

	exclusion := map[string]bool{"b1": true}
	ops.EXPECT().SelectReplicaTarget(gomock.Any(), int64(50), exclusion).Return("b2", nil)

	r := NewReplicator(ops, ms)
	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000},
		"b2": {BytesUsed: 100, BytesLimit: 1000},
	}

	target := r.FindReplicaTarget(context.Background(), stats, "key1", 50, exclusion)
	if target != "b2" {
		t.Errorf("FindReplicaTarget = %q, want b2", target)
	}
}

// TestFindReplicaTarget_NoSpace verifies the find replica target no space contract.
// Asserts that FindReplicaTarget = , want empty (no space).
func TestFindReplicaTarget_NoSpace(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	ms := &mockMetadataStore{}

	ops.EXPECT().SelectReplicaTarget(gomock.Any(), int64(50), gomock.Any()).Return("", nil)

	r := NewReplicator(ops, ms)
	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 990, BytesLimit: 1000},
	}

	target := r.FindReplicaTarget(context.Background(), stats, "key1", 50, nil)
	if target != "" {
		t.Errorf("FindReplicaTarget = %q, want empty (no space)", target)
	}
}

// TestFindReplicaTarget_SelectionError verifies the find replica target selection error contract.
// Asserts that FindReplicaTarget = , want empty (error).
func TestFindReplicaTarget_SelectionError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	ms := &mockMetadataStore{}

	ops.EXPECT().SelectReplicaTarget(gomock.Any(), int64(50), gomock.Any()).
		Return("", fmt.Errorf("database unavailable"))

	r := NewReplicator(ops, ms)

	target := r.FindReplicaTarget(context.Background(), nil, "key1", 50, nil)
	if target != "" {
		t.Errorf("FindReplicaTarget = %q, want empty (error)", target)
	}
}

// TestCopyToReplica_Success verifies the copy to replica success contract.
// Asserts that unexpected error:.
func TestCopyToReplica_Success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	ms := &mockMetadataStore{}

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().GetBackend("b2").Return(dstBe, nil)
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe}).AnyTimes()
	ops.EXPECT().StreamCopy(gomock.Any(), srcBe, dstBe, "key1").Return(nil)

	r := NewReplicator(ops, ms)
	copies := []core.ObjectLocation{{BackendName: "b1", SizeBytes: 4096}}

	src, size, err := r.CopyToReplica(context.Background(), "key1", copies, "b2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src != "b1" {
		t.Errorf("source = %q, want b1", src)
	}
	if size != 4096 {
		t.Errorf("size = %d, want 4096 (the source's SizeBytes)", size)
	}
}

// TestCopyToReplica_404CleansUpStaleMetadata verifies the copy to replica 404 cleans up stale metadata contract.
// Asserts that staleDeleted = , want 1.
func TestCopyToReplica_404CleansUpStaleMetadata(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	ms := &mockMetadataStore{}

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().GetBackend("b2").Return(dstBe, nil)
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe}).AnyTimes()
	ops.EXPECT().StreamCopy(gomock.Any(), srcBe, dstBe, "key1").
		Return(fmt.Errorf("read: %w", &httpError{code: 404, msg: "NoSuchKey"}))

	r := NewReplicator(ops, ms)
	copies := []core.ObjectLocation{{BackendName: "b1"}}

	_, _, err := r.CopyToReplica(context.Background(), "key1", copies, "b2")
	if err == nil {
		t.Fatal("expected error when source returns 404")
	}
	if ms.staleDeleted != 1 {
		t.Errorf("staleDeleted = %d, want 1", ms.staleDeleted)
	}
}

// TestCopyToReplica_AllSourcesFail verifies the copy to replica all sources fail path by exercising gomock.NewController, backendtest.NewMockObjectBackend, ops.EXPECT.
func TestCopyToReplica_AllSourcesFail(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	ms := &mockMetadataStore{}

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().GetBackend("b2").Return(dstBe, nil)
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe}).AnyTimes()
	ops.EXPECT().StreamCopy(gomock.Any(), srcBe, dstBe, "key1").Return(fmt.Errorf("read: timeout"))

	r := NewReplicator(ops, ms)
	copies := []core.ObjectLocation{{BackendName: "b1"}}

	_, _, err := r.CopyToReplica(context.Background(), "key1", copies, "b2")
	if err == nil {
		t.Fatal("expected error when all sources fail")
	}
}

// TestCleanupOrphan_DelegatesToDeleteOrEnqueue verifies the cleanup orphan delegates to delete or enqueue path by exercising gomock.NewController, backendtest.NewMockObjectBackend, ops.EXPECT.
func TestCleanupOrphan_DelegatesToDeleteOrEnqueue(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	ms := &mockMetadataStore{}

	be := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": be})
	ops.EXPECT().DeleteOrEnqueue(gomock.Any(), be, "b1", "key1", "replication_orphan", int64(100))
	ops.EXPECT().Usage().Return(newTestUsageTracker())

	r := NewReplicator(ops, ms)
	r.CleanupOrphan(context.Background(), "b1", "key1", 100)
}

// TestReplicate_FactorOne_Noop verifies the replicate factor one noop contract.
// Asserts that unexpected error:.
func TestReplicate_FactorOne_Noop(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	r := NewReplicator(NewMockOps(ctrl), &mockMetadataStore{})

	created, err := r.Replicate(context.Background(), config.ReplicationConfig{Factor: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if created != 0 {
		t.Errorf("created = %d, want 0", created)
	}
}

// TestReplicate_NothingUnderReplicated verifies the replicate nothing under replicated contract.
// Asserts that unexpected error:.
func TestReplicate_NothingUnderReplicated(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	ms := &mockMetadataStore{}

	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{}).AnyTimes()

	r := NewReplicator(ops, ms)
	cfg := config.ReplicationConfig{Factor: 2, BatchSize: 10, Concurrency: 1, UnhealthyThreshold: time.Hour}
	created, err := r.Replicate(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if created != 0 {
		t.Errorf("created = %d, want 0", created)
	}
	if p := promtest.ToFloat64(telemetry.ReplicationPending); p != 0 {
		t.Errorf("ReplicationPending = %v, want 0", p)
	}
}

// TestReplicateObject_Success verifies the replicate object success contract.
// Asserts that unexpected error:.
func TestReplicateObject_Success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ms := &mockMetadataStore{recordReplicaOK: true}

	ops.EXPECT().SelectReplicaTarget(gomock.Any(), int64(50), gomock.Any()).Return("b2", nil)
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe, "b2": dstBe}).AnyTimes()
	ops.EXPECT().GetBackend("b2").Return(dstBe, nil)
	ops.EXPECT().StreamCopy(gomock.Any(), srcBe, dstBe, "key1").Return(nil)
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()

	r := NewReplicator(ops, ms)
	copies := []core.ObjectLocation{{BackendName: "b1", SizeBytes: 50}}
	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 100, BytesLimit: 1000},
		"b2": {BytesUsed: 100, BytesLimit: 1000},
	}

	created, err := r.ReplicateObject(context.Background(), stats, "key1", copies, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if created != 1 {
		t.Errorf("created = %d, want 1", created)
	}
	if ms.replicaRecorded != 1 {
		t.Errorf("replicaRecorded = %d, want 1", ms.replicaRecorded)
	}
}

// TestReplicateObject_WriteFailureExcludesTarget verifies the replicate object write failure excludes target contract.
// Asserts that unexpected error:.
func TestReplicateObject_WriteFailureExcludesTarget(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	failBe := backendtest.NewMockObjectBackend(ctrl)
	okBe := backendtest.NewMockObjectBackend(ctrl)

	ms := &mockMetadataStore{recordReplicaOK: true}

	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{
		"src": srcBe, "fail": failBe, "ok": okBe,
	}).AnyTimes()

	// First call selects "fail", second (with "fail" excluded) selects "ok",
	// third finds no remaining targets and returns "" to end the loop.
	gomock.InOrder(
		ops.EXPECT().SelectReplicaTarget(gomock.Any(), int64(50), gomock.Any()).
			DoAndReturn(func(_ context.Context, _ int64, excl map[string]bool) (string, error) {
				if excl["fail"] {
					t.Fatal("first call should not exclude 'fail'")
				}
				return "fail", nil
			}),
		ops.EXPECT().SelectReplicaTarget(gomock.Any(), int64(50), gomock.Any()).
			DoAndReturn(func(_ context.Context, _ int64, excl map[string]bool) (string, error) {
				if !excl["fail"] {
					t.Fatal("second call must exclude 'fail' after write failure")
				}
				return "ok", nil
			}),
		ops.EXPECT().SelectReplicaTarget(gomock.Any(), int64(50), gomock.Any()).
			Return("", nil),
	)

	// "fail" backend: GetBackend succeeds, StreamCopy returns write error
	ops.EXPECT().GetBackend("fail").Return(failBe, nil)
	ops.EXPECT().StreamCopy(gomock.Any(), srcBe, failBe, "key1").
		Return(fmt.Errorf("write: put object failed: 413 EntityTooLarge"))

	// "ok" backend: succeeds
	ops.EXPECT().GetBackend("ok").Return(okBe, nil)
	ops.EXPECT().StreamCopy(gomock.Any(), srcBe, okBe, "key1").Return(nil)

	r := NewReplicator(ops, ms)
	copies := []core.ObjectLocation{{BackendName: "src", SizeBytes: 50}}
	stats := map[string]core.QuotaStat{
		"src":  {BytesUsed: 100, BytesLimit: 1000},
		"fail": {BytesUsed: 100, BytesLimit: 1000},
		"ok":   {BytesUsed: 100, BytesLimit: 1000},
	}

	// Request 2 replicas: first attempt hits "fail" and should fall through
	// to "ok" on the second iteration with "fail" excluded.
	created, err := r.ReplicateObject(context.Background(), stats, "key1", copies, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if created != 1 {
		t.Errorf("created = %d, want 1", created)
	}
}

// TestReplicateObject_RecordReplicaErrorExcludesTarget verifies the replicate object record replica error excludes target contract.
// Asserts that unexpected error:.
func TestReplicateObject_RecordReplicaErrorExcludesTarget(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	failBe := backendtest.NewMockObjectBackend(ctrl)

	ms := &mockMetadataStore{recordReplicaErr: errors.New("db down")}

	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{
		"src": srcBe, "fail": failBe,
	}).AnyTimes()

	gomock.InOrder(
		ops.EXPECT().SelectReplicaTarget(gomock.Any(), int64(50), gomock.Any()).Return("fail", nil),
		// After RecordReplica error, "fail" is excluded; no targets left.
		ops.EXPECT().SelectReplicaTarget(gomock.Any(), int64(50), gomock.Any()).
			DoAndReturn(func(_ context.Context, _ int64, excl map[string]bool) (string, error) {
				if !excl["fail"] {
					t.Fatal("must exclude 'fail' after RecordReplica error")
				}
				return "", nil
			}),
	)

	ops.EXPECT().GetBackend("fail").Return(failBe, nil)
	ops.EXPECT().StreamCopy(gomock.Any(), srcBe, failBe, "key1").Return(nil)
	// CleanupOrphan path
	ops.EXPECT().DeleteOrEnqueue(gomock.Any(), failBe, "fail", "key1", "replication_orphan", int64(50))

	r := NewReplicator(ops, ms)
	copies := []core.ObjectLocation{{BackendName: "src", SizeBytes: 50}}
	stats := map[string]core.QuotaStat{}

	created, err := r.ReplicateObject(context.Background(), stats, "key1", copies, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if created != 0 {
		t.Errorf("created = %d, want 0", created)
	}
}

// TestReplicateObject_NotInsertedExcludesTarget verifies the replicate object not inserted excludes target contract.
// Asserts that unexpected error:.
func TestReplicateObject_NotInsertedExcludesTarget(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	staleBe := backendtest.NewMockObjectBackend(ctrl)

	// recordReplicaOK=false simulates source deleted during replication
	ms := &mockMetadataStore{recordReplicaOK: false}

	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{
		"src": srcBe, "stale": staleBe,
	}).AnyTimes()

	gomock.InOrder(
		ops.EXPECT().SelectReplicaTarget(gomock.Any(), int64(50), gomock.Any()).Return("stale", nil),
		// After !inserted, "stale" is excluded; no targets left.
		ops.EXPECT().SelectReplicaTarget(gomock.Any(), int64(50), gomock.Any()).
			DoAndReturn(func(_ context.Context, _ int64, excl map[string]bool) (string, error) {
				if !excl["stale"] {
					t.Fatal("must exclude 'stale' after !inserted")
				}
				return "", nil
			}),
	)

	ops.EXPECT().GetBackend("stale").Return(staleBe, nil)
	ops.EXPECT().StreamCopy(gomock.Any(), srcBe, staleBe, "key1").Return(nil)
	// CleanupOrphan path
	ops.EXPECT().DeleteOrEnqueue(gomock.Any(), staleBe, "stale", "key1", "replication_orphan", int64(50))

	r := NewReplicator(ops, ms)
	copies := []core.ObjectLocation{{BackendName: "src", SizeBytes: 50}}
	stats := map[string]core.QuotaStat{}

	created, err := r.ReplicateObject(context.Background(), stats, "key1", copies, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if created != 0 {
		t.Errorf("created = %d, want 0", created)
	}
}

// -------------------------------------------------------------------------
// isNotFound
// -------------------------------------------------------------------------

// httpError is a test helper that satisfies the HTTPStatusCode() interface.
type httpError struct {
	code int
	msg  string
}

// Error returns the error message.
func (e *httpError) Error() string       { return e.msg }
// HTTPStatusCode satisfies smithy/awserr's status-code accessor so
// the replicator's isNotFound-style typed-error tests can detect a
// 404 from this fake without depending on the real AWS SDK error
// hierarchy.
func (e *httpError) HTTPStatusCode() int { return e.code }

// TestIsNotFound_404 verifies the is not found 404 behaviour described by the test name.
func TestIsNotFound_404(t *testing.T) {
	t.Parallel()
	if !isNotFound(&httpError{code: 404, msg: "NoSuchKey"}) {
		t.Error("expected true for 404")
	}
}

// TestIsNotFound_500 verifies the is not found 500 behaviour described by the test name.
func TestIsNotFound_500(t *testing.T) {
	t.Parallel()
	if isNotFound(&httpError{code: 500, msg: "InternalServerError"}) {
		t.Error("expected false for 500")
	}
}

// TestIsNotFound_PlainError verifies the is not found plain error path by exercising errors.New.
func TestIsNotFound_PlainError(t *testing.T) {
	t.Parallel()
	if isNotFound(errors.New("connection refused")) {
		t.Error("expected false for plain error")
	}
}

// TestIsNotFound_Wrapped404 verifies the is not found wrapped404 path by exercising fmt.Errorf.
func TestIsNotFound_Wrapped404(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("read: %w", &httpError{code: 404, msg: "NoSuchKey"})
	if !isNotFound(wrapped) {
		t.Error("expected true for wrapped 404")
	}
}

// TestIsNotFound_Nil verifies the is not found nil behaviour described by the test name.
func TestIsNotFound_Nil(t *testing.T) {
	t.Parallel()
	if isNotFound(nil) {
		t.Error("expected false for nil")
	}
}
