// -------------------------------------------------------------------------------
// Rebalancer Tests
//
// Author: Alex Freidah
//
// Covers the rebalancer's threshold gate (only acts when utilization
// skew exceeds the configured fraction), config round-trip, the planner
// that batches per-source backend queries, and the move-and-cleanup
// orchestration against a mock store and mock backends. The threshold
// edge cases (single backend, empty stats) pin the early-exit
// invariants that keep the worker from no-op-spinning on degenerate
// inputs.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestRebalancer_SetConfig_RoundTrip verifies the rebalancer set config round trip contract.
// Asserts that Config().Strategy = , want spread.
func TestRebalancer_SetConfig_RoundTrip(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	r := NewRebalancer(NewMockOps(ctrl), NewMockPlacement(ctrl), &mockMetadataStore{})
	if r.Config() != nil {
		t.Fatal("expected nil config before set")
	}
	cfg := &config.RebalanceConfig{Strategy: "spread", Threshold: 0.1}
	r.SetConfig(cfg)
	if got := r.Config(); got.Strategy != "spread" {
		t.Errorf("Config().Strategy = %q, want spread", got.Strategy)
	}
}

// TestExceedsThreshold_BelowThreshold verifies the exceeds threshold below threshold behaviour described by the test name.
func TestExceedsThreshold_BelowThreshold(t *testing.T) {
	t.Parallel()
	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 500, BytesLimit: 1000},
		"b2": {BytesUsed: 450, BytesLimit: 1000},
	}
	if ExceedsThreshold(stats, []string{"b1", "b2"}, 0.1) {
		t.Error("5% spread should not exceed 10% threshold")
	}
}

// TestExceedsThreshold_AboveThreshold verifies the exceeds threshold above threshold behaviour described by the test name.
func TestExceedsThreshold_AboveThreshold(t *testing.T) {
	t.Parallel()
	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000},
		"b2": {BytesUsed: 100, BytesLimit: 1000},
	}
	if !ExceedsThreshold(stats, []string{"b1", "b2"}, 0.1) {
		t.Error("80% spread should exceed 10% threshold")
	}
}

// TestExceedsThreshold_SingleBackend verifies the exceeds threshold single backend behaviour described by the test name.
func TestExceedsThreshold_SingleBackend(t *testing.T) {
	t.Parallel()
	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000},
	}
	if ExceedsThreshold(stats, []string{"b1"}, 0.1) {
		t.Error("single backend cannot exceed threshold")
	}
}

// TestExceedsThreshold_EmptyStats verifies the exceeds threshold empty stats behaviour described by the test name.
func TestExceedsThreshold_EmptyStats(t *testing.T) {
	t.Parallel()
	if ExceedsThreshold(nil, []string{"b1", "b2"}, 0.1) {
		t.Error("empty stats should not exceed threshold")
	}
}

// TestPlanSpreadEven_BalancedSkipped verifies the plan spread even balanced skipped contract.
// Asserts that unexpected error:.
func TestPlanSpreadEven_BalancedSkipped(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	ops.EXPECT().BackendOrder().Return([]string{"b1", "b2"}).AnyTimes()
	ops.EXPECT().Usage().Return(unlimitedUsage()).AnyTimes()

	r := NewRebalancer(ops, pl, ms)
	// Equal utilization: no moves needed
	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 500, BytesLimit: 1000},
		"b2": {BytesUsed: 500, BytesLimit: 1000},
	}
	plan, err := r.PlanSpreadEven(context.Background(), stats, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan) != 0 {
		t.Errorf("balanced backends should produce empty plan, got %d moves", len(plan))
	}
}

// TestPlanSpreadEven_ImbalancedPlansMoves verifies the plan spread even imbalanced plans moves contract.
// Asserts that unexpected error:.
func TestPlanSpreadEven_ImbalancedPlansMoves(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{
		objectsByBackend: map[string][]core.ObjectLocation{
			"b1": {{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100}},
		},
	}

	ops.EXPECT().BackendOrder().Return([]string{"b1", "b2"}).AnyTimes()
	ops.EXPECT().Usage().Return(unlimitedUsage()).AnyTimes()

	r := NewRebalancer(ops, pl, ms)
	// b1 at 80%, b2 at 20% -> target ~50%, b1 has excess
	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 800, BytesLimit: 1000},
		"b2": {BytesUsed: 200, BytesLimit: 1000},
	}
	plan, err := r.PlanSpreadEven(context.Background(), stats, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan) == 0 {
		t.Error("imbalanced backends should produce moves")
	}
	for _, mv := range plan {
		if mv.FromBackend != "b1" || mv.ToBackend != "b2" {
			t.Errorf("move should be b1->b2, got %s->%s", mv.FromBackend, mv.ToBackend)
		}
	}
}

// TestPlanSpreadEven_BatchesBackendLookup verifies the planner issues
// exactly one GetObjectBackendsForKeys call per source backend it
// considers, regardless of how many candidate objects that source has.
// Catches regression of the original N+1 GetAllObjectLocations call
// inside the per-object loop.
func TestPlanSpreadEven_BatchesBackendLookup(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	objs := []core.ObjectLocation{
		{ObjectKey: "k1", BackendName: "b1", SizeBytes: 50},
		{ObjectKey: "k2", BackendName: "b1", SizeBytes: 50},
		{ObjectKey: "k3", BackendName: "b1", SizeBytes: 50},
		{ObjectKey: "k4", BackendName: "b1", SizeBytes: 50},
		{ObjectKey: "k5", BackendName: "b1", SizeBytes: 50},
	}
	ms := &mockMetadataStore{
		objectsByBackend: map[string][]core.ObjectLocation{"b1": objs},
		// Pre-populate: k2 already has a replica on b2, the others don't.
		getBackendsForKeysResp: map[string][]string{
			"k1": {"b1"},
			"k2": {"b1", "b2"},
			"k3": {"b1"},
			"k4": {"b1"},
			"k5": {"b1"},
		},
	}
	ops.EXPECT().BackendOrder().Return([]string{"b1", "b2"}).AnyTimes()
	ops.EXPECT().Usage().Return(unlimitedUsage()).AnyTimes()

	r := NewRebalancer(ops, pl, ms)
	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 800, BytesLimit: 1000},
		"b2": {BytesUsed: 200, BytesLimit: 1000},
	}
	plan, err := r.PlanSpreadEven(context.Background(), stats, 10)
	if err != nil {
		t.Fatalf("PlanSpreadEven: %v", err)
	}
	if ms.getBackendsForKeysCalls != 1 {
		t.Errorf("expected 1 GetObjectBackendsForKeys call (one per source), got %d", ms.getBackendsForKeysCalls)
	}
	for _, mv := range plan {
		if mv.ObjectKey == "k2" {
			t.Errorf("k2 already lives on b2 and must not be planned for move; plan=%+v", plan)
		}
	}
}

// TestPlanPackTight_BatchesBackendLookup verifies the same N+1 fix
// in the pack-tight planner: one batch lookup per source, regardless
// of how many candidate objects that source holds.
func TestPlanPackTight_BatchesBackendLookup(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	objs := []core.ObjectLocation{
		{ObjectKey: "k1", BackendName: "b2", SizeBytes: 50},
		{ObjectKey: "k2", BackendName: "b2", SizeBytes: 50},
		{ObjectKey: "k3", BackendName: "b2", SizeBytes: 50},
	}
	ms := &mockMetadataStore{
		objectsByBackend: map[string][]core.ObjectLocation{"b2": objs},
		// k2 already lives on b1 (the more-utilized destination); the
		// planner must skip it without re-querying per object.
		getBackendsForKeysResp: map[string][]string{
			"k1": {"b2"},
			"k2": {"b1", "b2"},
			"k3": {"b2"},
		},
	}
	ops.EXPECT().BackendOrder().Return([]string{"b1", "b2"}).AnyTimes()
	ops.EXPECT().Usage().Return(unlimitedUsage()).AnyTimes()

	r := NewRebalancer(ops, pl, ms)
	// b1 is the most-full destination, b2 is the source.
	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 800, BytesLimit: 1000},
		"b2": {BytesUsed: 100, BytesLimit: 1000},
	}
	plan, err := r.PlanPackTight(context.Background(), stats, 10)
	if err != nil {
		t.Fatalf("PlanPackTight: %v", err)
	}
	if ms.getBackendsForKeysCalls != 1 {
		t.Errorf("expected 1 GetObjectBackendsForKeys call (one per source), got %d", ms.getBackendsForKeysCalls)
	}
	for _, mv := range plan {
		if mv.ObjectKey == "k2" && mv.ToBackend == "b1" {
			t.Errorf("k2 already on b1 and must not be planned for move to b1; plan=%+v", plan)
		}
	}
}

// TestExecuteOneMove_Success verifies that the rebalancer dispatches a
// single MoveObject call to the shared primitive (#924) on the happy
// path. ExecuteOneMove no longer issues StreamCopy / DeleteOrEnqueue
// directly - the writepath.Coordinator owns those.
func TestExecuteOneMove_Success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	pl := NewMockPlacement(ctrl)

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)
	ms := &mockMetadataStore{}

	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe, "b2": dstBe}).AnyTimes()
	// Capture the request so the rebalance reason profile wiring is asserted,
	// not just the happy-path return.
	var got *writepath.MoveRequest
	pl.EXPECT().MoveObject(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, req *writepath.MoveRequest) (int64, error) {
			got = req
			return int64(100), nil
		})

	r := NewRebalancer(ops, pl, ms)
	ok := r.ExecuteOneMove(context.Background(), RebalanceMove{
		ObjectKey: "key1", FromBackend: "b1", ToBackend: "b2", SizeBytes: 100,
	}, "spread")
	if !ok {
		t.Error("expected successful move")
	}
	if got == nil || got.Reasons != writepath.RebalanceMoveReasons {
		t.Errorf("MoveRequest.Reasons = %+v, want RebalanceMoveReasons", got.Reasons)
	}
}

// TestExecuteOneMove_MoveObjectFails pins the failure-propagation
// branch: a generic MoveObject error (not ErrMoveStale) bumps the error
// telemetry counter and returns false.
func TestExecuteOneMove_MoveObjectFails(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe, "b2": dstBe}).AnyTimes()
	pl.EXPECT().MoveObject(gomock.Any(), gomock.Any()).Return(int64(0), errors.New("stream copy: timeout"))

	r := NewRebalancer(ops, pl, ms)
	ok := r.ExecuteOneMove(context.Background(), RebalanceMove{
		ObjectKey: "key1", FromBackend: "b1", ToBackend: "b2", SizeBytes: 100,
	}, "spread")
	if ok {
		t.Error("expected failed move on MoveObject error")
	}
}

// TestExecuteOneMove_MoveObjectStale pins the raced-row branch:
// MoveObject returns ErrMoveStale (movedSize=0) and the helper treats
// it as a non-error skip (no error-counter increment, no audit log).
func TestExecuteOneMove_MoveObjectStale(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe, "b2": dstBe}).AnyTimes()
	pl.EXPECT().MoveObject(gomock.Any(), gomock.Any()).Return(int64(0), writepath.ErrMoveStale)

	r := NewRebalancer(ops, pl, ms)
	ok := r.ExecuteOneMove(context.Background(), RebalanceMove{
		ObjectKey: "key1", FromBackend: "b1", ToBackend: "b2", SizeBytes: 100,
	}, "spread")
	if ok {
		t.Error("expected failed move when object already moved/deleted")
	}
}

// TestExecuteOneMove_SourceBackendNotFound verifies the execute one move source backend not found path by exercising gomock.NewController, ops.EXPECT, r.ExecuteOneMove.
func TestExecuteOneMove_SourceBackendNotFound(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{}).AnyTimes()

	r := NewRebalancer(ops, pl, ms)
	ok := r.ExecuteOneMove(context.Background(), RebalanceMove{
		ObjectKey: "key1", FromBackend: "gone", ToBackend: "b2",
	}, "spread")
	if ok {
		t.Error("expected failure when source backend missing")
	}
}

// TestRebalance_UnknownStrategy verifies the rebalance unknown strategy path by exercising gomock.NewController, ops.EXPECT, r.Rebalance.
func TestRebalance_UnknownStrategy(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{quotaStats: map[string]core.QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000},
		"b2": {BytesUsed: 100, BytesLimit: 1000},
	}}
	ops.EXPECT().BackendOrder().Return([]string{"b1", "b2"}).AnyTimes()
	ops.EXPECT().Usage().Return(unlimitedUsage()).AnyTimes()

	r := NewRebalancer(ops, pl, ms)
	_, err := r.Rebalance(context.Background(), config.RebalanceConfig{
		Strategy: "bogus", Threshold: 0.01, BatchSize: 10,
	}, nil)
	if err == nil {
		t.Error("expected error for unknown strategy")
	}
}

// TestRebalanceMove_ProgressLabel pins the streamed line's shape: a move is
// only meaningful as the object plus where it travelled between.
func TestRebalanceMove_ProgressLabel(t *testing.T) {
	t.Parallel()
	mv := RebalanceMove{ObjectKey: "photos/a.jpg", FromBackend: "oci", ToBackend: "e2", SizeBytes: 4096}
	if got, want := mv.progressLabel(), "photos/a.jpg  oci -> e2"; got != want {
		t.Errorf("progressLabel() = %q, want %q", got, want)
	}
}

// TestRebalance_SkipsWithinThreshold asserts a balanced fleet reports why it
// did nothing rather than a zero move count, which reads as a completed pass.
func TestRebalance_SkipsWithinThreshold(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	ms := &mockMetadataStore{quotaStats: map[string]core.QuotaStat{
		"b1": {BytesUsed: 500, BytesLimit: 1000},
		"b2": {BytesUsed: 500, BytesLimit: 1000},
	}}
	ops.EXPECT().BackendOrder().Return([]string{"b1", "b2"}).AnyTimes()
	ops.EXPECT().Usage().Return(unlimitedUsage()).AnyTimes()

	r := NewRebalancer(ops, NewMockPlacement(ctrl), ms)
	sum, err := r.Rebalance(context.Background(), config.RebalanceConfig{
		Strategy: "spread", Threshold: 0.1, BatchSize: 10,
	}, nil)
	if err != nil {
		t.Fatalf("Rebalance: %v", err)
	}
	if sum.SkipReason != SkipReasonWithinThreshold {
		t.Errorf("SkipReason = %q, want the threshold reason", sum.SkipReason)
	}
	if sum.Succeeded != 0 {
		t.Errorf("Succeeded = %d, want 0 on a skip", sum.Succeeded)
	}
}

// unlimitedUsage returns a tracker with no configured limits, so WithinLimits
// always passes. Planner tests that are not about budgets use it so the usage
// check never changes their outcome.
func unlimitedUsage() *counter.UsageTracker {
	return counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil)
}

// -------------------------------------------------------------------------
// USAGE BUDGET
// -------------------------------------------------------------------------

// TestUsageBudget_ChecksEgressOnSourceAndIngressOnDestination pins the
// asymmetry a move has: reading the object spends the source's egress, writing
// it spends the destination's ingress. A backend can have headroom in one and
// none in the other, so a single combined check would be wrong in both
// directions.
func TestUsageBudget_ChecksEgressOnSourceAndIngressOnDestination(t *testing.T) {
	t.Parallel()

	cb := counter.NewLocalCounterBackend([]string{"src", "dst"})
	tracker := counter.NewUsageTracker(cb, map[string]core.UsageLimits{
		"src": {EgressByteLimit: 100},
		"dst": {IngressByteLimit: 100},
	})
	b := newUsageBudget(tracker)

	if !b.allows("src", "dst", 50) {
		t.Fatal("a move inside both allowances should be permitted")
	}

	// Exhaust the source's egress only. The destination still has ingress
	// headroom, so a combined check would wrongly permit this.
	tracker.Record("src", s3op.GetObject, 100, 0)
	if b.allows("src", "dst", 50) {
		t.Error("a source over its egress allowance must not be read from")
	}

	// And the mirror: destination ingress exhausted, source egress free.
	b2 := newUsageBudget(counter.NewUsageTracker(
		counter.NewLocalCounterBackend([]string{"src2", "dst2"}),
		map[string]core.UsageLimits{"dst2": {IngressByteLimit: 10}},
	))
	b2.usage.Record("dst2", s3op.PutObject, 0, 100)
	if b2.allows("src2", "dst2", 5) {
		t.Error("a destination over its ingress allowance must not be written to")
	}
}

// TestUsageBudget_AccumulatesAcrossAPlan is the case a per-move check alone
// gets wrong: fifty moves that each fit inside the remaining allowance can
// still exceed it together, because none of them has been executed yet when the
// plan is built.
func TestUsageBudget_AccumulatesAcrossAPlan(t *testing.T) {
	t.Parallel()

	cb := counter.NewLocalCounterBackend([]string{"src", "dst"})
	tracker := counter.NewUsageTracker(cb, map[string]core.UsageLimits{
		"src": {EgressByteLimit: 100},
	})
	b := newUsageBudget(tracker)

	var planned int
	for range 10 {
		if !b.allows("src", "dst", 30) {
			break
		}
		b.commit("src", "dst", 30)
		planned++
	}

	// 3 x 30 fits inside 100; a fourth would not.
	if planned != 3 {
		t.Errorf("planned %d moves of 30 bytes against a 100-byte allowance, want 3", planned)
	}
}

// TestUsageBudget_UnlimitedWhenNoTracker keeps the budget inert for callers
// that have no usage tracking wired, rather than blocking every move.
func TestUsageBudget_UnlimitedWhenNoTracker(t *testing.T) {
	t.Parallel()

	var nilBudget *usageBudget
	if !nilBudget.allows("a", "b", 1<<40) {
		t.Error("a nil budget must not block moves")
	}
	if !newUsageBudget(nil).allows("a", "b", 1<<40) {
		t.Error("a budget with no tracker must not block moves")
	}
	// commit on a nil budget is a no-op rather than a panic, so a planner
	// wired without usage tracking still runs.
	nilBudget.commit("a", "b", 10)
}

// exhaustedIngress builds a tracker where dest has already spent its ingress
// allowance, so no further bytes may be written to it.
func exhaustedIngress(dest string, names ...string) *counter.UsageTracker {
	tracker := counter.NewUsageTracker(counter.NewLocalCounterBackend(names),
		map[string]core.UsageLimits{dest: {IngressByteLimit: 10}})
	tracker.Record(dest, s3op.PutObject, 0, 1000)
	return tracker
}

// TestPlanPackTight_DeclinesMovesIntoAnOverBudgetDestination proves the pack
// planner consults the budget, not merely that the budget works. Byte quota
// alone would happily plan these moves: the destination has plenty of free
// space and is the more-utilized backend pack pulls toward. Only the usage
// limit stops them.
func TestPlanPackTight_DeclinesMovesIntoAnOverBudgetDestination(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	ms := &mockMetadataStore{
		objectsByBackend: map[string][]core.ObjectLocation{"b2": {
			{ObjectKey: "k1", BackendName: "b2", SizeBytes: 50},
			{ObjectKey: "k2", BackendName: "b2", SizeBytes: 50},
		}},
		getBackendsForKeysResp: map[string][]string{"k1": {"b2"}, "k2": {"b2"}},
	}
	ops.EXPECT().BackendOrder().Return([]string{"b1", "b2"}).AnyTimes()
	ops.EXPECT().Usage().Return(exhaustedIngress("b1", "b1", "b2")).AnyTimes()

	r := NewRebalancer(ops, NewMockPlacement(ctrl), ms)
	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 800, BytesLimit: 1000}, // room for both objects
		"b2": {BytesUsed: 100, BytesLimit: 1000},
	}

	plan, err := r.PlanPackTight(context.Background(), stats, 10)
	if err != nil {
		t.Fatalf("PlanPackTight: %v", err)
	}
	if len(plan) != 0 {
		t.Errorf("planned %d moves into a destination over its ingress allowance, want 0: %+v",
			len(plan), plan)
	}
}

// TestPlanSpreadEven_DeclinesMovesIntoAnOverBudgetDestination is the same
// assertion for the spread planner, which selects destinations by a different
// route and so needs its own proof that the budget is consulted.
func TestPlanSpreadEven_DeclinesMovesIntoAnOverBudgetDestination(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	ms := &mockMetadataStore{
		objectsByBackend: map[string][]core.ObjectLocation{"b1": {
			{ObjectKey: "k1", BackendName: "b1", SizeBytes: 100},
			{ObjectKey: "k2", BackendName: "b1", SizeBytes: 100},
		}},
		getBackendsForKeysResp: map[string][]string{"k1": {"b1"}, "k2": {"b1"}},
	}
	ops.EXPECT().BackendOrder().Return([]string{"b1", "b2"}).AnyTimes()
	ops.EXPECT().Usage().Return(exhaustedIngress("b2", "b1", "b2")).AnyTimes()

	r := NewRebalancer(ops, NewMockPlacement(ctrl), ms)
	// b1 heavily over-utilized, b2 empty: spread wants to move into b2.
	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000},
		"b2": {BytesUsed: 0, BytesLimit: 1000},
	}

	plan, err := r.PlanSpreadEven(context.Background(), stats, 10)
	if err != nil {
		t.Fatalf("PlanSpreadEven: %v", err)
	}
	if len(plan) != 0 {
		t.Errorf("planned %d moves into a destination over its ingress allowance, want 0: %+v",
			len(plan), plan)
	}
}

// TestPlanSpreadEven_StillPlansWhenBudgetAllows is the control for the two
// tests above: with the same fleet and no usage limits the planner does produce
// moves, so their empty plans are the budget's doing and not a broken fixture.
func TestPlanSpreadEven_StillPlansWhenBudgetAllows(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	ms := &mockMetadataStore{
		objectsByBackend: map[string][]core.ObjectLocation{"b1": {
			{ObjectKey: "k1", BackendName: "b1", SizeBytes: 100},
			{ObjectKey: "k2", BackendName: "b1", SizeBytes: 100},
		}},
		getBackendsForKeysResp: map[string][]string{"k1": {"b1"}, "k2": {"b1"}},
	}
	ops.EXPECT().BackendOrder().Return([]string{"b1", "b2"}).AnyTimes()
	ops.EXPECT().Usage().Return(unlimitedUsage()).AnyTimes()

	r := NewRebalancer(ops, NewMockPlacement(ctrl), ms)
	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000},
		"b2": {BytesUsed: 0, BytesLimit: 1000},
	}

	plan, err := r.PlanSpreadEven(context.Background(), stats, 10)
	if err != nil {
		t.Fatalf("PlanSpreadEven: %v", err)
	}
	if len(plan) == 0 {
		t.Fatal("expected moves with no usage limits configured; the budget tests prove nothing otherwise")
	}
}
