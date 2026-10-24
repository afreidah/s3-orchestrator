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

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// copyReq matches the *backend.CopyRequest a StreamCopy is handed. These tests
// care which clients and paths a copy used, not the endpoint names, so a nil
// client or empty path matches anything.
type copyReq struct {
	src, dst       backend.ObjectBackend
	srcKey, dstKey string
}

// Matches reports whether x is a copy request between the expected clients
// and paths.
func (m copyReq) Matches(x any) bool {
	req, ok := x.(*backend.CopyRequest)
	if !ok {
		return false
	}
	return (m.src == nil || req.Src.Backend == m.src) && (m.dst == nil || req.Dst.Backend == m.dst) &&
		(m.srcKey == "" || req.SrcKey == m.srcKey) && (m.dstKey == "" || req.DstKey == m.dstKey)
}

// String describes the matcher for gomock's failure output.
func (m copyReq) String() string {
	return fmt.Sprintf("copy %v:%q -> %v:%q", m.src, m.srcKey, m.dst, m.dstKey)
}

// -------------------------------------------------------------------------
// planUnderReplicated (planner)
// -------------------------------------------------------------------------

// TestPlanUnderReplicated_TablePinsPlacementDecisions checks that keys at or
// above the factor are dropped and each task's needed count equals factor
// minus its existing copies.
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
	r := newTestReplicator(newMockOps(ctrl), NewMockPlacement(ctrl), &mockMetadataStore{})
	if r.Config() != nil {
		t.Fatal("expected nil config before set")
	}
	cfg := &config.ReplicationConfig{Factor: 3}
	r.SetConfig(cfg)
	if got := r.Config(); got.Factor != 3 {
		t.Errorf("Config().Factor = %d, want 3", got.Factor)
	}
}

// TestCopyToReplica_Success verifies the copy to replica success contract.
// Asserts that unexpected error:.
func TestCopyToReplica_Success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().GetBackend("b2").Return(dstBe, nil)
	expectFleet(ops, map[string]backend.ObjectBackend{"b1": srcBe})
	// Read at the source copy's path and written at the replica's own, which
	// the caller minted before the transfer started.
	ops.EXPECT().StreamCopy(gomock.Any(), copyReq{srcBe, dstBe, "key1!on-b1", "key1!replica"}).Return(int64(0), nil)

	r := newTestReplicator(ops, pl, ms)
	copies := []core.ObjectLocation{{BackendName: "b1", StorageKey: "key1!on-b1", SizeBytes: 4096}}

	src, err := r.CopyToReplica(context.Background(), "key1", "key1"+"!replica", copies, "b2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src.BackendName != "b1" {
		t.Errorf("source = %q, want b1", src.BackendName)
	}
	if src.SizeBytes != 4096 {
		t.Errorf("size = %d, want 4096 (the source's SizeBytes)", src.SizeBytes)
	}
}

// newOpenBreakerBackend wraps a mock whose calls fail and trips its breaker
// open (threshold 1), so it serves no reads. Used to prove
// CopyToReplica excludes dead sources from selection.
func newOpenBreakerBackend(t *testing.T, ctrl *gomock.Controller, name string) *backend.CircuitBreakerBackend {
	t.Helper()
	inner := backendtest.NewMockCheckedBackend(ctrl)
	inner.EXPECT().GetObject(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, errors.New("billing account closed")).AnyTimes()
	cb := backend.NewCircuitBreakerBackend(inner, backend.CircuitBreakerConfig{Name: name, Threshold: 1, Timeout: time.Hour})
	if _, err := cb.GetObject(context.Background(), "trip", ""); err == nil {
		t.Fatal("expected the priming call to fail")
	}
	if cb.Served().Has(core.RequiredForRead) {
		t.Fatal("expected reads refused after a failure")
	}
	return cb
}

// TestCopyToReplica_SkipsOpenBreakerSource verifies a source whose breaker is
// open is excluded from selection: the copy starts against the healthy source
// only, so a dead source can never stall a copy and trip the target.
func TestCopyToReplica_SkipsOpenBreakerSource(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	deadSrc := newOpenBreakerBackend(t, ctrl, "b0")
	healthySrc := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().GetBackend("b2").Return(dstBe, nil)
	expectFleet(ops, map[string]backend.ObjectBackend{"b0": deadSrc, "b1": healthySrc})
	// Only the healthy source may be streamed; an unexpected StreamCopy against
	// deadSrc fails the test (no expectation registered for it).
	ops.EXPECT().StreamCopy(gomock.Any(), copyReq{healthySrc, dstBe, "key1!on-b1", "key1!replica"}).Return(int64(0), nil)

	r := newTestReplicator(ops, pl, ms)
	// Dead source listed first to prove exclusion, not just ordering, is at work.
	copies := []core.ObjectLocation{
		{BackendName: "b0", StorageKey: "key1!on-b0", SizeBytes: 10},
		{BackendName: "b1", StorageKey: "key1!on-b1", SizeBytes: 4096},
	}

	src, err := r.CopyToReplica(context.Background(), "key1", "key1"+"!replica", copies, "b2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src.BackendName != "b1" || src.SizeBytes != 4096 {
		t.Errorf("got (%q, %d), want (b1, 4096)", src.BackendName, src.SizeBytes)
	}
}

// TestCopyToReplica_FallsBackWhenAllSourcesUnhealthy verifies an object whose
// only copy sits on an open-breaker backend still gets a copy attempt rather
// than being stranded.
func TestCopyToReplica_FallsBackWhenAllSourcesUnhealthy(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	deadSrc := newOpenBreakerBackend(t, ctrl, "b0")
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().GetBackend("b2").Return(dstBe, nil)
	expectFleet(ops, map[string]backend.ObjectBackend{"b0": deadSrc})
	// With no healthy source, the fallback must still attempt the dead one.
	ops.EXPECT().StreamCopy(gomock.Any(), copyReq{src: deadSrc, dst: dstBe}).Return(int64(0), nil)

	r := newTestReplicator(ops, pl, ms)
	copies := []core.ObjectLocation{{BackendName: "b0", SizeBytes: 7}}

	src, err := r.CopyToReplica(context.Background(), "key1", "key1"+"!replica", copies, "b2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src.BackendName != "b0" {
		t.Errorf("source = %q, want b0 (fallback attempt)", src.BackendName)
	}
}

// TestCopyToReplica_404CleansUpStaleMetadata verifies the copy to replica 404 cleans up stale metadata contract.
// Asserts that staleDeleted = , want 1.
func TestCopyToReplica_404CleansUpStaleMetadata(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().GetBackend("b2").Return(dstBe, nil)
	expectFleet(ops, map[string]backend.ObjectBackend{"b1": srcBe})
	ops.EXPECT().StreamCopy(gomock.Any(), copyReq{src: srcBe, dst: dstBe}).
		Return(int64(0), fmt.Errorf("read: %w", &backendtest.StatusError{Code: 404, Msg: "NoSuchKey"}))

	r := newTestReplicator(ops, pl, ms)
	copies := []core.ObjectLocation{{BackendName: "b1"}}

	_, err := r.CopyToReplica(context.Background(), "key1", "key1"+"!replica", copies, "b2")
	if err == nil {
		t.Fatal("expected error when source returns 404")
	}
	if ms.staleDeleted != 1 {
		t.Errorf("staleDeleted = %d, want 1", ms.staleDeleted)
	}
	if len(ms.deleteReasons) != 1 || ms.deleteReasons[0] != "" {
		t.Errorf("delete reasons = %q, want one empty reason so nothing is queued", ms.deleteReasons)
	}
}

// TestPruneStaleSource_KeepsTheRowOnFailure verifies a stale-row removal that
// the store declines or fails leaves the row in place without aborting.
func TestPruneStaleSource_KeepsTheRowOnFailure(t *testing.T) {
	t.Parallel()
	for _, delErr := range []error{core.ErrCopyChanged, errors.New("db down")} {
		t.Run(delErr.Error(), func(t *testing.T) {
			t.Parallel()
			ctrl := gomock.NewController(t)
			ms := &mockMetadataStore{deleteLocationErr: delErr}
			r := newTestReplicator(newMockOps(ctrl), NewMockPlacement(ctrl), ms)

			r.pruneStaleSource(context.Background(), &core.ObjectLocation{ObjectKey: "key1", BackendName: "b1"})
			if ms.staleDeleted != 0 {
				t.Errorf("staleDeleted = %d, want 0", ms.staleDeleted)
			}
		})
	}
}

// TestCopyToReplica_AllSourcesFail verifies the copy to replica all sources fail path by exercising gomock.NewController, backendtest.NewMockObjectBackend, ops.EXPECT.
func TestCopyToReplica_AllSourcesFail(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().GetBackend("b2").Return(dstBe, nil)
	expectFleet(ops, map[string]backend.ObjectBackend{"b1": srcBe})
	ops.EXPECT().StreamCopy(gomock.Any(), copyReq{src: srcBe, dst: dstBe}).Return(int64(0), &backend.CopyError{Phase: backend.CopyPhaseRead, Err: errors.New("timeout")})

	r := newTestReplicator(ops, pl, ms)
	copies := []core.ObjectLocation{{BackendName: "b1"}}

	_, err := r.CopyToReplica(context.Background(), "key1", "key1"+"!replica", copies, "b2")
	if err == nil {
		t.Fatal("expected error when all sources fail")
	}
}

// TestCopyToReplica_WriteErrorShortCircuits checks that a *backend.CopyError
// with CopyPhaseWrite from the first source ends the per-source loop and
// surfaces the write failure without trying the remaining sources.
func TestCopyToReplica_WriteErrorShortCircuits(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	src1 := backendtest.NewMockObjectBackend(ctrl)
	src2 := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().GetBackend("dst").Return(dstBe, nil)
	expectFleet(ops, map[string]backend.ObjectBackend{
		"src1": src1,
		"src2": src2,
	})

	// First source triggers a typed write error. No expectation is set
	// for src2's StreamCopy, so the test fails (unexpected call) if
	// the short-circuit logic regresses and we fall through.
	ops.EXPECT().StreamCopy(gomock.Any(), copyReq{src: src1, dst: dstBe}).
		Return(int64(0), &backend.CopyError{Phase: backend.CopyPhaseWrite, Err: errors.New("413 EntityTooLarge")})

	r := newTestReplicator(ops, pl, ms)
	copies := []core.ObjectLocation{
		{BackendName: "src1"},
		{BackendName: "src2"},
	}
	if _, err := r.CopyToReplica(context.Background(), "key1", "key1"+"!replica", copies, "dst"); err == nil {
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
	ops := newMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	src1 := backendtest.NewMockObjectBackend(ctrl)
	src2 := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().GetBackend("dst").Return(dstBe, nil)
	expectFleet(ops, map[string]backend.ObjectBackend{
		"src1": src1,
		"src2": src2,
	})

	// Untyped error from src1 must NOT terminate; the loop must call
	// StreamCopy on src2 next. Sentinel success on src2 proves the
	// fall-through happened.
	gomock.InOrder(
		ops.EXPECT().StreamCopy(gomock.Any(), copyReq{src: src1, dst: dstBe}).
			Return(int64(0), errors.New("plain error string")),
		ops.EXPECT().StreamCopy(gomock.Any(), copyReq{src: src2, dst: dstBe}).Return(int64(0), nil),
	)

	r := newTestReplicator(ops, pl, ms)
	copies := []core.ObjectLocation{
		{BackendName: "src1"},
		{BackendName: "src2"},
	}
	source, err := r.CopyToReplica(context.Background(), "key1", "key1"+"!replica", copies, "dst")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if source.BackendName != "src2" {
		t.Errorf("source = %q, want src2 (fallthrough after untyped error)", source.BackendName)
	}
}

// declinedClaimReplicator builds a replicator whose only target, b2, declines
// the claim with claimErr, so a test can assert how the attempt is tallied.
// Nothing is copied or placed: the unset gomock expectations assert it.
func declinedClaimReplicator(t *testing.T, claimErr error) *Replicator {
	t.Helper()
	ctrl := gomock.NewController(t)
	ops := newMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	expectFleet(ops, map[string]backend.ObjectBackend{"b1": backendtest.NewMockObjectBackend(ctrl)})
	gomock.InOrder(
		pl.EXPECT().RankReplicaTargets(int64(50), gomock.Any()).Return([]string{"b2"}),
		pl.EXPECT().RankReplicaTargets(int64(50), gomock.Any()).
			DoAndReturn(func(_ int64, excl map[string]bool) []string {
				if !excl["b2"] {
					t.Error("must exclude b2 after its claim was declined")
				}
				return nil
			}),
	)
	pl.EXPECT().ClaimCopyTarget(gomock.Any(), intentOn("b2", "key1"), core.AcceptForWrite).Return(claimErr)
	return newTestReplicator(ops, pl, &mockMetadataStore{})
}

// TestReplicateObject_ClaimRefusedExcludesTarget verifies a target that has no
// room for the replica is dropped without counting an error.
func TestReplicateObject_ClaimRefusedExcludesTarget(t *testing.T) {
	t.Parallel()
	r := declinedClaimReplicator(t, core.ErrTargetRefused)
	copies := []core.ObjectLocation{{BackendName: "b1", SizeBytes: 50}}

	out := r.ReplicateObject(context.Background(), "key1", copies, 1)
	if out.Created != 0 || out.RecordErrors != 0 || out.CopyErrors != 0 {
		t.Errorf("outcome = %+v, want nothing created and no errors", out)
	}
}

// TestReplicateObject_ClaimErrorCountsARecordError verifies a claim that fails
// for any reason but a refusal drops the target and counts a record error.
func TestReplicateObject_ClaimErrorCountsARecordError(t *testing.T) {
	t.Parallel()
	r := declinedClaimReplicator(t, errors.New("db down"))
	copies := []core.ObjectLocation{{BackendName: "b1", SizeBytes: 50}}

	before := promtest.ToFloat64(telemetry.ReplicationErrorsTotal)
	out := r.ReplicateObject(context.Background(), "key1", copies, 1)
	if out.Created != 0 || out.RecordErrors != 1 {
		t.Errorf("outcome = %+v, want one record error", out)
	}
	if got := promtest.ToFloat64(telemetry.ReplicationErrorsTotal) - before; got < 1 {
		t.Errorf("ReplicationErrorsTotal moved by %v, want at least 1", got)
	}
}

// TestReplicate_FactorOne_Noop verifies the replicate factor one noop contract.
// Asserts that unexpected error:.
func TestReplicate_FactorOne_Noop(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	r := newTestReplicator(newMockOps(ctrl), NewMockPlacement(ctrl), &mockMetadataStore{})

	sum, err := r.Replicate(context.Background(), config.ReplicationConfig{Factor: 1}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sum.CopiesCreated != 0 {
		t.Errorf("created = %d, want 0", sum.CopiesCreated)
	}
}

// TestReplicate_NothingUnderReplicated verifies the replicate nothing under replicated contract.
// Asserts that unexpected error:.
// Deliberately not parallel: asserts a process-wide gauge that other
// replication tests overwrite, so a concurrent run reads whichever cycle
// finished last.
func TestReplicate_NothingUnderReplicated(t *testing.T) {
	ctrl := gomock.NewController(t)
	ops := newMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	expectFleet(ops, map[string]backend.ObjectBackend{})

	r := newTestReplicator(ops, pl, ms)
	cfg := config.ReplicationConfig{Factor: 2, BatchSize: 10, Concurrency: 1, UnhealthyThreshold: time.Hour}
	sum, err := r.Replicate(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sum.CopiesCreated != 0 {
		t.Errorf("created = %d, want 0", sum.CopiesCreated)
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
	ops := newMockOps(ctrl)
	pl := NewMockPlacement(ctrl)

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ms := &mockMetadataStore{placeCopySize: 50}
	placeThroughStore(pl, ms)

	pl.EXPECT().RankReplicaTargets(int64(50), gomock.Any()).Return([]string{"b2"})
	expectFleet(ops, map[string]backend.ObjectBackend{"b1": srcBe, "b2": dstBe})
	ops.EXPECT().GetBackend("b2").Return(dstBe, nil)
	ops.EXPECT().StreamCopy(gomock.Any(), copyReq{src: srcBe, dst: dstBe, srcKey: "key1!on-b1"}).Return(int64(0), nil)

	r := newTestReplicator(ops, pl, ms)
	copies := []core.ObjectLocation{{BackendName: "b1", StorageKey: "key1!on-b1", SizeBytes: 50}}

	outcome := r.ReplicateObject(context.Background(), "key1", copies, 1)
	if outcome.Created != 1 {
		t.Errorf("created = %d, want 1", outcome.Created)
	}
	if outcome.Failed() != 0 {
		t.Errorf("Failed() = %d, want 0", outcome.Failed())
	}
	if len(ms.placements) != 1 {
		t.Fatalf("PlaceCopy calls = %d, want 1", len(ms.placements))
	}
	p := ms.placements[0]
	if p.Source == nil || p.Source.BackendName != "b1" || p.Source.StorageKey != "key1!on-b1" {
		t.Errorf("placement source = %+v, want the b1 copy the pass read", p.Source)
	}
	if p.Intent == nil || p.Intent.BackendName != "b2" || p.Intent.StorageKey == "" || p.RemoveSource {
		t.Fatalf("placement = %+v, want an intent on b2 with a minted key, and the source kept", p)
	}
	if p.Intent.Role != core.PendingRoleCompanion || p.Intent.SizeBytes != 50 {
		t.Errorf("intent = %+v, want a companion claim of 50 bytes", p.Intent)
	}
	if p.Reformed || p.Reasons != writepath.ReplicationReasons {
		t.Errorf("placement reformed %v reasons %+v, want a verbatim replica under the replication reasons",
			p.Reformed, p.Reasons)
	}
}

// TestReplicateObject_WriteFailureExcludesTarget verifies the replicate object write failure excludes target contract.
// Asserts that unexpected error:.
func TestReplicateObject_WriteFailureExcludesTarget(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockOps(ctrl)
	pl := NewMockPlacement(ctrl)

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	failBe := backendtest.NewMockObjectBackend(ctrl)
	okBe := backendtest.NewMockObjectBackend(ctrl)

	ms := &mockMetadataStore{placeCopySize: 50}
	placeThroughStore(pl, ms)

	expectFleet(ops, map[string]backend.ObjectBackend{
		"src": srcBe, "fail": failBe, "ok": okBe,
	})

	// First call selects "fail", second (with "fail" excluded) selects "ok",
	// third finds no remaining targets and returns "" to end the loop.
	gomock.InOrder(
		pl.EXPECT().RankReplicaTargets(int64(50), gomock.Any()).
			DoAndReturn(func(_ int64, excl map[string]bool) []string {
				if excl["fail"] {
					t.Fatal("first call should not exclude 'fail'")
				}
				return []string{"fail"}
			}),
		pl.EXPECT().RankReplicaTargets(int64(50), gomock.Any()).
			DoAndReturn(func(_ int64, excl map[string]bool) []string {
				if !excl["fail"] {
					t.Fatal("second call must exclude 'fail' after write failure")
				}
				return []string{"ok"}
			}),
		pl.EXPECT().RankReplicaTargets(int64(50), gomock.Any()).
			Return(nil),
	)

	// "fail" backend: GetBackend succeeds, StreamCopy returns write error
	ops.EXPECT().GetBackend("fail").Return(failBe, nil)
	ops.EXPECT().StreamCopy(gomock.Any(), copyReq{src: srcBe, dst: failBe}).
		Return(int64(0), &backend.CopyError{Phase: backend.CopyPhaseWrite, Err: errors.New("put object failed: 413 EntityTooLarge")})
	// The failed copy may have left bytes on "fail", so its intent is discarded.
	pl.EXPECT().Discard(gomock.Any(), intentOn("fail", "key1"), writepath.ReplicationReasons.Orphan)

	// "ok" backend: succeeds
	ops.EXPECT().GetBackend("ok").Return(okBe, nil)
	ops.EXPECT().StreamCopy(gomock.Any(), copyReq{src: srcBe, dst: okBe}).Return(int64(0), nil)

	r := newTestReplicator(ops, pl, ms)
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

// TestReplicateObject_PlaceCopyErrorExcludesTarget pins that a placement that
// errors counts as a record error and drops the target, leaving the written
// bytes to the placement and the reaper.
func TestReplicateObject_PlaceCopyErrorExcludesTarget(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockOps(ctrl)
	pl := NewMockPlacement(ctrl)

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	failBe := backendtest.NewMockObjectBackend(ctrl)

	ms := &mockMetadataStore{placeCopyErr: errors.New("db down")}
	placeThroughStore(pl, ms)

	expectFleet(ops, map[string]backend.ObjectBackend{
		"src": srcBe, "fail": failBe,
	})

	gomock.InOrder(
		pl.EXPECT().RankReplicaTargets(int64(50), gomock.Any()).Return([]string{"fail"}),
		// After a PlaceCopy error, "fail" is excluded; no targets left.
		pl.EXPECT().RankReplicaTargets(int64(50), gomock.Any()).
			DoAndReturn(func(_ int64, excl map[string]bool) []string {
				if !excl["fail"] {
					t.Fatal("must exclude 'fail' after a PlaceCopy error")
				}
				return nil
			}),
	)

	ops.EXPECT().GetBackend("fail").Return(failBe, nil)
	ops.EXPECT().StreamCopy(gomock.Any(), copyReq{src: srcBe, dst: failBe}).Return(int64(0), nil)

	r := newTestReplicator(ops, pl, ms)
	copies := []core.ObjectLocation{{BackendName: "src", SizeBytes: 50}}
	outcome := r.ReplicateObject(context.Background(), "key1", copies, 1)
	if outcome.Created != 0 {
		t.Errorf("created = %d, want 0", outcome.Created)
	}
	if outcome.RecordErrors != 1 {
		t.Errorf("RecordErrors = %d, want 1", outcome.RecordErrors)
	}
	if len(ms.placements) != 1 || ms.placements[0].Intent.BackendName != "fail" {
		t.Errorf("placements = %+v, want one on the fail backend", ms.placements)
	}
}

// TestReplicateObject_DeclinedPlacementExcludesTarget pins that a placement
// declined because the source changed or the target refused counts as
// superseded and drops the target, leaving the cleanup of the written bytes to
// the placement.
func TestReplicateObject_DeclinedPlacementExcludesTarget(t *testing.T) {
	t.Parallel()
	for _, declined := range []error{core.ErrCopyChanged, core.ErrTargetRefused} {
		t.Run(declined.Error(), func(t *testing.T) {
			t.Parallel()
			ctrl := gomock.NewController(t)
			ops := newMockOps(ctrl)
			pl := NewMockPlacement(ctrl)

			srcBe := backendtest.NewMockObjectBackend(ctrl)
			staleBe := backendtest.NewMockObjectBackend(ctrl)
			ms := &mockMetadataStore{placeCopyErr: declined}
			placeThroughStore(pl, ms)

			expectFleet(ops, map[string]backend.ObjectBackend{
				"src": srcBe, "stale": staleBe,
			})

			gomock.InOrder(
				pl.EXPECT().RankReplicaTargets(int64(50), gomock.Any()).Return([]string{"stale"}),
				pl.EXPECT().RankReplicaTargets(int64(50), gomock.Any()).
					DoAndReturn(func(_ int64, excl map[string]bool) []string {
						if !excl["stale"] {
							t.Error("must exclude 'stale' after a declined placement")
						}
						return nil
					}),
			)

			ops.EXPECT().GetBackend("stale").Return(staleBe, nil)
			ops.EXPECT().StreamCopy(gomock.Any(), copyReq{src: srcBe, dst: staleBe}).Return(int64(0), nil)

			r := newTestReplicator(ops, pl, ms)
			copies := []core.ObjectLocation{{BackendName: "src", SizeBytes: 50}}
			outcome := r.ReplicateObject(context.Background(), "key1", copies, 1)
			if outcome.Created != 0 {
				t.Errorf("created = %d, want 0", outcome.Created)
			}
			if outcome.Superseded != 1 || outcome.RecordErrors != 0 {
				t.Errorf("Superseded = %d RecordErrors = %d, want 1/0", outcome.Superseded, outcome.RecordErrors)
			}
		})
	}
}
