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

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"go.uber.org/mock/gomock"
)

// TestRebalancer_SetConfig_RoundTrip verifies the rebalancer set config round trip contract.
// Asserts that Config().Strategy = , want spread.
func TestRebalancer_SetConfig_RoundTrip(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	r := NewRebalancer(NewMockOps(ctrl), &mockMetadataStore{})
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
	ms := &mockMetadataStore{}

	ops.EXPECT().BackendOrder().Return([]string{"b1", "b2"}).AnyTimes()

	r := NewRebalancer(ops, ms)
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
	ms := &mockMetadataStore{
		objectsByBackend: map[string][]core.ObjectLocation{
			"b1": {{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100}},
		},
	}

	ops.EXPECT().BackendOrder().Return([]string{"b1", "b2"}).AnyTimes()

	r := NewRebalancer(ops, ms)
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

	r := NewRebalancer(ops, ms)
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

	r := NewRebalancer(ops, ms)
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

// TestExecuteOneMove_Success verifies the execute one move success path by exercising gomock.NewController, backendtest.NewMockObjectBackend, ops.EXPECT.
func TestExecuteOneMove_Success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)
	ms := &mockMetadataStore{moveSize: 100}

	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe, "b2": dstBe}).AnyTimes()
	ops.EXPECT().StreamCopy(gomock.Any(), srcBe, dstBe, "key1").Return(nil)
	ops.EXPECT().DeleteOrEnqueue(gomock.Any(), srcBe, "b1", "key1", "rebalance_source_delete", int64(100))
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()

	r := NewRebalancer(ops, ms)
	ok := r.ExecuteOneMove(context.Background(), RebalanceMove{
		ObjectKey: "key1", FromBackend: "b1", ToBackend: "b2", SizeBytes: 100,
	}, "spread")
	if !ok {
		t.Error("expected successful move")
	}
}

// TestExecuteOneMove_StreamCopyFails verifies the execute one move stream copy fails path by exercising gomock.NewController, backendtest.NewMockObjectBackend, ops.EXPECT.
func TestExecuteOneMove_StreamCopyFails(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	ms := &mockMetadataStore{}

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe, "b2": dstBe}).AnyTimes()
	ops.EXPECT().StreamCopy(gomock.Any(), srcBe, dstBe, "key1").Return(errors.New("timeout"))

	r := NewRebalancer(ops, ms)
	ok := r.ExecuteOneMove(context.Background(), RebalanceMove{
		ObjectKey: "key1", FromBackend: "b1", ToBackend: "b2", SizeBytes: 100,
	}, "spread")
	if ok {
		t.Error("expected failed move on stream copy error")
	}
}

// TestExecuteOneMove_MoveLocationFails verifies the execute one move move location fails path by exercising gomock.NewController, backendtest.NewMockObjectBackend, ops.EXPECT.
func TestExecuteOneMove_MoveLocationFails(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)
	ms := &mockMetadataStore{moveSize: 0} // 0 = object was deleted/moved

	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe, "b2": dstBe}).AnyTimes()
	ops.EXPECT().StreamCopy(gomock.Any(), srcBe, dstBe, "key1").Return(nil)
	ops.EXPECT().DeleteOrEnqueue(gomock.Any(), dstBe, "b2", "key1", "rebalance_stale_orphan", int64(100))
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()

	r := NewRebalancer(ops, ms)
	ok := r.ExecuteOneMove(context.Background(), RebalanceMove{
		ObjectKey: "key1", FromBackend: "b1", ToBackend: "b2", SizeBytes: 100,
	}, "spread")
	if ok {
		t.Error("expected failed move when object already gone")
	}
}

// TestExecuteOneMove_SourceBackendNotFound verifies the execute one move source backend not found path by exercising gomock.NewController, ops.EXPECT, r.ExecuteOneMove.
func TestExecuteOneMove_SourceBackendNotFound(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	ms := &mockMetadataStore{}

	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{}).AnyTimes()

	r := NewRebalancer(ops, ms)
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
	ms := &mockMetadataStore{quotaStats: map[string]core.QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000},
		"b2": {BytesUsed: 100, BytesLimit: 1000},
	}}
	ops.EXPECT().BackendOrder().Return([]string{"b1", "b2"}).AnyTimes()

	r := NewRebalancer(ops, ms)
	_, err := r.Rebalance(context.Background(), config.RebalanceConfig{
		Strategy: "bogus", Threshold: 0.01, BatchSize: 10,
	})
	if err == nil {
		t.Error("expected error for unknown strategy")
	}
}
