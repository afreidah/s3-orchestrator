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
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// rebalRetries captures RetryCleanupItem calls for rebalancer tests.
type rebalRetries struct {
	mu  sync.Mutex
	ids []int64
}

// rebalCleanupID is the id of the cleanup row a stubbed placement queues.
const rebalCleanupID = 7

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// stubRebalRetry returns a DoAndReturn that captures into rr.
func stubRebalRetry(rr *rebalRetries) func(context.Context, int64, time.Duration, string) error {
	return func(_ context.Context, id int64, _ time.Duration, _ string) error {
		rr.mu.Lock()
		defer rr.mu.Unlock()
		rr.ids = append(rr.ids, id)
		return nil
	}
}

// refusePlacement returns a PlaceCopy stub that refuses with err and queues
// the copy the move wrote under the placement's orphan reason, as the store
// does. Every placement's reasons are recorded on reasons.
func refusePlacement(err error, reasons *[]core.MoveReasons) func(context.Context, *core.Placement) (int64, []core.CleanupItem, error) {
	return func(_ context.Context, p *core.Placement) (int64, []core.CleanupItem, error) {
		*reasons = append(*reasons, p.Reasons)
		return 0, []core.CleanupItem{{
			ID: rebalCleanupID, BackendName: p.Intent.BackendName, ObjectKey: p.Intent.ObjectKey,
			StorageKey: p.Intent.StorageKey, Reason: p.Reasons.Orphan, SizeBytes: p.Intent.SizeBytes,
		}}, err
	}
}

// placeMovingSource is a PlaceCopy stub that records the move and queues the
// source copy it removed under the placement's source-delete reason.
func placeMovingSource(_ context.Context, p *core.Placement) (int64, []core.CleanupItem, error) {
	return p.Source.SizeBytes, []core.CleanupItem{{
		ID: rebalCleanupID, BackendName: p.Source.BackendName, ObjectKey: p.Source.ObjectKey,
		StorageKey: core.StoragePath(p.Source.ObjectKey, p.Source.StorageKey),
		Reason:     p.Reasons.SourceDelete, SizeBytes: p.Source.SizeBytes,
	}}, nil
}

// planMove builds a rebalance move of key, read as a copy of size bytes on
// from stored at the key itself, toward to.
func planMove(key, from, to string, size int64) RebalanceMove {
	return RebalanceMove{
		Source:    &core.ObjectLocation{ObjectKey: key, BackendName: from, StorageKey: key, SizeBytes: size},
		ToBackend: to,
	}
}

// rebalanceStoreWithMoveSize returns a store whose PlaceCopy succeeds with
// the given size.
func rebalanceStoreWithMoveSize(t *testing.T, size int64) *storetest.MockMetadataStore {
	t.Helper()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().PlaceCopy(gomock.Any(), gomock.Any()).Return(size, nil, nil).AnyTimes()
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

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

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
		plan = append(plan, planMove(fmt.Sprintf("key%d", i), "src", "dest", 4))
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
		if !dest.HasCopyOf(fmt.Sprintf("key%d", i)) {
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
		planMove("ok1", "src", "dest", 4),
		planMove("fail", "src", "dest", 4),
		planMove("ok2", "src", "dest", 4),
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
		planMove("a", "src", "dest", 5),
		planMove("b", "src", "dest", 5),
	}
	moved := w.ExecuteMoves(context.Background(), plan, "pack", 1, nil).Succeeded
	if moved != 2 {
		t.Errorf("moved = %d, want 2", moved)
	}
	if !dest.HasCopyOf("a") || !dest.HasCopyOf("b") {
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
	store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
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
	store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
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
		store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(nil, listErr).AnyTimes()
	} else {
		store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
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
	if plan[0].Source.BackendName != "b2" || plan[0].ToBackend != "b1" {
		t.Errorf("expected move from b2 to b1, got %s -> %s", plan[0].Source.BackendName, plan[0].ToBackend)
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
		if mv.Source.BackendName != "b1" || mv.ToBackend != "b2" {
			t.Errorf("expected move from b1 to b2, got %s -> %s", mv.Source.BackendName, mv.ToBackend)
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

// TestExecuteOneMove_AccountsAPICallExactlyOncePerDelete verifies the settle
// owns the DELETE API-call accounting, so a successful rebalance move records
// source APICalls == 2 (one for the GET via Egress, one for the settled source
// DELETE), not 3.
func TestExecuteOneMove_AccountsAPICallExactlyOncePerDelete(t *testing.T) {
	t.Parallel()
	src := backendtest.NewInMemory()
	_, _ = src.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	dest := backendtest.NewInMemory()

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().PlaceCopy(gomock.Any(), gomock.Any()).DoAndReturn(placeMovingSource).AnyTimes()
	storetest.Permissive(store)

	rt, coord := newFleet(t, store, map[string]backend.ObjectBackend{"src": src, "dest": dest},
		&fleetOpts{Order: []string{"src", "dest"}})
	w := NewRebalancer(rt, coord, store)

	move := planMove("key", "src", "dest", 4)
	if !w.ExecuteOneMove(context.Background(), move, "spread") {
		t.Fatal("ExecuteOneMove returned false on the success path")
	}

	if got := rt.Usage().Backend().Load("src", counter.FieldAPIRequests); got != 2 {
		t.Errorf("src apiRequests = %d, want 2 (Egress + settled delete, no double-count)", got)
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

	move := planMove("key", "src", "nonexistent", 4)
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

	move := planMove("key", "src", "dest", 4)
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

	move := planMove("key", "src", "dest", 4)
	if w.ExecuteOneMove(context.Background(), move, "spread") {
		t.Error("expected false when dest put fails")
	}
}

// TestExecuteOneMove_PlaceCopyError_LeavesCopyForReaper asserts a placement
// that fails outright queues nothing, so the destination copy stays under its
// intent for the pending reaper to resolve.
func TestExecuteOneMove_PlaceCopyError_LeavesCopyForReaper(t *testing.T) {
	t.Parallel()
	src := backendtest.NewInMemory()
	_, _ = src.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	dest := backendtest.NewInMemory()

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().PlaceCopy(gomock.Any(), gomock.Any()).
		Return(int64(0), nil, errors.New("db error")).AnyTimes()
	storetest.Permissive(store)

	rt, coord := newFleet(t, store, map[string]backend.ObjectBackend{"src": src, "dest": dest},
		&fleetOpts{Order: []string{"src", "dest"}})
	w := NewRebalancer(rt, coord, store)

	move := planMove("key", "src", "dest", 4)
	if w.ExecuteOneMove(context.Background(), move, "spread") {
		t.Error("expected false when PlaceCopy fails")
	}
	if !dest.HasCopyOf("key") {
		t.Error("the unplaced copy was deleted; its intent leaves it to the reaper")
	}
	if !src.HasCopyOf("key") {
		t.Error("source bytes must stay when the placement fails")
	}
}

// TestExecuteOneMove_RefusalSettleFails_RetriesTheRow asserts a refused
// placement whose destination delete fails leaves the queued row retried
// for the cleanup worker, under the rebalancer's reasons.
func TestExecuteOneMove_RefusalSettleFails_RetriesTheRow(t *testing.T) {
	t.Parallel()
	src := backendtest.NewInMemory()
	_, _ = src.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	dest := backendtest.NewInMemory()
	dest.DeleteErr = errors.New("delete failed")

	var reasons []core.MoveReasons
	rr := &rebalRetries{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().PlaceCopy(gomock.Any(), gomock.Any()).DoAndReturn(refusePlacement(core.ErrTargetRefused, &reasons))
	store.EXPECT().RetryCleanupItem(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubRebalRetry(rr)).AnyTimes()
	storetest.Permissive(store)

	rt, coord := newFleet(t, store, map[string]backend.ObjectBackend{"src": src, "dest": dest},
		&fleetOpts{Order: []string{"src", "dest"}})
	w := NewRebalancer(rt, coord, store)

	if w.ExecuteOneMove(context.Background(), planMove("key", "src", "dest", 4), "spread") {
		t.Error("expected false")
	}
	if len(reasons) != 1 || reasons[0] != writepath.RebalanceMove.Reasons {
		t.Errorf("placement reasons = %+v, want the rebalancer's", reasons)
	}
	if len(rr.ids) != 1 || rr.ids[0] != rebalCleanupID {
		t.Errorf("retried rows %v, want the refused copy's row", rr.ids)
	}
}

// TestExecuteOneMove_PlacementDeclined_CleansUpOrphan asserts a move whose
// placement is declined reports no move and settles the cleanup the refusal
// queued, removing the destination bytes.
func TestExecuteOneMove_PlacementDeclined_CleansUpOrphan(t *testing.T) {
	t.Parallel()
	for _, declined := range []error{core.ErrCopyChanged, core.ErrTargetRefused} {
		t.Run(declined.Error(), func(t *testing.T) {
			t.Parallel()
			src := backendtest.NewInMemory()
			_, _ = src.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
			dest := backendtest.NewInMemory()

			var reasons []core.MoveReasons
			ctrl := gomock.NewController(t)
			store := storetest.NewMockMetadataStore(ctrl)
			store.EXPECT().PlaceCopy(gomock.Any(), gomock.Any()).DoAndReturn(refusePlacement(declined, &reasons))
			storetest.Permissive(store)
			rt, coord := newFleet(t, store, map[string]backend.ObjectBackend{"src": src, "dest": dest},
				&fleetOpts{Order: []string{"src", "dest"}})
			w := NewRebalancer(rt, coord, store)

			if w.ExecuteOneMove(context.Background(), planMove("key", "src", "dest", 4), "spread") {
				t.Error("expected false when the placement is declined")
			}
			if len(dest.Objects) != 0 {
				t.Errorf("destination holds %d objects, want the orphan cleaned up", len(dest.Objects))
			}
			if !src.HasCopyOf("key") {
				t.Error("source bytes must stay when the placement is declined")
			}
		})
	}
}

// TestExecuteOneMove_SourceDeleteFails_RetriesTheRow asserts a move whose
// source delete fails still succeeds, leaving the source's queued row retried
// for the cleanup worker.
func TestExecuteOneMove_SourceDeleteFails_RetriesTheRow(t *testing.T) {
	t.Parallel()
	src := backendtest.NewInMemory()
	_, _ = src.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	src.DeleteErr = errors.New("delete failed")
	dest := backendtest.NewInMemory()

	rr := &rebalRetries{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().PlaceCopy(gomock.Any(), gomock.Any()).DoAndReturn(placeMovingSource)
	store.EXPECT().RetryCleanupItem(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubRebalRetry(rr)).AnyTimes()
	storetest.Permissive(store)

	rt, coord := newFleet(t, store, map[string]backend.ObjectBackend{"src": src, "dest": dest},
		&fleetOpts{Order: []string{"src", "dest"}})
	w := NewRebalancer(rt, coord, store)

	move := planMove("key", "src", "dest", 4)
	if !w.ExecuteOneMove(context.Background(), move, "spread") {
		t.Error("expected true (move succeeded, source delete failure is non-fatal)")
	}
	if len(rr.ids) != 1 || rr.ids[0] != rebalCleanupID {
		t.Errorf("retried rows %v, want the source copy's row", rr.ids)
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
		planMove("key1", "b1", "b2", 4),
	}, "pack", 1, nil).Succeeded
	if moved != 0 {
		t.Errorf("expected 0 moves when admission blocked, got %d", moved)
	}
}
