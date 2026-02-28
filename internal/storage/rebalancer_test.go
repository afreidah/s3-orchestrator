// -------------------------------------------------------------------------------
// Rebalancer Tests - Move Execution Concurrency
//
// Author: Alex Freidah
//
// Unit tests for the parallel move execution in the rebalancer. Verifies that
// concurrent moves complete correctly, partial failures are handled, and
// sequential fallback (concurrency=1) works identically to the old behavior.
// -------------------------------------------------------------------------------

package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// delayedGetBackend wraps mockBackend and adds a delay to GetObject
// to simulate real backend latency for concurrency testing.
type delayedGetBackend struct {
	mu      sync.Mutex
	objects map[string]mockObject
	putErr  error
	getErr  error
	headErr error
	delErr  error
	delay   time.Duration
}

func newDelayedGetBackend(delay time.Duration) *delayedGetBackend {
	return &delayedGetBackend{
		objects: make(map[string]mockObject),
		delay:   delay,
	}
}

var _ ObjectBackend = (*delayedGetBackend)(nil)

func (m *delayedGetBackend) PutObject(_ context.Context, key string, body io.Reader, _ int64, contentType string) (string, error) {
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
	m.objects[key] = mockObject{data: data, contentType: contentType, etag: etag}
	m.mu.Unlock()
	return etag, nil
}

func (m *delayedGetBackend) GetObject(_ context.Context, key string, _ string) (*GetObjectResult, error) {
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
	return &GetObjectResult{
		Body:        io.NopCloser(bytes.NewReader(cp)),
		Size:        int64(len(cp)),
		ContentType: obj.contentType,
		ETag:        obj.etag,
	}, nil
}

func (m *delayedGetBackend) HeadObject(_ context.Context, key string) (int64, string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.headErr != nil {
		return 0, "", "", m.headErr
	}
	obj, ok := m.objects[key]
	if !ok {
		return 0, "", "", fmt.Errorf("object %q not found", key)
	}
	return int64(len(obj.data)), obj.contentType, obj.etag, nil
}

func (m *delayedGetBackend) DeleteObject(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.delErr != nil {
		return m.delErr
	}
	delete(m.objects, key)
	return nil
}

func (m *delayedGetBackend) seedObject(key string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = mockObject{data: data, contentType: "application/octet-stream"}
}

func (m *delayedGetBackend) hasObject(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objects[key]
	return ok
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

func TestExecuteMoves_Concurrent(t *testing.T) {
	src := newDelayedGetBackend(50 * time.Millisecond)
	dest := newDelayedGetBackend(0)

	for i := range 5 {
		src.seedObject(fmt.Sprintf("key%d", i), []byte("data"))
	}

	store := &mockStore{moveObjectLocationSize: 4}
	obs := map[string]ObjectBackend{"src": src, "dest": dest}
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        obs,
		Store:           store,
		Order:           []string{"src", "dest"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: "pack",
	})

	var plan []rebalanceMove
	for i := range 5 {
		plan = append(plan, rebalanceMove{
			ObjectKey:   fmt.Sprintf("key%d", i),
			FromBackend: "src",
			ToBackend:   "dest",
			SizeBytes:   4,
		})
	}

	start := time.Now()
	moved := mgr.executeMoves(context.Background(), plan, "spread", 3)
	elapsed := time.Since(start)

	if moved != 5 {
		t.Errorf("moved = %d, want 5", moved)
	}

	// 5 moves at 50ms each with concurrency 3 should take ~100ms (2 batches),
	// not 250ms (sequential). Allow generous margin for CI.
	if elapsed > 200*time.Millisecond {
		t.Errorf("elapsed = %v, expected < 200ms with concurrency 3", elapsed)
	}

	// Verify objects landed on dest
	for i := range 5 {
		if !dest.hasObject(fmt.Sprintf("key%d", i)) {
			t.Errorf("key%d not found on destination", i)
		}
	}
}

func TestExecuteMoves_PartialFailure(t *testing.T) {
	src := newDelayedGetBackend(0)
	dest := newDelayedGetBackend(0)

	src.seedObject("ok1", []byte("data"))
	src.seedObject("ok2", []byte("data"))

	store := &mockStore{moveObjectLocationSize: 4}
	obs := map[string]ObjectBackend{"src": src, "dest": dest}
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        obs,
		Store:           store,
		Order:           []string{"src", "dest"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: "pack",
	})

	// "fail" key does not exist on source, so GetObject returns not-found
	plan := []rebalanceMove{
		{ObjectKey: "ok1", FromBackend: "src", ToBackend: "dest", SizeBytes: 4},
		{ObjectKey: "fail", FromBackend: "src", ToBackend: "dest", SizeBytes: 4},
		{ObjectKey: "ok2", FromBackend: "src", ToBackend: "dest", SizeBytes: 4},
	}

	moved := mgr.executeMoves(context.Background(), plan, "spread", 3)
	if moved != 2 {
		t.Errorf("moved = %d, want 2 (one should fail)", moved)
	}
}

func TestExecuteMoves_SequentialFallback(t *testing.T) {
	src := newDelayedGetBackend(0)
	dest := newDelayedGetBackend(0)

	src.seedObject("a", []byte("hello"))
	src.seedObject("b", []byte("world"))

	store := &mockStore{moveObjectLocationSize: 5}
	obs := map[string]ObjectBackend{"src": src, "dest": dest}
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        obs,
		Store:           store,
		Order:           []string{"src", "dest"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: "pack",
	})

	plan := []rebalanceMove{
		{ObjectKey: "a", FromBackend: "src", ToBackend: "dest", SizeBytes: 5},
		{ObjectKey: "b", FromBackend: "src", ToBackend: "dest", SizeBytes: 5},
	}

	moved := mgr.executeMoves(context.Background(), plan, "pack", 1)
	if moved != 2 {
		t.Errorf("moved = %d, want 2", moved)
	}

	if !dest.hasObject("a") || !dest.hasObject("b") {
		t.Error("expected both objects on destination")
	}
}

// -------------------------------------------------------------------------
// exceedsThreshold
// -------------------------------------------------------------------------

func TestExceedsThreshold_BelowThreshold(t *testing.T) {
	stats := map[string]QuotaStat{
		"b1": {BytesUsed: 500, BytesLimit: 1000},
		"b2": {BytesUsed: 400, BytesLimit: 1000},
	}
	// 50% vs 40% = 10% spread, threshold is 20%
	if exceedsThreshold(stats, []string{"b1", "b2"}, 0.20) {
		t.Error("10% spread should not exceed 20% threshold")
	}
}

func TestExceedsThreshold_AtThreshold(t *testing.T) {
	stats := map[string]QuotaStat{
		"b1": {BytesUsed: 800, BytesLimit: 1000},
		"b2": {BytesUsed: 200, BytesLimit: 1000},
	}
	// 80% vs 20% = 60% spread, threshold is 50%
	if !exceedsThreshold(stats, []string{"b1", "b2"}, 0.50) {
		t.Error("60% spread should exceed 50% threshold")
	}
}

func TestExceedsThreshold_SingleBackend(t *testing.T) {
	stats := map[string]QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000},
	}
	if exceedsThreshold(stats, []string{"b1"}, 0.10) {
		t.Error("single backend should never exceed threshold")
	}
}

func TestExceedsThreshold_ZeroLimitSkipped(t *testing.T) {
	stats := map[string]QuotaStat{
		"b1": {BytesUsed: 800, BytesLimit: 1000},
		"b2": {BytesUsed: 0, BytesLimit: 0}, // unlimited, skipped
	}
	if exceedsThreshold(stats, []string{"b1", "b2"}, 0.10) {
		t.Error("zero-limit backends should be skipped")
	}
}

func TestExceedsThreshold_MissingStatsSkipped(t *testing.T) {
	stats := map[string]QuotaStat{
		"b1": {BytesUsed: 800, BytesLimit: 1000},
	}
	if exceedsThreshold(stats, []string{"b1", "b2"}, 0.10) {
		t.Error("missing stats should be skipped, leaving single backend")
	}
}

// -------------------------------------------------------------------------
// Rebalance (top-level)
// -------------------------------------------------------------------------

func TestRebalance_QuotaStatsError(t *testing.T) {
	store := &mockStore{getQuotaStatsErr: fmt.Errorf("db down")}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, err := mgr.Rebalance(context.Background(), config.RebalanceConfig{
		Strategy:  "spread",
		BatchSize: 10,
		Threshold: 0.10,
	})
	if err == nil {
		t.Fatal("expected error from GetQuotaStats failure")
	}
}

func TestRebalance_BelowThreshold_Skips(t *testing.T) {
	store := &mockStore{
		getQuotaStatsResp: map[string]QuotaStat{
			"b1": {BytesUsed: 500, BytesLimit: 1000},
			"b2": {BytesUsed: 490, BytesLimit: 1000},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{
		"b1": newMockBackend(),
		"b2": newMockBackend(),
	})

	moved, err := mgr.Rebalance(context.Background(), config.RebalanceConfig{
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

func TestRebalance_UnknownStrategy(t *testing.T) {
	store := &mockStore{
		getQuotaStatsResp: map[string]QuotaStat{
			"b1": {BytesUsed: 900, BytesLimit: 1000},
			"b2": {BytesUsed: 100, BytesLimit: 1000},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{
		"b1": newMockBackend(),
		"b2": newMockBackend(),
	})

	_, err := mgr.Rebalance(context.Background(), config.RebalanceConfig{
		Strategy:  "invalid",
		BatchSize: 10,
		Threshold: 0.10,
	})
	if err == nil {
		t.Fatal("expected error for unknown strategy")
	}
}

// -------------------------------------------------------------------------
// planPackTight
// -------------------------------------------------------------------------

func newRebalanceManager(store *mockStore, names []string) *BackendManager {
	backends := make(map[string]ObjectBackend, len(names))
	for _, name := range names {
		backends[name] = newMockBackend()
	}
	return NewBackendManager(&BackendManagerConfig{
		Backends:        backends,
		Store:           store,
		Order:           names,
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: "pack",
	})
}

func TestPlanPackTight_MovesFromLeastToMostFull(t *testing.T) {
	store := &mockStore{
		listObjectsByBackendResp: []ObjectLocation{
			{ObjectKey: "small.txt", BackendName: "b2", SizeBytes: 100},
		},
	}
	mgr := newRebalanceManager(store, []string{"b1", "b2"})

	stats := map[string]QuotaStat{
		"b1": {BytesUsed: 800, BytesLimit: 1000}, // 80% full, 200 free
		"b2": {BytesUsed: 200, BytesLimit: 1000}, // 20% full
	}

	plan, err := mgr.planPackTight(context.Background(), stats, 10)
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

func TestPlanPackTight_RespectsBatchSize(t *testing.T) {
	objects := make([]ObjectLocation, 10)
	for i := range objects {
		objects[i] = ObjectLocation{
			ObjectKey:   fmt.Sprintf("obj%d", i),
			BackendName: "b2",
			SizeBytes:   10,
		}
	}
	store := &mockStore{listObjectsByBackendResp: objects}
	mgr := newRebalanceManager(store, []string{"b1", "b2"})

	stats := map[string]QuotaStat{
		"b1": {BytesUsed: 100, BytesLimit: 1000}, // 10% full
		"b2": {BytesUsed: 900, BytesLimit: 1000}, // 90% full
	}

	plan, err := mgr.planPackTight(context.Background(), stats, 3)
	if err != nil {
		t.Fatalf("planPackTight: %v", err)
	}
	if len(plan) > 3 {
		t.Errorf("plan has %d moves, batch limit is 3", len(plan))
	}
}

func TestPlanPackTight_SkipsLargeObjects(t *testing.T) {
	store := &mockStore{
		listObjectsByBackendResp: []ObjectLocation{
			{ObjectKey: "huge.bin", BackendName: "b2", SizeBytes: 500},
		},
	}
	mgr := newRebalanceManager(store, []string{"b1", "b2"})

	stats := map[string]QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000}, // only 100 bytes free
		"b2": {BytesUsed: 200, BytesLimit: 1000},
	}

	plan, err := mgr.planPackTight(context.Background(), stats, 10)
	if err != nil {
		t.Fatalf("planPackTight: %v", err)
	}
	if len(plan) != 0 {
		t.Errorf("expected 0 moves (object too large), got %d", len(plan))
	}
}

func TestPlanPackTight_ZeroLimitBackendsSkipped(t *testing.T) {
	store := &mockStore{}
	mgr := newRebalanceManager(store, []string{"b1", "b2"})

	stats := map[string]QuotaStat{
		"b1": {BytesUsed: 0, BytesLimit: 0}, // unlimited, skip
		"b2": {BytesUsed: 500, BytesLimit: 1000},
	}

	plan, err := mgr.planPackTight(context.Background(), stats, 10)
	if err != nil {
		t.Fatalf("planPackTight: %v", err)
	}
	if len(plan) != 0 {
		t.Errorf("expected 0 moves with single quotad backend, got %d", len(plan))
	}
}

// -------------------------------------------------------------------------
// planSpreadEven
// -------------------------------------------------------------------------

func TestPlanSpreadEven_EqualizesUtilization(t *testing.T) {
	store := &mockStore{
		listObjectsByBackendResp: []ObjectLocation{
			{ObjectKey: "obj1", BackendName: "b1", SizeBytes: 100},
			{ObjectKey: "obj2", BackendName: "b1", SizeBytes: 100},
		},
	}
	mgr := newRebalanceManager(store, []string{"b1", "b2"})

	stats := map[string]QuotaStat{
		"b1": {BytesUsed: 800, BytesLimit: 1000}, // 80%
		"b2": {BytesUsed: 200, BytesLimit: 1000}, // 20%
	}
	// Target ratio = 1000/2000 = 50%
	// b1 excess = 800 - 500 = 300
	// b2 deficit = 200 - 500 = -300

	plan, err := mgr.planSpreadEven(context.Background(), stats, 10)
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

func TestPlanSpreadEven_ZeroTotalLimit(t *testing.T) {
	store := &mockStore{}
	mgr := newRebalanceManager(store, []string{"b1"})

	stats := map[string]QuotaStat{} // no stats at all

	plan, err := mgr.planSpreadEven(context.Background(), stats, 10)
	if err != nil {
		t.Fatalf("planSpreadEven: %v", err)
	}
	if plan != nil {
		t.Errorf("expected nil plan for zero total limit, got %d moves", len(plan))
	}
}

func TestPlanSpreadEven_AlreadyBalanced(t *testing.T) {
	store := &mockStore{}
	mgr := newRebalanceManager(store, []string{"b1", "b2"})

	stats := map[string]QuotaStat{
		"b1": {BytesUsed: 500, BytesLimit: 1000},
		"b2": {BytesUsed: 500, BytesLimit: 1000},
	}

	plan, err := mgr.planSpreadEven(context.Background(), stats, 10)
	if err != nil {
		t.Fatalf("planSpreadEven: %v", err)
	}
	if len(plan) != 0 {
		t.Errorf("expected 0 moves for balanced backends, got %d", len(plan))
	}
}

func TestPlanSpreadEven_RespectsBatchSize(t *testing.T) {
	objects := make([]ObjectLocation, 20)
	for i := range objects {
		objects[i] = ObjectLocation{
			ObjectKey:   fmt.Sprintf("obj%d", i),
			BackendName: "b1",
			SizeBytes:   10,
		}
	}
	store := &mockStore{listObjectsByBackendResp: objects}
	mgr := newRebalanceManager(store, []string{"b1", "b2"})

	stats := map[string]QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000},
		"b2": {BytesUsed: 100, BytesLimit: 1000},
	}

	plan, err := mgr.planSpreadEven(context.Background(), stats, 5)
	if err != nil {
		t.Fatalf("planSpreadEven: %v", err)
	}
	if len(plan) > 5 {
		t.Errorf("plan has %d moves, batch limit is 5", len(plan))
	}
}
