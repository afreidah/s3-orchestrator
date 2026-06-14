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

// -------------------------------------------------------------------------
// planUnderReplicated (planner)
// -------------------------------------------------------------------------

// TestPlanUnderReplicated_TablePinsPlacementDecisions exercises the
// placement-policy half of replication without any backend I/O. The
// planner takes a flat object_locations slice + the configured
// replication factor and decides which keys still need work and how
// many extra copies they need. Tasks for keys that already meet or
// exceed the factor must be filtered out; the per-task `needed` count
// must equal `factor - len(existing copies)`.
func TestPlanUnderReplicated_TablePinsPlacementDecisions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		locations []core.ObjectLocation
		factor    int
		wantTasks map[string]int // key -> needed
	}{
		{
			name:      "empty input yields no tasks",
			locations: nil,
			factor:    3,
			wantTasks: map[string]int{},
		},
		{
			name: "single-copy key with factor 2 needs one more copy",
			locations: []core.ObjectLocation{
				{ObjectKey: "k1", BackendName: "b1", SizeBytes: 100},
			},
			factor:    2,
			wantTasks: map[string]int{"k1": 1},
		},
		{
			name: "key already at factor produces no task",
			locations: []core.ObjectLocation{
				{ObjectKey: "k1", BackendName: "b1", SizeBytes: 100},
				{ObjectKey: "k1", BackendName: "b2", SizeBytes: 100},
				{ObjectKey: "k1", BackendName: "b3", SizeBytes: 100},
			},
			factor:    3,
			wantTasks: map[string]int{},
		},
		{
			name: "key above factor is also filtered",
			locations: []core.ObjectLocation{
				{ObjectKey: "k1", BackendName: "b1"},
				{ObjectKey: "k1", BackendName: "b2"},
				{ObjectKey: "k1", BackendName: "b3"},
			},
			factor:    2,
			wantTasks: map[string]int{},
		},
		{
			name: "mixed under and at-factor keys",
			locations: []core.ObjectLocation{
				{ObjectKey: "needs2", BackendName: "b1"},
				{ObjectKey: "needs1", BackendName: "b1"},
				{ObjectKey: "needs1", BackendName: "b2"},
				{ObjectKey: "satisfied", BackendName: "b1"},
				{ObjectKey: "satisfied", BackendName: "b2"},
				{ObjectKey: "satisfied", BackendName: "b3"},
			},
			factor:    3,
			wantTasks: map[string]int{"needs2": 2, "needs1": 1},
		},
		{
			name: "factor 1 yields no tasks even for single-copy keys",
			locations: []core.ObjectLocation{
				{ObjectKey: "k1", BackendName: "b1"},
			},
			factor:    1,
			wantTasks: map[string]int{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tasks := planUnderReplicated(tc.locations, tc.factor)
			if len(tasks) != len(tc.wantTasks) {
				t.Fatalf("tasks = %d, want %d (%+v)", len(tasks), len(tc.wantTasks), tasks)
			}
			for _, task := range tasks {
				want, ok := tc.wantTasks[task.key]
				if !ok {
					t.Errorf("unexpected task for key %q", task.key)
					continue
				}
				if task.needed != want {
					t.Errorf("task %q needed = %d, want %d", task.key, task.needed, want)
				}
			}
		})
	}
}

// TestPlanUnderReplicated_PreservesCopyMembership confirms that the
// per-key copy slice handed to the executor includes every backend the
// flat locations slice listed for that key. The executor relies on this
// to populate the exclusion map without re-querying the store.
func TestPlanUnderReplicated_PreservesCopyMembership(t *testing.T) {
	t.Parallel()
	locations := []core.ObjectLocation{
		{ObjectKey: "k1", BackendName: "b1", SizeBytes: 100},
		{ObjectKey: "k1", BackendName: "b2", SizeBytes: 100},
	}
	tasks := planUnderReplicated(locations, 3)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	got := make(map[string]bool)
	for _, c := range tasks[0].copies {
		got[c.BackendName] = true
	}
	if !got["b1"] || !got["b2"] {
		t.Errorf("copies missing a backend: %+v", got)
	}
}

// -------------------------------------------------------------------------
// ReplicationOutcome / reportObjectOutcome
// -------------------------------------------------------------------------

// TestReplicationOutcome_FailedCountsAllFailureKinds pins the contract
// that Failed() aggregates every "this attempt did not produce a
// recorded copy" reason. NoTarget is *not* a failed-attempt counter so
// it sits outside the sum.
func TestReplicationOutcome_FailedCountsAllFailureKinds(t *testing.T) {
	t.Parallel()
	o := ReplicationOutcome{CopyErrors: 2, RecordErrors: 1, Superseded: 3}
	if got := o.Failed(); got != 6 {
		t.Errorf("Failed() = %d, want 6", got)
	}
	o = ReplicationOutcome{Created: 5}
	if got := o.Failed(); got != 0 {
		t.Errorf("Failed() = %d on clean outcome, want 0", got)
	}
}

// TestReplicator_SetConfig_RoundTrip verifies the replicator set config round trip contract.
// Asserts that Config().Factor = , want 3.
func TestReplicator_SetConfig_RoundTrip(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	r := NewReplicator(NewMockOps(ctrl), NewMockPlacement(ctrl), &mockMetadataStore{})
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
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	exclusion := map[string]bool{"b1": true}
	pl.EXPECT().SelectReplicaTarget(gomock.Any(), int64(50), exclusion).Return("b2", nil)

	r := NewReplicator(ops, pl, ms)

	target := r.FindReplicaTarget(context.Background(), "key1", 50, exclusion)
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
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	pl.EXPECT().SelectReplicaTarget(gomock.Any(), int64(50), gomock.Any()).Return("", nil)

	r := NewReplicator(ops, pl, ms)

	target := r.FindReplicaTarget(context.Background(), "key1", 50, nil)
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
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	pl.EXPECT().SelectReplicaTarget(gomock.Any(), int64(50), gomock.Any()).
		Return("", fmt.Errorf("database unavailable"))

	r := NewReplicator(ops, pl, ms)

	target := r.FindReplicaTarget(context.Background(), "key1", 50, nil)
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
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().GetBackend("b2").Return(dstBe, nil)
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe}).AnyTimes()
	ops.EXPECT().StreamCopy(gomock.Any(), srcBe, dstBe, "key1").Return(nil)

	r := NewReplicator(ops, pl, ms)
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

// newOpenBreakerBackend wraps a mock whose calls fail and trips its breaker
// open (threshold 1), so IsBackendHealthy reports it unhealthy. Used to prove
// CopyToReplica excludes dead sources from selection.
func newOpenBreakerBackend(t *testing.T, ctrl *gomock.Controller, name string) *backend.CircuitBreakerBackend {
	t.Helper()
	inner := backendtest.NewMockObjectBackend(ctrl)
	inner.EXPECT().GetObject(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, errors.New("billing account closed")).AnyTimes()
	cb := backend.NewCircuitBreakerBackend(inner, backend.CircuitBreakerConfig{Name: name, Threshold: 1, Timeout: time.Hour})
	if _, err := cb.GetObject(context.Background(), "trip", ""); err == nil {
		t.Fatal("expected the priming call to fail")
	}
	if cb.IsHealthy() {
		t.Fatal("expected breaker to be open after a failure")
	}
	return cb
}

// TestCopyToReplica_SkipsOpenBreakerSource verifies a source whose breaker is
// open is excluded from selection: the copy starts against the healthy source
// only, so a dead source can never stall a copy and trip the target.
func TestCopyToReplica_SkipsOpenBreakerSource(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	deadSrc := newOpenBreakerBackend(t, ctrl, "b0")
	healthySrc := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().GetBackend("b2").Return(dstBe, nil)
	ops.EXPECT().Backends().
		Return(map[string]backend.ObjectBackend{"b0": deadSrc, "b1": healthySrc}).AnyTimes()
	// Only the healthy source may be streamed; an unexpected StreamCopy against
	// deadSrc fails the test (no expectation registered for it).
	ops.EXPECT().StreamCopy(gomock.Any(), healthySrc, dstBe, "key1").Return(nil)

	r := NewReplicator(ops, pl, ms)
	// Dead source listed first to prove exclusion, not just ordering, is at work.
	copies := []core.ObjectLocation{{BackendName: "b0", SizeBytes: 10}, {BackendName: "b1", SizeBytes: 4096}}

	src, size, err := r.CopyToReplica(context.Background(), "key1", copies, "b2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src != "b1" || size != 4096 {
		t.Errorf("got (%q, %d), want (b1, 4096)", src, size)
	}
}

// TestCopyToReplica_FallsBackWhenAllSourcesUnhealthy verifies an object whose
// only copy sits on an open-breaker backend still gets a copy attempt rather
// than being stranded.
func TestCopyToReplica_FallsBackWhenAllSourcesUnhealthy(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	deadSrc := newOpenBreakerBackend(t, ctrl, "b0")
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().GetBackend("b2").Return(dstBe, nil)
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b0": deadSrc}).AnyTimes()
	// With no healthy source, the fallback must still attempt the dead one.
	ops.EXPECT().StreamCopy(gomock.Any(), deadSrc, dstBe, "key1").Return(nil)

	r := NewReplicator(ops, pl, ms)
	copies := []core.ObjectLocation{{BackendName: "b0", SizeBytes: 7}}

	src, _, err := r.CopyToReplica(context.Background(), "key1", copies, "b2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src != "b0" {
		t.Errorf("source = %q, want b0 (fallback attempt)", src)
	}
}

// TestCopyToReplica_404CleansUpStaleMetadata verifies the copy to replica 404 cleans up stale metadata contract.
// Asserts that staleDeleted = , want 1.
func TestCopyToReplica_404CleansUpStaleMetadata(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().GetBackend("b2").Return(dstBe, nil)
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe}).AnyTimes()
	ops.EXPECT().StreamCopy(gomock.Any(), srcBe, dstBe, "key1").
		Return(fmt.Errorf("read: %w", &httpError{code: 404, msg: "NoSuchKey"}))

	r := NewReplicator(ops, pl, ms)
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
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().GetBackend("b2").Return(dstBe, nil)
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe}).AnyTimes()
	ops.EXPECT().StreamCopy(gomock.Any(), srcBe, dstBe, "key1").Return(&backend.CopyError{Phase: backend.CopyPhaseRead, Err: errors.New("timeout")})

	r := NewReplicator(ops, pl, ms)
	copies := []core.ObjectLocation{{BackendName: "b1"}}

	_, _, err := r.CopyToReplica(context.Background(), "key1", copies, "b2")
	if err == nil {
		t.Fatal("expected error when all sources fail")
	}
}

// TestCopyToReplica_WriteErrorShortCircuits pins the typed-write-error
// classification: a *backend.CopyError with CopyPhaseWrite returned by
// the first source must terminate the per-source loop immediately and
// surface the write failure, without StreamCopy being invoked against
// the remaining sources. Untyped errors (or read-phase errors) would
// fall through to the next source - that contrast is the whole point
// of the typed-error refactor.
func TestCopyToReplica_WriteErrorShortCircuits(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	src1 := backendtest.NewMockObjectBackend(ctrl)
	src2 := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().GetBackend("dst").Return(dstBe, nil)
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{
		"src1": src1,
		"src2": src2,
	}).AnyTimes()

	// First source triggers a typed write error. No expectation is set
	// for src2's StreamCopy, so the test fails (unexpected call) if
	// the short-circuit logic regresses and we fall through.
	ops.EXPECT().StreamCopy(gomock.Any(), src1, dstBe, "key1").
		Return(&backend.CopyError{Phase: backend.CopyPhaseWrite, Err: errors.New("413 EntityTooLarge")})

	r := NewReplicator(ops, pl, ms)
	copies := []core.ObjectLocation{
		{BackendName: "src1"},
		{BackendName: "src2"},
	}
	if _, _, err := r.CopyToReplica(context.Background(), "key1", copies, "dst"); err == nil {
		t.Fatal("expected write-phase error to surface, got nil")
	}
}

// TestCopyToReplica_UntypedErrorRetriesNextSource documents that errors
// without a phase tag are treated as read-side: the loop moves on to the
// next source rather than short-circuiting. This is the safety-fallback
// half of the classification contract.
func TestCopyToReplica_UntypedErrorRetriesNextSource(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	src1 := backendtest.NewMockObjectBackend(ctrl)
	src2 := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().GetBackend("dst").Return(dstBe, nil)
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{
		"src1": src1,
		"src2": src2,
	}).AnyTimes()

	// Untyped error from src1 must NOT terminate; the loop must call
	// StreamCopy on src2 next. Sentinel success on src2 proves the
	// fall-through happened.
	gomock.InOrder(
		ops.EXPECT().StreamCopy(gomock.Any(), src1, dstBe, "key1").
			Return(errors.New("plain error string")),
		ops.EXPECT().StreamCopy(gomock.Any(), src2, dstBe, "key1").Return(nil),
	)

	r := NewReplicator(ops, pl, ms)
	copies := []core.ObjectLocation{
		{BackendName: "src1"},
		{BackendName: "src2"},
	}
	source, _, err := r.CopyToReplica(context.Background(), "key1", copies, "dst")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if source != "src2" {
		t.Errorf("source = %q, want src2 (fallthrough after untyped error)", source)
	}
}

// TestCleanupOrphan_DelegatesToDeleteOrEnqueue verifies the cleanup orphan delegates to delete or enqueue path by exercising gomock.NewController, backendtest.NewMockObjectBackend, ops.EXPECT.
func TestCleanupOrphan_DelegatesToDeleteOrEnqueue(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	be := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": be})
	pl.EXPECT().DeleteOrEnqueue(gomock.Any(), be, "b1", "key1", "replication_orphan", int64(100))

	r := NewReplicator(ops, pl, ms)
	r.CleanupOrphan(context.Background(), "b1", "key1", 100)
}

// TestReplicate_FactorOne_Noop verifies the replicate factor one noop contract.
// Asserts that unexpected error:.
func TestReplicate_FactorOne_Noop(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	r := NewReplicator(NewMockOps(ctrl), NewMockPlacement(ctrl), &mockMetadataStore{})

	created, err := r.Replicate(context.Background(), config.ReplicationConfig{Factor: 1}, nil)
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
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{}).AnyTimes()

	r := NewReplicator(ops, pl, ms)
	cfg := config.ReplicationConfig{Factor: 2, BatchSize: 10, Concurrency: 1, UnhealthyThreshold: time.Hour}
	created, err := r.Replicate(context.Background(), cfg, nil)
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
	pl := NewMockPlacement(ctrl)

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ms := &mockMetadataStore{recordReplicaOK: true}

	pl.EXPECT().SelectReplicaTarget(gomock.Any(), int64(50), gomock.Any()).Return("b2", nil)
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe, "b2": dstBe}).AnyTimes()
	ops.EXPECT().GetBackend("b2").Return(dstBe, nil)
	ops.EXPECT().StreamCopy(gomock.Any(), srcBe, dstBe, "key1").Return(nil)
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()

	r := NewReplicator(ops, pl, ms)
	copies := []core.ObjectLocation{{BackendName: "b1", SizeBytes: 50}}

	outcome := r.ReplicateObject(context.Background(), "key1", copies, 1)
	if outcome.Created != 1 {
		t.Errorf("created = %d, want 1", outcome.Created)
	}
	if outcome.Failed() != 0 {
		t.Errorf("Failed() = %d, want 0", outcome.Failed())
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
	pl := NewMockPlacement(ctrl)

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	failBe := backendtest.NewMockObjectBackend(ctrl)
	okBe := backendtest.NewMockObjectBackend(ctrl)

	ms := &mockMetadataStore{recordReplicaOK: true}

	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{
		"src": srcBe, "fail": failBe, "ok": okBe,
	}).AnyTimes()

	// First call selects "fail", second (with "fail" excluded) selects "ok",
	// third finds no remaining targets and returns "" to end the loop.
	gomock.InOrder(
		pl.EXPECT().SelectReplicaTarget(gomock.Any(), int64(50), gomock.Any()).
			DoAndReturn(func(_ context.Context, _ int64, excl map[string]bool) (string, error) {
				if excl["fail"] {
					t.Fatal("first call should not exclude 'fail'")
				}
				return "fail", nil
			}),
		pl.EXPECT().SelectReplicaTarget(gomock.Any(), int64(50), gomock.Any()).
			DoAndReturn(func(_ context.Context, _ int64, excl map[string]bool) (string, error) {
				if !excl["fail"] {
					t.Fatal("second call must exclude 'fail' after write failure")
				}
				return "ok", nil
			}),
		pl.EXPECT().SelectReplicaTarget(gomock.Any(), int64(50), gomock.Any()).
			Return("", nil),
	)

	// "fail" backend: GetBackend succeeds, StreamCopy returns write error
	ops.EXPECT().GetBackend("fail").Return(failBe, nil)
	ops.EXPECT().StreamCopy(gomock.Any(), srcBe, failBe, "key1").
		Return(&backend.CopyError{Phase: backend.CopyPhaseWrite, Err: errors.New("put object failed: 413 EntityTooLarge")})

	// "ok" backend: succeeds
	ops.EXPECT().GetBackend("ok").Return(okBe, nil)
	ops.EXPECT().StreamCopy(gomock.Any(), srcBe, okBe, "key1").Return(nil)

	r := NewReplicator(ops, pl, ms)
	copies := []core.ObjectLocation{{BackendName: "src", SizeBytes: 50}}

	// Request 2 replicas: first attempt hits "fail" and should fall through
	// to "ok" on the second iteration with "fail" excluded.
	outcome := r.ReplicateObject(context.Background(), "key1", copies, 2)
	if outcome.Created != 1 {
		t.Errorf("created = %d, want 1", outcome.Created)
	}
	if outcome.CopyErrors != 1 {
		t.Errorf("CopyErrors = %d, want 1", outcome.CopyErrors)
	}
	if !outcome.NoTarget {
		t.Errorf("NoTarget = false, want true (selection ran out)")
	}
}

// TestReplicateObject_RecordReplicaErrorExcludesTarget verifies the replicate object record replica error excludes target contract.
// Asserts that unexpected error:.
func TestReplicateObject_RecordReplicaErrorExcludesTarget(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	pl := NewMockPlacement(ctrl)

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	failBe := backendtest.NewMockObjectBackend(ctrl)

	ms := &mockMetadataStore{recordReplicaErr: errors.New("db down")}

	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{
		"src": srcBe, "fail": failBe,
	}).AnyTimes()

	gomock.InOrder(
		pl.EXPECT().SelectReplicaTarget(gomock.Any(), int64(50), gomock.Any()).Return("fail", nil),
		// After RecordReplica error, "fail" is excluded; no targets left.
		pl.EXPECT().SelectReplicaTarget(gomock.Any(), int64(50), gomock.Any()).
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
	pl.EXPECT().DeleteOrEnqueue(gomock.Any(), failBe, "fail", "key1", "replication_orphan", int64(50))

	r := NewReplicator(ops, pl, ms)
	copies := []core.ObjectLocation{{BackendName: "src", SizeBytes: 50}}
	outcome := r.ReplicateObject(context.Background(), "key1", copies, 1)
	if outcome.Created != 0 {
		t.Errorf("created = %d, want 0", outcome.Created)
	}
	if outcome.RecordErrors != 1 {
		t.Errorf("RecordErrors = %d, want 1", outcome.RecordErrors)
	}
}

// TestReplicateObject_NotInsertedExcludesTarget verifies the replicate object not inserted excludes target contract.
// Asserts that unexpected error:.
func TestReplicateObject_NotInsertedExcludesTarget(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	pl := NewMockPlacement(ctrl)

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	staleBe := backendtest.NewMockObjectBackend(ctrl)

	// recordReplicaOK=false simulates source deleted during replication
	ms := &mockMetadataStore{recordReplicaOK: false}

	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{
		"src": srcBe, "stale": staleBe,
	}).AnyTimes()

	gomock.InOrder(
		pl.EXPECT().SelectReplicaTarget(gomock.Any(), int64(50), gomock.Any()).Return("stale", nil),
		// After !inserted, "stale" is excluded; no targets left.
		pl.EXPECT().SelectReplicaTarget(gomock.Any(), int64(50), gomock.Any()).
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
	pl.EXPECT().DeleteOrEnqueue(gomock.Any(), staleBe, "stale", "key1", "replication_orphan", int64(50))

	r := NewReplicator(ops, pl, ms)
	copies := []core.ObjectLocation{{BackendName: "src", SizeBytes: 50}}
	outcome := r.ReplicateObject(context.Background(), "key1", copies, 1)
	if outcome.Created != 0 {
		t.Errorf("created = %d, want 0", outcome.Created)
	}
	if outcome.Superseded != 1 {
		t.Errorf("Superseded = %d, want 1", outcome.Superseded)
	}
}

// httpError is a worker-package test helper that satisfies the smithy
// HTTPStatusCode() interface used by backend.IsNotFound. Worker tests
// (replicator, pending) construct typed 404s through this fake instead
// of pulling in the real AWS SDK error hierarchy.
type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string       { return e.msg }
func (e *httpError) HTTPStatusCode() int { return e.code }
