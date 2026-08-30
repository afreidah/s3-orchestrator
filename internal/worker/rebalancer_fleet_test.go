// -------------------------------------------------------------------------------
// Rebalancer Tests - Move Execution Concurrency
//
// Author: Alex Freidah
//
// Unit tests for the parallel move execution in the rebalancer. Verifies that
// concurrent moves complete correctly, partial failures are handled, and
// sequential fallback (concurrency=1) works identically to the old behavior.
// -------------------------------------------------------------------------------

package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// rebalEnqueue captures EnqueueCleanup calls for rebalancer tests.
type rebalEnqueue struct {
	mu    sync.Mutex
	calls []core.CleanupItem
}

// stubRebalEnqueue returns a DoAndReturn that captures into re.
func stubRebalEnqueue(re *rebalEnqueue) func(context.Context, string, string, string, int64) error {
	return func(_ context.Context, backend, key, reason string, size int64) error {
		re.mu.Lock()
		defer re.mu.Unlock()
		re.calls = append(re.calls, core.CleanupItem{
			BackendName: backend, ObjectKey: key, Reason: reason, SizeBytes: size,
		})
		return nil
	}
}

// stubMoveSize returns a MoveObjectLocation stub returning size+nil.
func stubMoveSize(size int64) func(context.Context, string, string, string) (int64, error) {
	return func(_ context.Context, _, _, _ string) (int64, error) {
		return size, nil
	}
}

// stubMoveErr returns a MoveObjectLocation stub returning 0+err.
func stubMoveErr(err error) func(context.Context, string, string, string) (int64, error) {
	return func(_ context.Context, _, _, _ string) (int64, error) {
		return 0, err
	}
}

// rebalanceStoreWithMoveSize returns a store with MoveObjectLocation
// returning the given size on success.
func rebalanceStoreWithMoveSize(t *testing.T, size int64) *storetest.MockMetadataStore {
	t.Helper()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().MoveObjectLocation(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubMoveSize(size)).AnyTimes()
	storetest.Permissive(store)
	return store
}

// delayedGetBackend adds a configurable GetObject delay to the shared
// in-memory backend, so the concurrency tests can prove moves overlap rather
// than run one after another.
type delayedGetBackend struct {
	*backendtest.InMemory
	delay time.Duration
}

// newDelayedGetBackend constructs a delayed-Get backend.
func newDelayedGetBackend(delay time.Duration) *delayedGetBackend {
	return &delayedGetBackend{InMemory: backendtest.NewInMemory(), delay: delay}
}

var _ backend.ObjectBackend = (*delayedGetBackend)(nil)

// GetObject sleeps for the configured delay before delegating.
func (m *delayedGetBackend) GetObject(ctx context.Context, key, rangeHeader string) (*backend.GetObjectResult, error) {
	time.Sleep(m.delay)
	return m.InMemory.GetObject(ctx, key, rangeHeader)
}

// seedObject pre-populates the backing store without going through the write
// path, for a test that only cares that the object is already there.
func (m *delayedGetBackend) seedObject(key string, data []byte) {
	m.Put(key, &backendtest.Object{Data: data, ContentType: "application/octet-stream"})
}

// TestExecuteMoves_Concurrent pins parallel execution.
func TestExecuteMoves_Concurrent(t *testing.T) {
	t.Parallel()
	src := newDelayedGetBackend(50 * time.Millisecond)
	dest := newDelayedGetBackend(0)
	for i := range 5 {
		src.seedObject(fmt.Sprintf("key%d", i), []byte("data"))
	}

	store := rebalanceStoreWithMoveSize(t, 4)
	obs := map[string]backend.ObjectBackend{"src": src, "dest": dest}
	w := newRebalancerFor(t, store, obs, &fleetOpts{Order: []string{"src", "dest"}})

	var plan []RebalanceMove
	for i := range 5 {
		plan = append(plan, RebalanceMove{
			ObjectKey:   fmt.Sprintf("key%d", i),
			FromBackend: "src",
			ToBackend:   "dest",
			SizeBytes:   4,
		})
	}

	start := time.Now()
	moved := w.ExecuteMoves(context.Background(), plan, "spread", 3, nil).Succeeded
	elapsed := time.Since(start)

	if moved != 5 {
		t.Errorf("moved = %d, want 5", moved)
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("elapsed = %v, expected < 200ms with concurrency 3", elapsed)
	}
	for i := range 5 {
		if !dest.Has(fmt.Sprintf("key%d", i)) {
			t.Errorf("key%d not found on destination", i)
		}
	}
}

// TestExecuteMoves_PartialFailure pins partial-success counting.
func TestExecuteMoves_PartialFailure(t *testing.T) {
	t.Parallel()
	src := newDelayedGetBackend(0)
	dest := newDelayedGetBackend(0)
	src.seedObject("ok1", []byte("data"))
	src.seedObject("ok2", []byte("data"))

	store := rebalanceStoreWithMoveSize(t, 4)
	obs := map[string]backend.ObjectBackend{"src": src, "dest": dest}
	w := newRebalancerFor(t, store, obs, &fleetOpts{Order: []string{"src", "dest"}})

	plan := []RebalanceMove{
		{ObjectKey: "ok1", FromBackend: "src", ToBackend: "dest", SizeBytes: 4},
		{ObjectKey: "fail", FromBackend: "src", ToBackend: "dest", SizeBytes: 4},
		{ObjectKey: "ok2", FromBackend: "src", ToBackend: "dest", SizeBytes: 4},
	}

	moved := w.ExecuteMoves(context.Background(), plan, "spread", 3, nil).Succeeded
	if moved != 2 {
		t.Errorf("moved = %d, want 2 (one should fail)", moved)
	}
}

// TestExecuteMoves_SequentialFallback pins concurrency=1 behaviour.
func TestExecuteMoves_SequentialFallback(t *testing.T) {
	t.Parallel()
	src := newDelayedGetBackend(0)
	dest := newDelayedGetBackend(0)
	src.seedObject("a", []byte("hello"))
	src.seedObject("b", []byte("world"))

	store := rebalanceStoreWithMoveSize(t, 5)
	obs := map[string]backend.ObjectBackend{"src": src, "dest": dest}
	w := newRebalancerFor(t, store, obs, &fleetOpts{Order: []string{"src", "dest"}})

	plan := []RebalanceMove{
		{ObjectKey: "a", FromBackend: "src", ToBackend: "dest", SizeBytes: 5},
		{ObjectKey: "b", FromBackend: "src", ToBackend: "dest", SizeBytes: 5},
	}
	moved := w.ExecuteMoves(context.Background(), plan, "pack", 1, nil).Succeeded
	if moved != 2 {
		t.Errorf("moved = %d, want 2", moved)
	}
	if !dest.Has("a") || !dest.Has("b") {
		t.Error("expected both objects on destination")
	}
}

// TestExceedsThreshold_AtThreshold pins the threshold-met case.
func TestExceedsThreshold_AtThreshold(t *testing.T) {
	t.Parallel()
	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 800, BytesLimit: 1000},
		"b2": {BytesUsed: 200, BytesLimit: 1000},
	}
	if !ExceedsThreshold(stats, []string{"b1", "b2"}, 0.50) {
		t.Error("60% spread should exceed 50% threshold")
	}
}

// TestExceedsThreshold_ZeroLimitSkipped pins the unlimited-skip
// behaviour.
func TestExceedsThreshold_ZeroLimitSkipped(t *testing.T) {
	t.Parallel()
	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 800, BytesLimit: 1000},
		"b2": {BytesUsed: 0, BytesLimit: 0},
	}
	if ExceedsThreshold(stats, []string{"b1", "b2"}, 0.10) {
		t.Error("zero-limit backends should be skipped")
	}
}

// TestExceedsThreshold_MissingStatsSkipped pins the missing-stats
// fallback.
func TestExceedsThreshold_MissingStatsSkipped(t *testing.T) {
	t.Parallel()
	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 800, BytesLimit: 1000},
	}
	if ExceedsThreshold(stats, []string{"b1", "b2"}, 0.10) {
		t.Error("missing stats should be skipped, leaving single backend")
	}
}

// TestRebalance_QuotaStatsError surfaces a quota-stats failure.
func TestRebalance_QuotaStatsError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(nil, fmt.Errorf("db down")).AnyTimes()
	storetest.Permissive(store)

	w := newRebalancerFor(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, &fleetOpts{})

	if _, err := w.Rebalance(context.Background(), config.RebalanceConfig{
		Strategy:  "spread",
		BatchSize: 10,
		Threshold: 0.10,
	}, nil); err == nil {
		t.Fatal("expected error from GetQuotaStats failure")
	}
}

// TestRebalance_CopyMapFetchFails_Propagates pins issue #921: a
// GetObjectBackendsForKeys failure during planning must surface as a
// rebalance error rather than being silently swallowed with an empty
// copy map, which previously caused unnecessary transfers because the
// planner could not see that destinations already held copies.
func TestRebalance_CopyMapFetchFails_Propagates(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{
			"b1": {BytesUsed: 900, BytesLimit: 1000},
			"b2": {BytesUsed: 100, BytesLimit: 1000},
		}, nil).AnyTimes()
	store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{{ObjectKey: "k", BackendName: "b1", SizeBytes: 100}}, nil).AnyTimes()
	store.EXPECT().GetObjectBackendsForKeys(gomock.Any(), gomock.Any()).
		Return(nil, fmt.Errorf("db down")).AnyTimes()
	storetest.Permissive(store)

	w := newRebalancerFor(t, store, map[string]backend.ObjectBackend{
		"b1": backendtest.NewInMemory(),
		"b2": backendtest.NewInMemory(),
	}, &fleetOpts{})

	if _, err := w.Rebalance(context.Background(), config.RebalanceConfig{
		Strategy:  "spread",
		BatchSize: 10,
		Threshold: 0.10,
	}, nil); err == nil {
		t.Fatal("expected error when GetObjectBackendsForKeys fails")
	}
}

// TestRebalance_BelowThreshold_Skips short-circuits when not enough
// spread.
func TestRebalance_BelowThreshold_Skips(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{
			"b1": {BytesUsed: 500, BytesLimit: 1000},
			"b2": {BytesUsed: 490, BytesLimit: 1000},
		}, nil).AnyTimes()
	storetest.Permissive(store)

	w := newRebalancerFor(t, store, map[string]backend.ObjectBackend{
		"b1": backendtest.NewInMemory(),
		"b2": backendtest.NewInMemory(),
	}, &fleetOpts{})

	movedSum, err := w.Rebalance(context.Background(), config.RebalanceConfig{
		Strategy:  "spread",
		BatchSize: 10,
		Threshold: 0.20,
	}, nil)
	moved := movedSum.Succeeded
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if moved != 0 {
		t.Errorf("expected 0 moved (below threshold), got %d", moved)
	}
}

// TestRebalance_EmptyPlan_Skips asserts an empty plan produces zero
// moves and zero pending.
func TestRebalance_EmptyPlan_Skips(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{
			"b1": {BytesUsed: 900, BytesLimit: 1000},
			"b2": {BytesUsed: 100, BytesLimit: 1000},
		}, nil).AnyTimes()
	store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, nil).AnyTimes()
	storetest.Permissive(store)

	w := newRebalancerFor(t, store, map[string]backend.ObjectBackend{
		"b1": backendtest.NewInMemory(),
		"b2": backendtest.NewInMemory(),
	}, &fleetOpts{})

	movedSum, err := w.Rebalance(context.Background(), config.RebalanceConfig{
		Strategy:  "pack",
		BatchSize: 10,
		Threshold: 0.10,
	}, nil)
	moved := movedSum.Succeeded
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if moved != 0 {
		t.Errorf("expected 0 moved (empty plan), got %d", moved)
	}
	if pending := promtest.ToFloat64(telemetry.RebalancePending); pending != 0 {
		t.Errorf("RebalancePending = %v, want 0 (empty plan)", pending)
	}
}

// newRebalancerOver builds a Rebalancer over one in-memory backend per name,
// in the order given.
func newRebalancerOver(t *testing.T, store storetest.MetadataStore, names []string) *Rebalancer {
	t.Helper()
	backends := make(map[string]backend.ObjectBackend, len(names))
	for _, name := range names {
		backends[name] = backendtest.NewInMemory()
	}
	return newRebalancerFor(t, store, backends, &fleetOpts{Order: names})
}

// rebalanceStoreWithList returns a store that lists the supplied
// objects from ListObjectsByBackend.
func rebalanceStoreWithList(t *testing.T, objects []core.ObjectLocation, listErr error, backendsForKeys map[string][]string) *storetest.MockMetadataStore {
	t.Helper()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	if listErr != nil {
		store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(nil, listErr).AnyTimes()
	} else {
		store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(objects, nil).AnyTimes()
	}
	if backendsForKeys != nil {
		store.EXPECT().GetObjectBackendsForKeys(gomock.Any(), gomock.Any()).
			Return(backendsForKeys, nil).AnyTimes()
	}
	storetest.Permissive(store)
	return store
}

// TestPlanPackTight_MovesFromLeastToMostFull exercises the planner
// happy path.
func TestPlanPackTight_MovesFromLeastToMostFull(t *testing.T) {
	t.Parallel()
	store := rebalanceStoreWithList(t, []core.ObjectLocation{
		{ObjectKey: "small.txt", BackendName: "b2", SizeBytes: 100},
	}, nil, nil)
	w := newRebalancerOver(t, store, []string{"b1", "b2"})

	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 800, BytesLimit: 1000},
		"b2": {BytesUsed: 200, BytesLimit: 1000},
	}

	plan, err := w.PlanPackTight(context.Background(), stats, 10)
	if err != nil {
		t.Fatalf("planPackTight: %v", err)
	}
	if len(plan) == 0 {
		t.Fatal("expected at least one move")
	}
	if plan[0].FromBackend != "b2" || plan[0].ToBackend != "b1" {
		t.Errorf("expected move from b2 to b1, got %s -> %s", plan[0].FromBackend, plan[0].ToBackend)
	}
}

// TestPlanPackTight_RespectsBatchSize asserts the batch cap.
func TestPlanPackTight_RespectsBatchSize(t *testing.T) {
	t.Parallel()
	objects := make([]core.ObjectLocation, 10)
	for i := range objects {
		objects[i] = core.ObjectLocation{
			ObjectKey:   fmt.Sprintf("obj%d", i),
			BackendName: "b2",
			SizeBytes:   10,
		}
	}
	store := rebalanceStoreWithList(t, objects, nil, nil)
	w := newRebalancerOver(t, store, []string{"b1", "b2"})

	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 100, BytesLimit: 1000},
		"b2": {BytesUsed: 900, BytesLimit: 1000},
	}
	plan, err := w.PlanPackTight(context.Background(), stats, 3)
	if err != nil {
		t.Fatalf("planPackTight: %v", err)
	}
	if len(plan) > 3 {
		t.Errorf("plan has %d moves, batch limit is 3", len(plan))
	}
}

// TestPlanPackTight_SkipsLargeObjects asserts that objects too big for
// the destination's free space are skipped.
func TestPlanPackTight_SkipsLargeObjects(t *testing.T) {
	t.Parallel()
	store := rebalanceStoreWithList(t, []core.ObjectLocation{
		{ObjectKey: "huge.bin", BackendName: "b2", SizeBytes: 500},
	}, nil, nil)
	w := newRebalancerOver(t, store, []string{"b1", "b2"})

	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000},
		"b2": {BytesUsed: 200, BytesLimit: 1000},
	}
	plan, err := w.PlanPackTight(context.Background(), stats, 10)
	if err != nil {
		t.Fatalf("planPackTight: %v", err)
	}
	if len(plan) != 0 {
		t.Errorf("expected 0 moves (object too large), got %d", len(plan))
	}
}

// TestPlanPackTight_ZeroLimitBackendsSkipped asserts unlimited backends
// are skipped.
func TestPlanPackTight_ZeroLimitBackendsSkipped(t *testing.T) {
	t.Parallel()
	store := newPermissiveStore(t)
	w := newRebalancerOver(t, store, []string{"b1", "b2"})

	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 0, BytesLimit: 0},
		"b2": {BytesUsed: 500, BytesLimit: 1000},
	}
	plan, err := w.PlanPackTight(context.Background(), stats, 10)
	if err != nil {
		t.Fatalf("planPackTight: %v", err)
	}
	if len(plan) != 0 {
		t.Errorf("expected 0 moves with single quotad backend, got %d", len(plan))
	}
}

// TestPlanSpreadEven_EqualizesUtilization pins the spread planner.
func TestPlanSpreadEven_EqualizesUtilization(t *testing.T) {
	t.Parallel()
	store := rebalanceStoreWithList(t, []core.ObjectLocation{
		{ObjectKey: "obj1", BackendName: "b1", SizeBytes: 100},
		{ObjectKey: "obj2", BackendName: "b1", SizeBytes: 100},
	}, nil, nil)
	w := newRebalancerOver(t, store, []string{"b1", "b2"})

	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 800, BytesLimit: 1000},
		"b2": {BytesUsed: 200, BytesLimit: 1000},
	}
	plan, err := w.PlanSpreadEven(context.Background(), stats, 10)
	if err != nil {
		t.Fatalf("planSpreadEven: %v", err)
	}
	if len(plan) == 0 {
		t.Fatal("expected at least one move")
	}
	for _, mv := range plan {
		if mv.FromBackend != "b1" || mv.ToBackend != "b2" {
			t.Errorf("expected move from b1 to b2, got %s -> %s", mv.FromBackend, mv.ToBackend)
		}
	}
}

// TestPlanSpreadEven_SkipsWhenTargetHasCopy asserts the duplicate-target
// skip.
func TestPlanSpreadEven_SkipsWhenTargetHasCopy(t *testing.T) {
	t.Parallel()
	store := rebalanceStoreWithList(t, []core.ObjectLocation{
		{ObjectKey: "obj1", BackendName: "b1", SizeBytes: 100},
	}, nil, map[string][]string{"obj1": {"b1", "b2"}})
	w := newRebalancerOver(t, store, []string{"b1", "b2"})

	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 800, BytesLimit: 1000},
		"b2": {BytesUsed: 200, BytesLimit: 1000},
	}
	plan, err := w.PlanSpreadEven(context.Background(), stats, 10)
	if err != nil {
		t.Fatalf("planSpreadEven: %v", err)
	}
	if len(plan) != 0 {
		t.Errorf("expected 0 moves (target already has copy), got %d", len(plan))
	}
}

// TestPlanPackTight_SkipsWhenTargetHasCopy mirrors the duplicate-target
// skip on the pack planner.
func TestPlanPackTight_SkipsWhenTargetHasCopy(t *testing.T) {
	t.Parallel()
	store := rebalanceStoreWithList(t, []core.ObjectLocation{
		{ObjectKey: "obj1", BackendName: "b2", SizeBytes: 100},
	}, nil, map[string][]string{"obj1": {"b1", "b2"}})
	w := newRebalancerOver(t, store, []string{"b1", "b2"})

	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000},
		"b2": {BytesUsed: 100, BytesLimit: 1000},
	}
	plan, err := w.PlanPackTight(context.Background(), stats, 10)
	if err != nil {
		t.Fatalf("planPackTight: %v", err)
	}
	if len(plan) != 0 {
		t.Errorf("expected 0 moves (target already has copy), got %d", len(plan))
	}
}

// TestPlanSpreadEven_ZeroTotalLimit handles the empty-stats case.
func TestPlanSpreadEven_ZeroTotalLimit(t *testing.T) {
	t.Parallel()
	store := newPermissiveStore(t)
	w := newRebalancerOver(t, store, []string{"b1"})

	plan, err := w.PlanSpreadEven(context.Background(), map[string]core.QuotaStat{}, 10)
	if err != nil {
		t.Fatalf("planSpreadEven: %v", err)
	}
	if plan != nil {
		t.Errorf("expected nil plan for zero total limit, got %d moves", len(plan))
	}
}

// TestPlanSpreadEven_AlreadyBalanced asserts no moves when balanced.
func TestPlanSpreadEven_AlreadyBalanced(t *testing.T) {
	t.Parallel()
	store := newPermissiveStore(t)
	w := newRebalancerOver(t, store, []string{"b1", "b2"})

	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 500, BytesLimit: 1000},
		"b2": {BytesUsed: 500, BytesLimit: 1000},
	}
	plan, err := w.PlanSpreadEven(context.Background(), stats, 10)
	if err != nil {
		t.Fatalf("planSpreadEven: %v", err)
	}
	if len(plan) != 0 {
		t.Errorf("expected 0 moves for balanced backends, got %d", len(plan))
	}
}

// TestPlanSpreadEven_ListObjectsByBackendError surfaces a list failure.
func TestPlanSpreadEven_ListObjectsByBackendError(t *testing.T) {
	t.Parallel()
	store := rebalanceStoreWithList(t, nil, errors.New("db error"), nil)
	w := newRebalancerOver(t, store, []string{"b1", "b2"})

	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 800, BytesLimit: 1000},
		"b2": {BytesUsed: 200, BytesLimit: 1000},
	}
	if _, err := w.PlanSpreadEven(context.Background(), stats, 10); err == nil {
		t.Fatal("expected error from ListObjectsByBackend failure")
	}
}

// TestPlanPackTight_ListObjectsByBackendError surfaces a list failure
// on the pack planner.
func TestPlanPackTight_ListObjectsByBackendError(t *testing.T) {
	t.Parallel()
	store := rebalanceStoreWithList(t, nil, errors.New("db error"), nil)
	w := newRebalancerOver(t, store, []string{"b1", "b2"})

	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 800, BytesLimit: 1000},
		"b2": {BytesUsed: 200, BytesLimit: 1000},
	}
	if _, err := w.PlanPackTight(context.Background(), stats, 10); err == nil {
		t.Fatal("expected error from ListObjectsByBackend failure")
	}
}

// TestExecuteOneMove_AccountsAPICallExactlyOncePerDelete pins issue
// #917: DeleteOrEnqueue owns the DELETE API-call accounting, so a
// successful rebalance move should record source APICalls == 2 (one
// for the GET via Egress, one for the source DELETE via
// DeleteOrEnqueue), not 3.
func TestExecuteOneMove_AccountsAPICallExactlyOncePerDelete(t *testing.T) {
	t.Parallel()
	src := backendtest.NewInMemory()
	_, _ = src.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	dest := backendtest.NewInMemory()

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().MoveObjectLocation(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(int64(4), nil).AnyTimes()
	storetest.Permissive(store)

	rt, coord := newFleet(t, store, map[string]backend.ObjectBackend{"src": src, "dest": dest},
		&fleetOpts{Order: []string{"src", "dest"}})
	w := NewRebalancer(rt, coord, store)

	move := RebalanceMove{
		ObjectKey:   "key",
		FromBackend: "src",
		ToBackend:   "dest",
		SizeBytes:   4,
	}
	if !w.ExecuteOneMove(context.Background(), move, "spread") {
		t.Fatal("ExecuteOneMove returned false on the success path")
	}

	if got := rt.Usage().Backend().Load("src", counter.FieldAPIRequests); got != 2 {
		t.Errorf("src apiRequests = %d, want 2 (Egress + DeleteOrEnqueue, no double-count)", got)
	}
	if got := rt.Usage().Backend().Load("dest", counter.FieldAPIRequests); got != 1 {
		t.Errorf("dest apiRequests = %d, want 1 (Ingress only)", got)
	}
}

// TestExecuteOneMove_DestBackendNotFound rejects an unknown destination.
func TestExecuteOneMove_DestBackendNotFound(t *testing.T) {
	t.Parallel()
	src := backendtest.NewInMemory()
	_, _ = src.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	store := newPermissiveStore(t)
	w := newRebalancerFor(t, store, map[string]backend.ObjectBackend{"src": src}, &fleetOpts{Order: []string{"src"}})

	move := RebalanceMove{
		ObjectKey:   "key",
		FromBackend: "src",
		ToBackend:   "nonexistent",
		SizeBytes:   4,
	}
	if w.ExecuteOneMove(context.Background(), move, "spread") {
		t.Error("expected false when dest backend not found")
	}
}

// TestExecuteOneMove_SourceGetFails returns false on a Get failure.
func TestExecuteOneMove_SourceGetFails(t *testing.T) {
	t.Parallel()
	src := backendtest.NewInMemory()
	src.GetErr = errors.New("read error")
	dest := backendtest.NewInMemory()

	store := newPermissiveStore(t)
	rt, coord := newFleet(t, store, map[string]backend.ObjectBackend{"src": src, "dest": dest},
		&fleetOpts{Order: []string{"src", "dest"}})
	w := NewRebalancer(rt, coord, store)

	move := RebalanceMove{
		ObjectKey:   "key",
		FromBackend: "src",
		ToBackend:   "dest",
		SizeBytes:   4,
	}
	if w.ExecuteOneMove(context.Background(), move, "spread") {
		t.Error("expected false when source get fails")
	}
}

// TestExecuteOneMove_DestPutFails returns false on a Put failure.
func TestExecuteOneMove_DestPutFails(t *testing.T) {
	t.Parallel()
	src := backendtest.NewInMemory()
	_, _ = src.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	dest := backendtest.NewInMemory()
	dest.PutErr = errors.New("write error")

	store := newPermissiveStore(t)
	rt, coord := newFleet(t, store, map[string]backend.ObjectBackend{"src": src, "dest": dest},
		&fleetOpts{Order: []string{"src", "dest"}})
	w := NewRebalancer(rt, coord, store)

	move := RebalanceMove{
		ObjectKey:   "key",
		FromBackend: "src",
		ToBackend:   "dest",
		SizeBytes:   4,
	}
	if w.ExecuteOneMove(context.Background(), move, "spread") {
		t.Error("expected false when dest put fails")
	}
}

// TestExecuteOneMove_MoveLocationError_CleansUpOrphan asserts the
// dest-orphan cleanup runs when MoveObjectLocation returns an error.
func TestExecuteOneMove_MoveLocationError_CleansUpOrphan(t *testing.T) {
	t.Parallel()
	src := backendtest.NewInMemory()
	_, _ = src.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	dest := backendtest.NewInMemory()

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().MoveObjectLocation(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubMoveErr(errors.New("db error"))).AnyTimes()
	storetest.Permissive(store)

	rt, coord := newFleet(t, store, map[string]backend.ObjectBackend{"src": src, "dest": dest},
		&fleetOpts{Order: []string{"src", "dest"}})
	w := NewRebalancer(rt, coord, store)

	move := RebalanceMove{
		ObjectKey:   "key",
		FromBackend: "src",
		ToBackend:   "dest",
		SizeBytes:   4,
	}
	if w.ExecuteOneMove(context.Background(), move, "spread") {
		t.Error("expected false when MoveObjectLocation fails")
	}
	if dest.Has("key") {
		t.Error("orphan should be cleaned up from destination")
	}
}

// TestExecuteOneMove_MoveLocationError_CleanupFails_EnqueuesCleanup
// asserts the cleanup-queue fallback when the inline delete also fails.
func TestExecuteOneMove_MoveLocationError_CleanupFails_EnqueuesCleanup(t *testing.T) {
	t.Parallel()
	src := backendtest.NewInMemory()
	_, _ = src.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	dest := backendtest.NewInMemory()
	dest.DeleteErr = errors.New("delete failed")

	re := &rebalEnqueue{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().MoveObjectLocation(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubMoveErr(errors.New("db error"))).AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubRebalEnqueue(re)).AnyTimes()
	storetest.Permissive(store)

	rt, coord := newFleet(t, store, map[string]backend.ObjectBackend{"src": src, "dest": dest},
		&fleetOpts{Order: []string{"src", "dest"}})
	w := NewRebalancer(rt, coord, store)

	move := RebalanceMove{
		ObjectKey:   "key",
		FromBackend: "src",
		ToBackend:   "dest",
		SizeBytes:   4,
	}
	if w.ExecuteOneMove(context.Background(), move, "spread") {
		t.Error("expected false")
	}
	if len(re.calls) != 1 {
		t.Fatalf("expected 1 enqueue call, got %d", len(re.calls))
	}
	if re.calls[0].Reason != "rebalance_orphan" {
		t.Errorf("expected reason=rebalance_orphan, got %q", re.calls[0].Reason)
	}
}

// TestExecuteOneMove_MovedSizeZero_CleansUpOrphan asserts that a zero-
// row Move triggers dest cleanup.
func TestExecuteOneMove_MovedSizeZero_CleansUpOrphan(t *testing.T) {
	t.Parallel()
	src := backendtest.NewInMemory()
	_, _ = src.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	dest := backendtest.NewInMemory()

	store := rebalanceStoreWithMoveSize(t, 0)
	rt, coord := newFleet(t, store, map[string]backend.ObjectBackend{"src": src, "dest": dest},
		&fleetOpts{Order: []string{"src", "dest"}})
	w := NewRebalancer(rt, coord, store)

	move := RebalanceMove{
		ObjectKey:   "key",
		FromBackend: "src",
		ToBackend:   "dest",
		SizeBytes:   4,
	}
	if w.ExecuteOneMove(context.Background(), move, "spread") {
		t.Error("expected false when movedSize is 0")
	}
	if dest.Has("key") {
		t.Error("orphan should be cleaned up from destination")
	}
}

// TestExecuteOneMove_SourceDeleteFails_EnqueuesCleanup asserts the
// source-delete failure enqueues a cleanup row but still returns true.
func TestExecuteOneMove_SourceDeleteFails_EnqueuesCleanup(t *testing.T) {
	t.Parallel()
	src := backendtest.NewInMemory()
	_, _ = src.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	src.DeleteErr = errors.New("delete failed")
	dest := backendtest.NewInMemory()

	re := &rebalEnqueue{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().MoveObjectLocation(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubMoveSize(4)).AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubRebalEnqueue(re)).AnyTimes()
	storetest.Permissive(store)

	rt, coord := newFleet(t, store, map[string]backend.ObjectBackend{"src": src, "dest": dest},
		&fleetOpts{Order: []string{"src", "dest"}})
	w := NewRebalancer(rt, coord, store)

	move := RebalanceMove{
		ObjectKey:   "key",
		FromBackend: "src",
		ToBackend:   "dest",
		SizeBytes:   4,
	}
	if !w.ExecuteOneMove(context.Background(), move, "spread") {
		t.Error("expected true (move succeeded, source delete failure is non-fatal)")
	}
	if len(re.calls) != 1 {
		t.Fatalf("expected 1 enqueue call, got %d", len(re.calls))
	}
	if re.calls[0].Reason != "rebalance_source_delete" {
		t.Errorf("expected reason=rebalance_source_delete, got %q", re.calls[0].Reason)
	}
}

// TestPlanSpreadEven_RespectsBatchSize asserts the spread planner caps
// at the configured batch size.
func TestPlanSpreadEven_RespectsBatchSize(t *testing.T) {
	t.Parallel()
	objects := make([]core.ObjectLocation, 20)
	for i := range objects {
		objects[i] = core.ObjectLocation{
			ObjectKey:   fmt.Sprintf("obj%d", i),
			BackendName: "b1",
			SizeBytes:   10,
		}
	}
	store := rebalanceStoreWithList(t, objects, nil, nil)
	w := newRebalancerOver(t, store, []string{"b1", "b2"})

	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000},
		"b2": {BytesUsed: 100, BytesLimit: 1000},
	}
	plan, err := w.PlanSpreadEven(context.Background(), stats, 5)
	if err != nil {
		t.Fatalf("planSpreadEven: %v", err)
	}
	if len(plan) > 5 {
		t.Errorf("plan has %d moves, batch limit is 5", len(plan))
	}
}

// TestExecuteMoves_AdmissionBlocked asserts the worker honors a
// saturated admission semaphore + cancelled ctx.
func TestExecuteMoves_AdmissionBlocked(t *testing.T) {
	t.Parallel()
	sem := make(chan struct{}, 1)
	sem <- struct{}{}

	src := backendtest.NewInMemory()
	src.Objects["key1"] = backendtest.Object{Data: []byte("data")}
	dst := backendtest.NewInMemory()

	store := newPermissiveStore(t)
	w := newRebalancerFor(t, store, map[string]backend.ObjectBackend{"b1": src, "b2": dst}, &fleetOpts{Order: []string{"b1", "b2"}})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	moved := w.ExecuteMoves(ctx, []RebalanceMove{
		{ObjectKey: "key1", FromBackend: "b1", ToBackend: "b2", SizeBytes: 4},
	}, "pack", 1, nil).Succeeded
	if moved != 0 {
		t.Errorf("expected 0 moves when admission blocked, got %d", moved)
	}
}
