// -------------------------------------------------------------------------------
// Rebalancer Tests - Move Execution Concurrency
//
// Author: Alex Freidah
//
// Unit tests for the parallel move execution in the rebalancer. Verifies that
// concurrent moves complete correctly, partial failures are handled, and
// sequential fallback (concurrency=1) works identically to the old behavior.
// -------------------------------------------------------------------------------

package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
	"github.com/afreidah/s3-orchestrator/internal/worker"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
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

// delayedGetBackend wraps mockBackend with a configurable Get delay so
// the concurrency tests can prove parallel execution.
type delayedGetBackend struct {
	mu      sync.Mutex
	objects map[string]mockObject
	putErr  error
	getErr  error
	headErr error
	delErr  error
	delay   time.Duration
}

// newDelayedGetBackend constructs a delayed-Get backend.
func newDelayedGetBackend(delay time.Duration) *delayedGetBackend {
	return &delayedGetBackend{
		objects: make(map[string]mockObject),
		delay:   delay,
	}
}

var _ s3be.ObjectBackend = (*delayedGetBackend)(nil)

// PutObject implements ObjectBackend.
func (m *delayedGetBackend) PutObject(_ context.Context, key string, body io.Reader, _ int64, contentType string, metadata map[string]string) (string, error) {
	m.mu.Lock()
	err := m.putErr
	m.mu.Unlock()
	if err != nil {
		return "", err
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	etag := fmt.Sprintf(`"%x"`, len(data))
	m.mu.Lock()
	m.objects[key] = mockObject{data: data, contentType: contentType, etag: etag, metadata: metadata}
	m.mu.Unlock()
	return etag, nil
}

// GetObject implements ObjectBackend.
func (m *delayedGetBackend) GetObject(_ context.Context, key string, _ string) (*s3be.GetObjectResult, error) {
	time.Sleep(m.delay)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return nil, m.getErr
	}
	obj, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("object %q not found", key)
	}
	cp := make([]byte, len(obj.data))
	copy(cp, obj.data)
	return &s3be.GetObjectResult{
		Body:        io.NopCloser(bytes.NewReader(cp)),
		Size:        int64(len(cp)),
		ContentType: obj.contentType,
		ETag:        obj.etag,
		Metadata:    obj.metadata,
	}, nil
}

// HeadObject implements ObjectBackend.
func (m *delayedGetBackend) HeadObject(_ context.Context, key string) (*s3be.HeadObjectResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.headErr != nil {
		return nil, m.headErr
	}
	obj, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("object %q not found", key)
	}
	return &s3be.HeadObjectResult{
		Size:        int64(len(obj.data)),
		ContentType: obj.contentType,
		ETag:        obj.etag,
		Metadata:    obj.metadata,
	}, nil
}

// DeleteObject implements ObjectBackend.
func (m *delayedGetBackend) DeleteObject(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.delErr != nil {
		return m.delErr
	}
	delete(m.objects, key)
	return nil
}

// seedObject pre-populates the fake's in-memory store.
func (m *delayedGetBackend) seedObject(key string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = mockObject{data: data, contentType: "application/octet-stream"}
}

// hasObject reports whether the key is present.
func (m *delayedGetBackend) hasObject(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objects[key]
	return ok
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
	obs := map[string]s3be.ObjectBackend{"src": src, "dest": dest}
	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        obs,
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"src", "dest"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers

	var plan []worker.RebalanceMove
	for i := range 5 {
		plan = append(plan, worker.RebalanceMove{
			ObjectKey:   fmt.Sprintf("key%d", i),
			FromBackend: "src",
			ToBackend:   "dest",
			SizeBytes:   4,
		})
	}

	start := time.Now()
	moved := workers.Rebalancer.ExecuteMoves(context.Background(), plan, "spread", 3)
	elapsed := time.Since(start)

	if moved != 5 {
		t.Errorf("moved = %d, want 5", moved)
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("elapsed = %v, expected < 200ms with concurrency 3", elapsed)
	}
	for i := range 5 {
		if !dest.hasObject(fmt.Sprintf("key%d", i)) {
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
	obs := map[string]s3be.ObjectBackend{"src": src, "dest": dest}
	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        obs,
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"src", "dest"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers

	plan := []worker.RebalanceMove{
		{ObjectKey: "ok1", FromBackend: "src", ToBackend: "dest", SizeBytes: 4},
		{ObjectKey: "fail", FromBackend: "src", ToBackend: "dest", SizeBytes: 4},
		{ObjectKey: "ok2", FromBackend: "src", ToBackend: "dest", SizeBytes: 4},
	}

	moved := workers.Rebalancer.ExecuteMoves(context.Background(), plan, "spread", 3)
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
	obs := map[string]s3be.ObjectBackend{"src": src, "dest": dest}
	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        obs,
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"src", "dest"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers

	plan := []worker.RebalanceMove{
		{ObjectKey: "a", FromBackend: "src", ToBackend: "dest", SizeBytes: 5},
		{ObjectKey: "b", FromBackend: "src", ToBackend: "dest", SizeBytes: 5},
	}
	moved := workers.Rebalancer.ExecuteMoves(context.Background(), plan, "pack", 1)
	if moved != 2 {
		t.Errorf("moved = %d, want 2", moved)
	}
	if !dest.hasObject("a") || !dest.hasObject("b") {
		t.Error("expected both objects on destination")
	}
}

// TestExceedsThreshold_BelowThreshold pins the no-spread case.
func TestExceedsThreshold_BelowThreshold(t *testing.T) {
	t.Parallel()
	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 500, BytesLimit: 1000},
		"b2": {BytesUsed: 400, BytesLimit: 1000},
	}
	if worker.ExceedsThreshold(stats, []string{"b1", "b2"}, 0.20) {
		t.Error("10% spread should not exceed 20% threshold")
	}
}

// TestExceedsThreshold_AtThreshold pins the threshold-met case.
func TestExceedsThreshold_AtThreshold(t *testing.T) {
	t.Parallel()
	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 800, BytesLimit: 1000},
		"b2": {BytesUsed: 200, BytesLimit: 1000},
	}
	if !worker.ExceedsThreshold(stats, []string{"b1", "b2"}, 0.50) {
		t.Error("60% spread should exceed 50% threshold")
	}
}

// TestExceedsThreshold_SingleBackend pins the single-backend short
// circuit.
func TestExceedsThreshold_SingleBackend(t *testing.T) {
	t.Parallel()
	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000},
	}
	if worker.ExceedsThreshold(stats, []string{"b1"}, 0.10) {
		t.Error("single backend should never exceed threshold")
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
	if worker.ExceedsThreshold(stats, []string{"b1", "b2"}, 0.10) {
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
	if worker.ExceedsThreshold(stats, []string{"b1", "b2"}, 0.10) {
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

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	if _, err := workers.Rebalancer.Rebalance(context.Background(), config.RebalanceConfig{
		Strategy:  "spread",
		BatchSize: 10,
		Threshold: 0.10,
	}); err == nil {
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

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{
		"b1": newMockBackend(),
		"b2": newMockBackend(),
	})

	if _, err := workers.Rebalancer.Rebalance(context.Background(), config.RebalanceConfig{
		Strategy:  "spread",
		BatchSize: 10,
		Threshold: 0.10,
	}); err == nil {
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

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{
		"b1": newMockBackend(),
		"b2": newMockBackend(),
	})

	moved, err := workers.Rebalancer.Rebalance(context.Background(), config.RebalanceConfig{
		Strategy:  "spread",
		BatchSize: 10,
		Threshold: 0.20,
	})
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

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{
		"b1": newMockBackend(),
		"b2": newMockBackend(),
	})

	moved, err := workers.Rebalancer.Rebalance(context.Background(), config.RebalanceConfig{
		Strategy:  "pack",
		BatchSize: 10,
		Threshold: 0.10,
	})
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

// TestRebalance_UnknownStrategy surfaces the unknown-strategy guard.
func TestRebalance_UnknownStrategy(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{
			"b1": {BytesUsed: 900, BytesLimit: 1000},
			"b2": {BytesUsed: 100, BytesLimit: 1000},
		}, nil).AnyTimes()
	storetest.Permissive(store)

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{
		"b1": newMockBackend(),
		"b2": newMockBackend(),
	})
	if _, err := workers.Rebalancer.Rebalance(context.Background(), config.RebalanceConfig{
		Strategy:  "invalid",
		BatchSize: 10,
		Threshold: 0.10,
	}); err == nil {
		t.Fatal("expected error for unknown strategy")
	}
}

// newRebalanceManager wires a manager with the mock store and named
// backends.
func newRebalanceManager(t *testing.T, store core.MetadataStore, names []string) (*BackendManager, *testWorkers) {
	t.Helper()
	backends := make(map[string]s3be.ObjectBackend, len(names))
	for _, name := range names {
		backends[name] = newMockBackend()
	}
	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        backends,
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           names,
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	return mgr, wireWorkersForTest(mgr)
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
	_, workers := newRebalanceManager(t, store, []string{"b1", "b2"})

	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 800, BytesLimit: 1000},
		"b2": {BytesUsed: 200, BytesLimit: 1000},
	}

	plan, err := workers.Rebalancer.PlanPackTight(context.Background(), stats, 10)
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
	_, workers := newRebalanceManager(t, store, []string{"b1", "b2"})

	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 100, BytesLimit: 1000},
		"b2": {BytesUsed: 900, BytesLimit: 1000},
	}
	plan, err := workers.Rebalancer.PlanPackTight(context.Background(), stats, 3)
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
	_, workers := newRebalanceManager(t, store, []string{"b1", "b2"})

	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000},
		"b2": {BytesUsed: 200, BytesLimit: 1000},
	}
	plan, err := workers.Rebalancer.PlanPackTight(context.Background(), stats, 10)
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
	store := newPermissiveMock(t)
	_, workers := newRebalanceManager(t, store, []string{"b1", "b2"})

	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 0, BytesLimit: 0},
		"b2": {BytesUsed: 500, BytesLimit: 1000},
	}
	plan, err := workers.Rebalancer.PlanPackTight(context.Background(), stats, 10)
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
	_, workers := newRebalanceManager(t, store, []string{"b1", "b2"})

	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 800, BytesLimit: 1000},
		"b2": {BytesUsed: 200, BytesLimit: 1000},
	}
	plan, err := workers.Rebalancer.PlanSpreadEven(context.Background(), stats, 10)
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
	_, workers := newRebalanceManager(t, store, []string{"b1", "b2"})

	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 800, BytesLimit: 1000},
		"b2": {BytesUsed: 200, BytesLimit: 1000},
	}
	plan, err := workers.Rebalancer.PlanSpreadEven(context.Background(), stats, 10)
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
	_, workers := newRebalanceManager(t, store, []string{"b1", "b2"})

	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000},
		"b2": {BytesUsed: 100, BytesLimit: 1000},
	}
	plan, err := workers.Rebalancer.PlanPackTight(context.Background(), stats, 10)
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
	store := newPermissiveMock(t)
	_, workers := newRebalanceManager(t, store, []string{"b1"})

	plan, err := workers.Rebalancer.PlanSpreadEven(context.Background(), map[string]core.QuotaStat{}, 10)
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
	store := newPermissiveMock(t)
	_, workers := newRebalanceManager(t, store, []string{"b1", "b2"})

	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 500, BytesLimit: 1000},
		"b2": {BytesUsed: 500, BytesLimit: 1000},
	}
	plan, err := workers.Rebalancer.PlanSpreadEven(context.Background(), stats, 10)
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
	_, workers := newRebalanceManager(t, store, []string{"b1", "b2"})

	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 800, BytesLimit: 1000},
		"b2": {BytesUsed: 200, BytesLimit: 1000},
	}
	if _, err := workers.Rebalancer.PlanSpreadEven(context.Background(), stats, 10); err == nil {
		t.Fatal("expected error from ListObjectsByBackend failure")
	}
}

// TestPlanPackTight_ListObjectsByBackendError surfaces a list failure
// on the pack planner.
func TestPlanPackTight_ListObjectsByBackendError(t *testing.T) {
	t.Parallel()
	store := rebalanceStoreWithList(t, nil, errors.New("db error"), nil)
	_, workers := newRebalanceManager(t, store, []string{"b1", "b2"})

	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 800, BytesLimit: 1000},
		"b2": {BytesUsed: 200, BytesLimit: 1000},
	}
	if _, err := workers.Rebalancer.PlanPackTight(context.Background(), stats, 10); err == nil {
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
	src := newMockBackend()
	_, _ = src.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	dest := newMockBackend()

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().MoveObjectLocation(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(int64(4), nil).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]s3be.ObjectBackend{"src": src, "dest": dest},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"src", "dest"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers

	move := worker.RebalanceMove{
		ObjectKey:   "key",
		FromBackend: "src",
		ToBackend:   "dest",
		SizeBytes:   4,
	}
	if !workers.Rebalancer.ExecuteOneMove(context.Background(), move, "spread") {
		t.Fatal("ExecuteOneMove returned false on the success path")
	}

	if got := mgr.Usage().Backend().Load("src", counter.FieldAPIRequests); got != 2 {
		t.Errorf("src apiRequests = %d, want 2 (Egress + DeleteOrEnqueue, no double-count)", got)
	}
	if got := mgr.Usage().Backend().Load("dest", counter.FieldAPIRequests); got != 1 {
		t.Errorf("dest apiRequests = %d, want 1 (Ingress only)", got)
	}
}

// TestExecuteOneMove_SourceBackendNotFound rejects an unknown source.
func TestExecuteOneMove_SourceBackendNotFound(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	_, workers := newRebalanceManager(t, store, []string{"b1"})

	move := worker.RebalanceMove{
		ObjectKey:   "key",
		FromBackend: "nonexistent",
		ToBackend:   "b1",
		SizeBytes:   100,
	}
	if workers.Rebalancer.ExecuteOneMove(context.Background(), move, "spread") {
		t.Error("expected false when source backend not found")
	}
}

// TestExecuteOneMove_DestBackendNotFound rejects an unknown destination.
func TestExecuteOneMove_DestBackendNotFound(t *testing.T) {
	t.Parallel()
	src := newMockBackend()
	_, _ = src.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	store := newPermissiveMock(t)
	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]s3be.ObjectBackend{"src": src},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"src"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers

	move := worker.RebalanceMove{
		ObjectKey:   "key",
		FromBackend: "src",
		ToBackend:   "nonexistent",
		SizeBytes:   4,
	}
	if workers.Rebalancer.ExecuteOneMove(context.Background(), move, "spread") {
		t.Error("expected false when dest backend not found")
	}
}

// TestExecuteOneMove_SourceGetFails returns false on a Get failure.
func TestExecuteOneMove_SourceGetFails(t *testing.T) {
	t.Parallel()
	src := newMockBackend()
	src.getErr = errors.New("read error")
	dest := newMockBackend()

	store := newPermissiveMock(t)
	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]s3be.ObjectBackend{"src": src, "dest": dest},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"src", "dest"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers

	move := worker.RebalanceMove{
		ObjectKey:   "key",
		FromBackend: "src",
		ToBackend:   "dest",
		SizeBytes:   4,
	}
	if workers.Rebalancer.ExecuteOneMove(context.Background(), move, "spread") {
		t.Error("expected false when source get fails")
	}
}

// TestExecuteOneMove_DestPutFails returns false on a Put failure.
func TestExecuteOneMove_DestPutFails(t *testing.T) {
	t.Parallel()
	src := newMockBackend()
	_, _ = src.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	dest := newMockBackend()
	dest.putErr = errors.New("write error")

	store := newPermissiveMock(t)
	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]s3be.ObjectBackend{"src": src, "dest": dest},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"src", "dest"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers

	move := worker.RebalanceMove{
		ObjectKey:   "key",
		FromBackend: "src",
		ToBackend:   "dest",
		SizeBytes:   4,
	}
	if workers.Rebalancer.ExecuteOneMove(context.Background(), move, "spread") {
		t.Error("expected false when dest put fails")
	}
}

// TestExecuteOneMove_MoveLocationError_CleansUpOrphan asserts the
// dest-orphan cleanup runs when MoveObjectLocation returns an error.
func TestExecuteOneMove_MoveLocationError_CleansUpOrphan(t *testing.T) {
	t.Parallel()
	src := newMockBackend()
	_, _ = src.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	dest := newMockBackend()

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().MoveObjectLocation(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubMoveErr(errors.New("db error"))).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]s3be.ObjectBackend{"src": src, "dest": dest},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"src", "dest"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers

	move := worker.RebalanceMove{
		ObjectKey:   "key",
		FromBackend: "src",
		ToBackend:   "dest",
		SizeBytes:   4,
	}
	if workers.Rebalancer.ExecuteOneMove(context.Background(), move, "spread") {
		t.Error("expected false when MoveObjectLocation fails")
	}
	if dest.hasObject("key") {
		t.Error("orphan should be cleaned up from destination")
	}
}

// TestExecuteOneMove_MoveLocationError_CleanupFails_EnqueuesCleanup
// asserts the cleanup-queue fallback when the inline delete also fails.
func TestExecuteOneMove_MoveLocationError_CleanupFails_EnqueuesCleanup(t *testing.T) {
	t.Parallel()
	src := newMockBackend()
	_, _ = src.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	dest := newMockBackend()
	dest.delErr = errors.New("delete failed")

	re := &rebalEnqueue{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().MoveObjectLocation(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubMoveErr(errors.New("db error"))).AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubRebalEnqueue(re)).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]s3be.ObjectBackend{"src": src, "dest": dest},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"src", "dest"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers

	move := worker.RebalanceMove{
		ObjectKey:   "key",
		FromBackend: "src",
		ToBackend:   "dest",
		SizeBytes:   4,
	}
	if workers.Rebalancer.ExecuteOneMove(context.Background(), move, "spread") {
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
	src := newMockBackend()
	_, _ = src.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	dest := newMockBackend()

	store := rebalanceStoreWithMoveSize(t, 0)
	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]s3be.ObjectBackend{"src": src, "dest": dest},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"src", "dest"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers

	move := worker.RebalanceMove{
		ObjectKey:   "key",
		FromBackend: "src",
		ToBackend:   "dest",
		SizeBytes:   4,
	}
	if workers.Rebalancer.ExecuteOneMove(context.Background(), move, "spread") {
		t.Error("expected false when movedSize is 0")
	}
	if dest.hasObject("key") {
		t.Error("orphan should be cleaned up from destination")
	}
}

// TestExecuteOneMove_SourceDeleteFails_EnqueuesCleanup asserts the
// source-delete failure enqueues a cleanup row but still returns true.
func TestExecuteOneMove_SourceDeleteFails_EnqueuesCleanup(t *testing.T) {
	t.Parallel()
	src := newMockBackend()
	_, _ = src.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	src.delErr = errors.New("delete failed")
	dest := newMockBackend()

	re := &rebalEnqueue{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().MoveObjectLocation(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubMoveSize(4)).AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubRebalEnqueue(re)).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]s3be.ObjectBackend{"src": src, "dest": dest},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"src", "dest"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers

	move := worker.RebalanceMove{
		ObjectKey:   "key",
		FromBackend: "src",
		ToBackend:   "dest",
		SizeBytes:   4,
	}
	if !workers.Rebalancer.ExecuteOneMove(context.Background(), move, "spread") {
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
	_, workers := newRebalanceManager(t, store, []string{"b1", "b2"})

	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000},
		"b2": {BytesUsed: 100, BytesLimit: 1000},
	}
	plan, err := workers.Rebalancer.PlanSpreadEven(context.Background(), stats, 5)
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

	src := newMockBackend()
	src.objects["key1"] = mockObject{data: []byte("data")}
	dst := newMockBackend()

	store := newPermissiveMock(t)
	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]s3be.ObjectBackend{"b1": src, "b2": dst},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1", "b2"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
		AdmissionSem:    sem,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	moved := workers.Rebalancer.ExecuteMoves(ctx, []worker.RebalanceMove{
		{ObjectKey: "key1", FromBackend: "b1", ToBackend: "b2", SizeBytes: 4},
	}, "pack", 1)
	if moved != 0 {
		t.Errorf("expected 0 moves when admission blocked, got %d", moved)
	}
}
