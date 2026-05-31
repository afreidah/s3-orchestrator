// -------------------------------------------------------------------------------
// Over-Replication Cleaner Tests
//
// Author: Alex Freidah
//
// Tests for background over-replication cleanup: finding over-replicated
// objects, scoring copies by backend health and utilization, removing excess
// copies, and handling errors during cleanup.
// -------------------------------------------------------------------------------

package proxy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// removeExcessRecord captures one RemoveExcessCopy call.
type removeExcessRecord struct {
	key, backend string
	size         int64
}

// removeExcessTracker accumulates RemoveExcessCopy calls and returns the
// configured error.
type removeExcessTracker struct {
	mu    sync.Mutex
	calls []removeExcessRecord
	err   error
}

// stubRemoveExcessCopy returns a DoAndReturn that captures into rt.
func stubRemoveExcessCopy(rt *removeExcessTracker) func(context.Context, string, string, int64) error {
	return func(_ context.Context, key, backend string, size int64) error {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		rt.calls = append(rt.calls, removeExcessRecord{key: key, backend: backend, size: size})
		return rt.err
	}
}

// TestScoreCopy_HealthyBackend pins the healthy-backend scoring formula.
func TestScoreCopy_HealthyBackend(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	loc := core.ObjectLocation{BackendName: "b1", SizeBytes: 100}
	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 500, BytesLimit: 1000},
	}
	score := workers.OverReplicationCleaner.ScoreCopy(&loc, stats)

	if score < 2.4 || score > 2.6 {
		t.Errorf("expected score ~2.5, got %f", score)
	}
}

// TestScoreCopy_UnknownBackend asserts unknown backends score 0.
func TestScoreCopy_UnknownBackend(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	loc := core.ObjectLocation{BackendName: "nonexistent", SizeBytes: 100}
	score := workers.OverReplicationCleaner.ScoreCopy(&loc, nil)

	if score != 0 {
		t.Errorf("expected score 0 for unknown backend, got %f", score)
	}
}

// TestScoreCopy_DrainingBackend asserts a draining backend scores 0.
func TestScoreCopy_DrainingBackend(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	mgr, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	mgr.drainManager.SeedActiveForTest("b1")

	loc := core.ObjectLocation{BackendName: "b1", SizeBytes: 100}
	score := workers.OverReplicationCleaner.ScoreCopy(&loc, nil)

	if score != 0 {
		t.Errorf("expected score 0 for draining backend, got %f", score)
	}
}

// TestScoreCopy_CircuitBrokenBackend pins the score for a tripped breaker.
func TestScoreCopy_CircuitBrokenBackend(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	mock := newMockBackend()
	mock.putErr = errors.New("backend down")
	cbBackend := backend.NewCircuitBreakerBackend(mock, "b1", 1, time.Minute)
	_, _ = cbBackend.PutObject(context.Background(), "k", nil, 0, "", nil)

	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": cbBackend},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr, store)
	_ = workers

	loc := core.ObjectLocation{BackendName: "b1", SizeBytes: 100}
	score := workers.OverReplicationCleaner.ScoreCopy(&loc, nil)

	if score != 1 {
		t.Errorf("expected score 1 for circuit-broken backend, got %f", score)
	}
}

// TestScoreCopy_NoQuotaData asserts the no-data fallback score.
func TestScoreCopy_NoQuotaData(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	loc := core.ObjectLocation{BackendName: "b1", SizeBytes: 100}
	score := workers.OverReplicationCleaner.ScoreCopy(&loc, nil)

	if score != 2.5 {
		t.Errorf("expected score 2.5, got %f", score)
	}
}

// TestClean_FactorDisabled asserts factor=1 short-circuits.
func TestClean_FactorDisabled(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	removed, err := workers.OverReplicationCleaner.Clean(context.Background(), config.ReplicationConfig{
		Factor:    1,
		BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if removed != 0 {
		t.Errorf("expected 0 removed, got %d", removed)
	}
}

// TestClean_NoOverReplicatedObjects asserts an empty over-replicated
// query produces zero work.
func TestClean_NoOverReplicatedObjects(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	removed, err := workers.OverReplicationCleaner.Clean(context.Background(), config.ReplicationConfig{
		Factor:    2,
		BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if removed != 0 {
		t.Errorf("expected 0 removed, got %d", removed)
	}
}

// TestClean_QueryError surfaces the over-replicated query failure.
func TestClean_QueryError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetOverReplicatedObjects(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, errors.New("db down")).AnyTimes()
	storetest.Permissive(store)

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	if _, err := workers.OverReplicationCleaner.Clean(context.Background(), config.ReplicationConfig{
		Factor:    2,
		BatchSize: 10,
	}); err == nil {
		t.Fatal("expected error from Clean")
	}
}

// TestClean_QuotaStatsError_StillCleansUp asserts a missing quota-stats
// payload doesn't abort the clean.
func TestClean_QuotaStatsError_StillCleansUp(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetOverReplicatedObjects(gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100},
			{ObjectKey: "key1", BackendName: "b2", SizeBytes: 100},
			{ObjectKey: "key1", BackendName: "b3", SizeBytes: 100},
		}, nil).AnyTimes()
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(nil, errors.New("db timeout")).AnyTimes()
	storetest.Permissive(store)

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{
		"b1": newMockBackend(),
		"b2": newMockBackend(),
		"b3": newMockBackend(),
	})

	removed, err := workers.OverReplicationCleaner.Clean(context.Background(), config.ReplicationConfig{
		Factor:      2,
		BatchSize:   10,
		Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if removed != 1 {
		t.Errorf("expected 1 removed, got %d", removed)
	}
}

// TestClean_RemovesExcessCopies pins the most-utilized-loses behaviour.
func TestClean_RemovesExcessCopies(t *testing.T) {
	t.Parallel()
	rt := &removeExcessTracker{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetOverReplicatedObjects(gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100},
			{ObjectKey: "key1", BackendName: "b2", SizeBytes: 100},
			{ObjectKey: "key1", BackendName: "b3", SizeBytes: 100},
		}, nil).AnyTimes()
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{
			"b1": {BytesUsed: 100, BytesLimit: 1000},
			"b2": {BytesUsed: 500, BytesLimit: 1000},
			"b3": {BytesUsed: 900, BytesLimit: 1000},
		}, nil).AnyTimes()
	store.EXPECT().RemoveExcessCopy(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubRemoveExcessCopy(rt)).AnyTimes()
	storetest.Permissive(store)

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{
		"b1": newMockBackend(),
		"b2": newMockBackend(),
		"b3": newMockBackend(),
	})

	removed, err := workers.OverReplicationCleaner.Clean(context.Background(), config.ReplicationConfig{
		Factor:      2,
		BatchSize:   10,
		Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if removed != 1 {
		t.Errorf("expected 1 removed, got %d", removed)
	}
	if len(rt.calls) != 1 {
		t.Fatalf("expected 1 RemoveExcessCopy call, got %d", len(rt.calls))
	}
	if rt.calls[0].backend != "b3" {
		t.Errorf("expected removal from b3 (most utilized), got %s", rt.calls[0].backend)
	}
}

// TestClean_RemoveExcessCopyError swallows the per-object failure.
func TestClean_RemoveExcessCopyError(t *testing.T) {
	t.Parallel()
	rt := &removeExcessTracker{err: errors.New("db error")}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetOverReplicatedObjects(gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100},
			{ObjectKey: "key1", BackendName: "b2", SizeBytes: 100},
			{ObjectKey: "key1", BackendName: "b3", SizeBytes: 100},
		}, nil).AnyTimes()
	store.EXPECT().RemoveExcessCopy(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubRemoveExcessCopy(rt)).AnyTimes()
	storetest.Permissive(store)

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{
		"b1": newMockBackend(),
		"b2": newMockBackend(),
		"b3": newMockBackend(),
	})

	removed, err := workers.OverReplicationCleaner.Clean(context.Background(), config.ReplicationConfig{
		Factor:      2,
		BatchSize:   10,
		Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Clean should not return error for per-object failures: %v", err)
	}
	if removed != 0 {
		t.Errorf("expected 0 removed (all failed), got %d", removed)
	}
}

// TestClean_MultipleObjects asserts the per-object removal counts sum.
func TestClean_MultipleObjects(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetOverReplicatedObjects(gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100},
			{ObjectKey: "key1", BackendName: "b2", SizeBytes: 100},
			{ObjectKey: "key1", BackendName: "b3", SizeBytes: 100},
			{ObjectKey: "key2", BackendName: "b1", SizeBytes: 200},
			{ObjectKey: "key2", BackendName: "b2", SizeBytes: 200},
			{ObjectKey: "key2", BackendName: "b3", SizeBytes: 200},
		}, nil).AnyTimes()
	storetest.Permissive(store)

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{
		"b1": newMockBackend(),
		"b2": newMockBackend(),
		"b3": newMockBackend(),
	})

	removed, err := workers.OverReplicationCleaner.Clean(context.Background(), config.ReplicationConfig{
		Factor:      2,
		BatchSize:   10,
		Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if removed != 2 {
		t.Errorf("expected 2 removed, got %d", removed)
	}
}

// TestClean_BackendNotFoundDuringCleanup asserts a missing backend is
// skipped without panic.
func TestClean_BackendNotFoundDuringCleanup(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetOverReplicatedObjects(gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100},
			{ObjectKey: "key1", BackendName: "gone", SizeBytes: 100},
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100},
		}, nil).AnyTimes()
	storetest.Permissive(store)

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{
		"b1": newMockBackend(),
	})

	removed, err := workers.OverReplicationCleaner.Clean(context.Background(), config.ReplicationConfig{
		Factor:      2,
		BatchSize:   10,
		Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if removed != 0 {
		t.Errorf("expected 0 removed (backend not found), got %d", removed)
	}
}

// TestSetConfig_Config pins the SetConfig/Config round-trip.
func TestSetConfig_Config(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	if workers.OverReplicationCleaner.Config() != nil {
		t.Fatal("expected nil config initially")
	}

	cfg := &config.ReplicationConfig{Factor: 3, BatchSize: 50, Concurrency: 5}
	workers.OverReplicationCleaner.SetConfig(cfg)

	got := workers.OverReplicationCleaner.Config()
	if got == nil {
		t.Fatal("expected non-nil config after SetConfig")
	}
	if got.Factor != 3 || got.BatchSize != 50 || got.Concurrency != 5 {
		t.Errorf("unexpected config: %+v", got)
	}
}

// TestCountPending_Success returns the configured count.
func TestCountPending_Success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().CountOverReplicatedObjects(gomock.Any(), gomock.Any()).
		Return(int64(42), nil).AnyTimes()
	storetest.Permissive(store)

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	count, err := workers.OverReplicationCleaner.CountPending(context.Background(), 2)
	if err != nil {
		t.Fatalf("CountPending: %v", err)
	}
	if count != 42 {
		t.Errorf("expected 42, got %d", count)
	}
}

// TestCountPending_Error surfaces the count-query failure.
func TestCountPending_Error(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().CountOverReplicatedObjects(gomock.Any(), gomock.Any()).
		Return(int64(0), errors.New("db error")).AnyTimes()
	storetest.Permissive(store)

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	if _, err := workers.OverReplicationCleaner.CountPending(context.Background(), 2); err == nil {
		t.Fatal("expected error from CountPending")
	}
}

// TestClean_AdmissionBlocked asserts a saturated admission semaphore +
// cancelled ctx halts the worker.
func TestClean_AdmissionBlocked(t *testing.T) {
	t.Parallel()
	sem := make(chan struct{}, 1)
	sem <- struct{}{}

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetOverReplicatedObjects(gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100},
			{ObjectKey: "key1", BackendName: "b2", SizeBytes: 100},
			{ObjectKey: "key1", BackendName: "b3", SizeBytes: 100},
		}, nil).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": newMockBackend(), "b2": newMockBackend(), "b3": newMockBackend()},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1", "b2", "b3"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
		AdmissionSem:    sem,
	})
	workers := wireWorkersForTest(mgr, store)
	_ = workers

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	removed, err := workers.OverReplicationCleaner.Clean(ctx, config.ReplicationConfig{
		Factor:      2,
		BatchSize:   10,
		Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if removed != 0 {
		t.Errorf("expected 0 removed when admission blocked, got %d", removed)
	}
}
