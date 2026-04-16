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
	"github.com/afreidah/s3-orchestrator/internal/store"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/mock/gomock"
)

func TestReplicator_SetConfig_RoundTrip(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	r := NewReplicator(NewMockOps(ctrl))
	if r.Config() != nil {
		t.Fatal("expected nil config before set")
	}
	cfg := &config.ReplicationConfig{Factor: 3}
	r.SetConfig(cfg)
	if got := r.Config(); got.Factor != 3 {
		t.Errorf("Config().Factor = %d, want 3", got.Factor)
	}
}

func TestFindReplicaTarget_SelectsBackendWithSpace(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)

	exclusion := map[string]bool{"b1": true}
	ops.EXPECT().SelectReplicaTarget(gomock.Any(), int64(50), exclusion).Return("b2", nil)

	r := NewReplicator(ops)
	stats := map[string]store.QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000},
		"b2": {BytesUsed: 100, BytesLimit: 1000},
	}

	target := r.FindReplicaTarget(context.Background(), stats, "key1", 50, exclusion)
	if target != "b2" {
		t.Errorf("FindReplicaTarget = %q, want b2", target)
	}
}

func TestFindReplicaTarget_NoSpace(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)

	ops.EXPECT().SelectReplicaTarget(gomock.Any(), int64(50), gomock.Any()).Return("", nil)

	r := NewReplicator(ops)
	stats := map[string]store.QuotaStat{
		"b1": {BytesUsed: 990, BytesLimit: 1000},
	}

	target := r.FindReplicaTarget(context.Background(), stats, "key1", 50, nil)
	if target != "" {
		t.Errorf("FindReplicaTarget = %q, want empty (no space)", target)
	}
}

func TestFindReplicaTarget_SelectionError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)

	ops.EXPECT().SelectReplicaTarget(gomock.Any(), int64(50), gomock.Any()).
		Return("", fmt.Errorf("database unavailable"))

	r := NewReplicator(ops)

	target := r.FindReplicaTarget(context.Background(), nil, "key1", 50, nil)
	if target != "" {
		t.Errorf("FindReplicaTarget = %q, want empty (error)", target)
	}
}

func TestCopyToReplica_Success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().GetBackend("b2").Return(dstBe, nil)
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe}).AnyTimes()
	ops.EXPECT().StreamCopy(gomock.Any(), srcBe, dstBe, "key1").Return(nil)

	r := NewReplicator(ops)
	copies := []store.ObjectLocation{{BackendName: "b1"}}

	src, err := r.CopyToReplica(context.Background(), "key1", copies, "b2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src != "b1" {
		t.Errorf("source = %q, want b1", src)
	}
}

func TestCopyToReplica_404CleansUpStaleMetadata(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)
	ms := &mockMetadataStore{}

	ops.EXPECT().GetBackend("b2").Return(dstBe, nil)
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe}).AnyTimes()
	ops.EXPECT().StreamCopy(gomock.Any(), srcBe, dstBe, "key1").
		Return(fmt.Errorf("read: %w", &httpError{code: 404, msg: "NoSuchKey"}))
	ops.EXPECT().Store().Return(ms).AnyTimes()

	r := NewReplicator(ops)
	copies := []store.ObjectLocation{{BackendName: "b1"}}

	_, err := r.CopyToReplica(context.Background(), "key1", copies, "b2")
	if err == nil {
		t.Fatal("expected error when source returns 404")
	}
	if ms.staleDeleted != 1 {
		t.Errorf("staleDeleted = %d, want 1", ms.staleDeleted)
	}
}

func TestCopyToReplica_AllSourcesFail(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().GetBackend("b2").Return(dstBe, nil)
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe}).AnyTimes()
	ops.EXPECT().StreamCopy(gomock.Any(), srcBe, dstBe, "key1").Return(fmt.Errorf("read: timeout"))

	r := NewReplicator(ops)
	copies := []store.ObjectLocation{{BackendName: "b1"}}

	_, err := r.CopyToReplica(context.Background(), "key1", copies, "b2")
	if err == nil {
		t.Fatal("expected error when all sources fail")
	}
}

func TestCleanupOrphan_DelegatesToDeleteOrEnqueue(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	be := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": be})
	ops.EXPECT().DeleteOrEnqueue(gomock.Any(), be, "b1", "key1", "replication_orphan", int64(100))
	ops.EXPECT().Usage().Return(newTestUsageTracker())

	r := NewReplicator(ops)
	r.CleanupOrphan(context.Background(), "b1", "key1", 100)
}

func TestReplicate_FactorOne_Noop(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	r := NewReplicator(NewMockOps(ctrl))

	created, err := r.Replicate(context.Background(), config.ReplicationConfig{Factor: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if created != 0 {
		t.Errorf("created = %d, want 0", created)
	}
}

func TestReplicate_NothingUnderReplicated(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	ms := &mockMetadataStore{}

	ops.EXPECT().Store().Return(ms).AnyTimes()
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{}).AnyTimes()

	r := NewReplicator(ops)
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

func TestReplicateObject_Success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ms := &mockMetadataStore{recordReplicaOK: true}

	ops.EXPECT().Store().Return(ms).AnyTimes()
	ops.EXPECT().SelectReplicaTarget(gomock.Any(), int64(50), gomock.Any()).Return("b2", nil)
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe, "b2": dstBe}).AnyTimes()
	ops.EXPECT().GetBackend("b2").Return(dstBe, nil)
	ops.EXPECT().StreamCopy(gomock.Any(), srcBe, dstBe, "key1").Return(nil)
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()

	r := NewReplicator(ops)
	copies := []store.ObjectLocation{{BackendName: "b1", SizeBytes: 50}}
	stats := map[string]store.QuotaStat{
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

func TestReplicateObject_WriteFailureExcludesTarget(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	srcBe := backendtest.NewMockObjectBackend(ctrl)
	failBe := backendtest.NewMockObjectBackend(ctrl)
	okBe := backendtest.NewMockObjectBackend(ctrl)

	ms := &mockMetadataStore{recordReplicaOK: true}

	ops.EXPECT().Store().Return(ms).AnyTimes()
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

	r := NewReplicator(ops)
	copies := []store.ObjectLocation{{BackendName: "src", SizeBytes: 50}}
	stats := map[string]store.QuotaStat{
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

func TestReplicateObject_RecordReplicaErrorExcludesTarget(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	srcBe := backendtest.NewMockObjectBackend(ctrl)
	failBe := backendtest.NewMockObjectBackend(ctrl)

	ms := &mockMetadataStore{recordReplicaErr: errors.New("db down")}

	ops.EXPECT().Store().Return(ms).AnyTimes()
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

	r := NewReplicator(ops)
	copies := []store.ObjectLocation{{BackendName: "src", SizeBytes: 50}}
	stats := map[string]store.QuotaStat{}

	created, err := r.ReplicateObject(context.Background(), stats, "key1", copies, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if created != 0 {
		t.Errorf("created = %d, want 0", created)
	}
}

func TestReplicateObject_NotInsertedExcludesTarget(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	srcBe := backendtest.NewMockObjectBackend(ctrl)
	staleBe := backendtest.NewMockObjectBackend(ctrl)

	// recordReplicaOK=false simulates source deleted during replication
	ms := &mockMetadataStore{recordReplicaOK: false}

	ops.EXPECT().Store().Return(ms).AnyTimes()
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

	r := NewReplicator(ops)
	copies := []store.ObjectLocation{{BackendName: "src", SizeBytes: 50}}
	stats := map[string]store.QuotaStat{}

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

func (e *httpError) Error() string       { return e.msg }
func (e *httpError) HTTPStatusCode() int { return e.code }

func TestIsNotFound_404(t *testing.T) {
	t.Parallel()
	if !isNotFound(&httpError{code: 404, msg: "NoSuchKey"}) {
		t.Error("expected true for 404")
	}
}

func TestIsNotFound_500(t *testing.T) {
	t.Parallel()
	if isNotFound(&httpError{code: 500, msg: "InternalServerError"}) {
		t.Error("expected false for 500")
	}
}

func TestIsNotFound_PlainError(t *testing.T) {
	t.Parallel()
	if isNotFound(errors.New("connection refused")) {
		t.Error("expected false for plain error")
	}
}

func TestIsNotFound_Wrapped404(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("read: %w", &httpError{code: 404, msg: "NoSuchKey"})
	if !isNotFound(wrapped) {
		t.Error("expected true for wrapped 404")
	}
}

func TestIsNotFound_Nil(t *testing.T) {
	t.Parallel()
	if isNotFound(nil) {
		t.Error("expected false for nil")
	}
}
