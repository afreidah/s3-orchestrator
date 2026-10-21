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
	"fmt"
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

// rebalanceFleet is the two-backend fleet most rebalance tests plan across,
// every backend in it a write target.
var rebalanceFleet = []string{"b1", "b2"}

// TestRebalancer_SetConfig_RoundTrip verifies the rebalancer set config round trip contract.
// Asserts that Config().Strategy = , want spread.
func TestRebalancer_SetConfig_RoundTrip(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	r := NewRebalancer(newMockOps(ctrl), NewMockPlacement(ctrl), &mockMetadataStore{})
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
	ops := newMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	expectBackendOrder(ops, rebalanceFleet, rebalanceFleet)
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
	ops := newMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{
		objectsByBackend: map[string][]core.ObjectLocation{
			"b1": {{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100}},
		},
	}

	expectBackendOrder(ops, rebalanceFleet, rebalanceFleet)
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
		if mv.Source.BackendName != "b1" || mv.ToBackend != "b2" {
			t.Errorf("move should be b1->b2, got %s->%s", mv.Source.BackendName, mv.ToBackend)
		}
	}
}

// TestRebalance_PlansNoMovesOntoANonWriteTarget verifies both strategies leave
// out a destination that is not a write target, such as one refusing reads:
// moving a copy there would leave it unreadable.
func TestRebalance_PlansNoMovesOntoANonWriteTarget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		plan  func(*Rebalancer, map[string]core.QuotaStat) ([]RebalanceMove, error)
		stats map[string]core.QuotaStat
		src   string
	}{
		{
			name: "spread",
			plan: func(r *Rebalancer, s map[string]core.QuotaStat) ([]RebalanceMove, error) {
				return r.PlanSpreadEven(context.Background(), s, 10)
			},
			stats: map[string]core.QuotaStat{"b1": {BytesUsed: 800, BytesLimit: 1000}, "b2": {BytesUsed: 200, BytesLimit: 1000}},
			src:   "b1",
		},
		{
			name: "pack",
			plan: func(r *Rebalancer, s map[string]core.QuotaStat) ([]RebalanceMove, error) {
				return r.PlanPackTight(context.Background(), s, 10)
			},
			stats: map[string]core.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}, "b2": {BytesUsed: 800, BytesLimit: 1000}},
			src:   "b1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctrl := gomock.NewController(t)
			ops := newMockOps(ctrl)
			ms := &mockMetadataStore{
				objectsByBackend: map[string][]core.ObjectLocation{
					tc.src: {{ObjectKey: "key1", BackendName: tc.src, SizeBytes: 100}},
				},
			}
			expectBackendOrder(ops, rebalanceFleet, []string{tc.src})
			ops.EXPECT().Usage().Return(unlimitedUsage()).AnyTimes()

			plan, err := tc.plan(NewRebalancer(ops, NewMockPlacement(ctrl), ms), tc.stats)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if len(plan) != 0 {
				t.Errorf("planned %+v onto b2, which takes no writes", plan)
			}
		})
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
	ops := newMockOps(ctrl)
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
	expectBackendOrder(ops, rebalanceFleet, rebalanceFleet)
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
		if mv.Source.ObjectKey == "k2" {
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
	ops := newMockOps(ctrl)
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
	expectBackendOrder(ops, rebalanceFleet, rebalanceFleet)
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
		if mv.Source.ObjectKey == "k2" && mv.ToBackend == "b1" {
			t.Errorf("k2 already on b1 and must not be planned for move to b1; plan=%+v", plan)
		}
	}
}

// TestExecuteOneMove_Success verifies that the rebalancer dispatches a
// single MoveObject call to the shared primitive on the happy path, under
// the rebalancer's move profile; the writepath.Coordinator owns the copy
// and the cleanup.
func TestExecuteOneMove_Success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockOps(ctrl)
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
	move := planMove("key1", "b1", "b2", 100)
	if !r.ExecuteOneMove(context.Background(), move, "spread") {
		t.Error("expected successful move")
	}
	if got == nil {
		t.Fatal("MoveObject was not called")
	}
	if got.Profile != writepath.RebalanceMove {
		t.Errorf("MoveRequest.Profile = %+v, want RebalanceMove", got.Profile)
	}
	if got.Source != move.Source || got.SrcBackend != srcBe || got.DestBackend != dstBe || got.DestName != "b2" {
		t.Errorf("MoveRequest = %+v, want the planned source copy moved b1 -> b2", got)
	}
}

// TestExecuteOneMove_MoveObjectFails pins the failure-propagation
// branch: a MoveObject error that is not a declined placement bumps the
// error telemetry counter and returns false.
func TestExecuteOneMove_MoveObjectFails(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe, "b2": dstBe}).AnyTimes()
	pl.EXPECT().MoveObject(gomock.Any(), gomock.Any()).Return(int64(0), errors.New("stream copy: timeout"))

	r := NewRebalancer(ops, pl, ms)
	if r.ExecuteOneMove(context.Background(), planMove("key1", "b1", "b2", 100), "spread") {
		t.Error("expected failed move on MoveObject error")
	}
}

// TestExecuteOneMove_PlacementDeclined pins the declined branch: MoveObject
// reports the source changed or the target refused, and the helper treats it
// as a skip rather than a failed move.
func TestExecuteOneMove_PlacementDeclined(t *testing.T) {
	t.Parallel()
	for _, declined := range []error{core.ErrCopyChanged, core.ErrTargetRefused} {
		t.Run(declined.Error(), func(t *testing.T) {
			t.Parallel()
			ctrl := gomock.NewController(t)
			ops := newMockOps(ctrl)
			pl := NewMockPlacement(ctrl)

			srcBe := backendtest.NewMockObjectBackend(ctrl)
			dstBe := backendtest.NewMockObjectBackend(ctrl)

			ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe, "b2": dstBe}).AnyTimes()
			pl.EXPECT().MoveObject(gomock.Any(), gomock.Any()).
				Return(int64(0), fmt.Errorf("place copy b1 -> b2: %w", declined))

			r := NewRebalancer(ops, pl, &mockMetadataStore{})
			if r.ExecuteOneMove(context.Background(), planMove("key1", "b1", "b2", 100), "spread") {
				t.Error("expected no move when the placement is declined")
			}
		})
	}
}

// TestExecuteOneMove_SourceBackendNotFound verifies the execute one move source backend not found path by exercising gomock.NewController, ops.EXPECT, r.ExecuteOneMove.
func TestExecuteOneMove_SourceBackendNotFound(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{}).AnyTimes()

	r := NewRebalancer(ops, pl, ms)
	ok := r.ExecuteOneMove(context.Background(), planMove("key1", "gone", "b2", 0), "spread")
	if ok {
		t.Error("expected failure when source backend missing")
	}
}

// TestRebalance_UnknownStrategy verifies the rebalance unknown strategy path by exercising gomock.NewController, ops.EXPECT, r.Rebalance.
func TestRebalance_UnknownStrategy(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{quotaStats: map[string]core.QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000},
		"b2": {BytesUsed: 100, BytesLimit: 1000},
	}}
	expectBackendOrder(ops, rebalanceFleet, rebalanceFleet)
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
	mv := planMove("photos/a.jpg", "oci", "e2", 4096)
	if got, want := mv.progressLabel(), "photos/a.jpg  oci -> e2"; got != want {
		t.Errorf("progressLabel() = %q, want %q", got, want)
	}
}

// TestRebalance_SkipsWithinThreshold asserts a balanced fleet reports why it
// did nothing rather than a zero move count, which reads as a completed pass.
func TestRebalance_SkipsWithinThreshold(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockOps(ctrl)
	ms := &mockMetadataStore{quotaStats: map[string]core.QuotaStat{
		"b1": {BytesUsed: 500, BytesLimit: 1000},
		"b2": {BytesUsed: 500, BytesLimit: 1000},
	}}
	expectBackendOrder(ops, rebalanceFleet, rebalanceFleet)
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
	tracker.Record(t.Context(), "src", s3op.GetObject, 100, 0)
	if b.allows("src", "dst", 50) {
		t.Error("a source over its egress allowance must not be read from")
	}

	// And the mirror: destination ingress exhausted, source egress free.
	b2 := newUsageBudget(counter.NewUsageTracker(
		counter.NewLocalCounterBackend([]string{"src2", "dst2"}),
		map[string]core.UsageLimits{"dst2": {IngressByteLimit: 10}},
	))
	b2.usage.Record(t.Context(), "dst2", s3op.PutObject, 0, 100)
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
	tracker.Record(context.Background(), dest, s3op.PutObject, 0, 1000)
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
	ops := newMockOps(ctrl)
	ms := &mockMetadataStore{
		objectsByBackend: map[string][]core.ObjectLocation{"b2": {
			{ObjectKey: "k1", BackendName: "b2", SizeBytes: 50},
			{ObjectKey: "k2", BackendName: "b2", SizeBytes: 50},
		}},
		getBackendsForKeysResp: map[string][]string{"k1": {"b2"}, "k2": {"b2"}},
	}
	expectBackendOrder(ops, rebalanceFleet, rebalanceFleet)
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
	ops := newMockOps(ctrl)
	ms := &mockMetadataStore{
		objectsByBackend: map[string][]core.ObjectLocation{"b1": {
			{ObjectKey: "k1", BackendName: "b1", SizeBytes: 100},
			{ObjectKey: "k2", BackendName: "b1", SizeBytes: 100},
		}},
		getBackendsForKeysResp: map[string][]string{"k1": {"b1"}, "k2": {"b1"}},
	}
	expectBackendOrder(ops, rebalanceFleet, rebalanceFleet)
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
	ops := newMockOps(ctrl)
	ms := &mockMetadataStore{
		objectsByBackend: map[string][]core.ObjectLocation{"b1": {
			{ObjectKey: "k1", BackendName: "b1", SizeBytes: 100},
			{ObjectKey: "k2", BackendName: "b1", SizeBytes: 100},
		}},
		getBackendsForKeysResp: map[string][]string{"k1": {"b1"}, "k2": {"b1"}},
	}
	expectBackendOrder(ops, rebalanceFleet, rebalanceFleet)
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
