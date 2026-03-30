package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"go.uber.org/mock/gomock"
)

func TestRebalancer_SetConfig_RoundTrip(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	r := NewRebalancer(NewMockOps(ctrl))
	if r.Config() != nil {
		t.Fatal("expected nil config before set")
	}
	cfg := &config.RebalanceConfig{Strategy: "spread", Threshold: 0.1}
	r.SetConfig(cfg)
	if got := r.Config(); got.Strategy != "spread" {
		t.Errorf("Config().Strategy = %q, want spread", got.Strategy)
	}
}

func TestExceedsThreshold_BelowThreshold(t *testing.T) {
	t.Parallel()
	stats := map[string]store.QuotaStat{
		"b1": {BytesUsed: 500, BytesLimit: 1000},
		"b2": {BytesUsed: 450, BytesLimit: 1000},
	}
	if ExceedsThreshold(stats, []string{"b1", "b2"}, 0.1) {
		t.Error("5% spread should not exceed 10% threshold")
	}
}

func TestExceedsThreshold_AboveThreshold(t *testing.T) {
	t.Parallel()
	stats := map[string]store.QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000},
		"b2": {BytesUsed: 100, BytesLimit: 1000},
	}
	if !ExceedsThreshold(stats, []string{"b1", "b2"}, 0.1) {
		t.Error("80% spread should exceed 10% threshold")
	}
}

func TestExceedsThreshold_SingleBackend(t *testing.T) {
	t.Parallel()
	stats := map[string]store.QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000},
	}
	if ExceedsThreshold(stats, []string{"b1"}, 0.1) {
		t.Error("single backend cannot exceed threshold")
	}
}

func TestExceedsThreshold_EmptyStats(t *testing.T) {
	t.Parallel()
	if ExceedsThreshold(nil, []string{"b1", "b2"}, 0.1) {
		t.Error("empty stats should not exceed threshold")
	}
}

func TestPlanSpreadEven_BalancedSkipped(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	ops.EXPECT().BackendOrder().Return([]string{"b1", "b2"}).AnyTimes()

	r := NewRebalancer(ops)
	// Equal utilization: no moves needed
	stats := map[string]store.QuotaStat{
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

func TestPlanSpreadEven_ImbalancedPlansMoves(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	ms := &mockMetadataStore{
		objectsByBackend: map[string][]store.ObjectLocation{
			"b1": {{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100}},
		},
	}

	ops.EXPECT().BackendOrder().Return([]string{"b1", "b2"}).AnyTimes()
	ops.EXPECT().Store().Return(ms).AnyTimes()

	r := NewRebalancer(ops)
	// b1 at 80%, b2 at 20% → target ~50%, b1 has excess
	stats := map[string]store.QuotaStat{
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
			t.Errorf("move should be b1→b2, got %s→%s", mv.FromBackend, mv.ToBackend)
		}
	}
}

func TestExecuteOneMove_Success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)
	ms := &mockMetadataStore{moveSize: 100}

	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe, "b2": dstBe}).AnyTimes()
	ops.EXPECT().StreamCopy(gomock.Any(), srcBe, dstBe, "key1").Return(nil)
	ops.EXPECT().Store().Return(ms).AnyTimes()
	ops.EXPECT().DeleteOrEnqueue(gomock.Any(), srcBe, "b1", "key1", "rebalance_source_delete", int64(100))
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()

	r := NewRebalancer(ops)
	ok := r.ExecuteOneMove(context.Background(), RebalanceMove{
		ObjectKey: "key1", FromBackend: "b1", ToBackend: "b2", SizeBytes: 100,
	}, "spread")
	if !ok {
		t.Error("expected successful move")
	}
}

func TestExecuteOneMove_StreamCopyFails(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe, "b2": dstBe}).AnyTimes()
	ops.EXPECT().StreamCopy(gomock.Any(), srcBe, dstBe, "key1").Return(errors.New("timeout"))

	r := NewRebalancer(ops)
	ok := r.ExecuteOneMove(context.Background(), RebalanceMove{
		ObjectKey: "key1", FromBackend: "b1", ToBackend: "b2", SizeBytes: 100,
	}, "spread")
	if ok {
		t.Error("expected failed move on stream copy error")
	}
}

func TestExecuteOneMove_MoveLocationFails(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)
	ms := &mockMetadataStore{moveSize: 0} // 0 = object was deleted/moved

	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe, "b2": dstBe}).AnyTimes()
	ops.EXPECT().StreamCopy(gomock.Any(), srcBe, dstBe, "key1").Return(nil)
	ops.EXPECT().Store().Return(ms).AnyTimes()
	ops.EXPECT().DeleteOrEnqueue(gomock.Any(), dstBe, "b2", "key1", "rebalance_stale_orphan", int64(100))
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()

	r := NewRebalancer(ops)
	ok := r.ExecuteOneMove(context.Background(), RebalanceMove{
		ObjectKey: "key1", FromBackend: "b1", ToBackend: "b2", SizeBytes: 100,
	}, "spread")
	if ok {
		t.Error("expected failed move when object already gone")
	}
}

func TestExecuteOneMove_SourceBackendNotFound(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{}).AnyTimes()

	r := NewRebalancer(ops)
	ok := r.ExecuteOneMove(context.Background(), RebalanceMove{
		ObjectKey: "key1", FromBackend: "gone", ToBackend: "b2",
	}, "spread")
	if ok {
		t.Error("expected failure when source backend missing")
	}
}

func TestRebalance_UnknownStrategy(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	ms := &mockMetadataStore{quotaStats: map[string]store.QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000},
		"b2": {BytesUsed: 100, BytesLimit: 1000},
	}}
	ops.EXPECT().Store().Return(ms).AnyTimes()
	ops.EXPECT().BackendOrder().Return([]string{"b1", "b2"}).AnyTimes()

	r := NewRebalancer(ops)
	_, err := r.Rebalance(context.Background(), config.RebalanceConfig{
		Strategy: "bogus", Threshold: 0.01, BatchSize: 10,
	})
	if err == nil {
		t.Error("expected error for unknown strategy")
	}
}
