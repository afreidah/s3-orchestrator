// -------------------------------------------------------------------------------
// Over-Replication Cleaner Tests
//
// Author: Alex Freidah
//
// Tests for background over-replication cleanup: finding over-replicated
// objects, scoring copies by backend health and utilization, removing excess
// copies, and handling errors during cleanup.
// -------------------------------------------------------------------------------

package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// -------------------------------------------------------------------------
// scoreCopy
// -------------------------------------------------------------------------

func TestScoreCopy_HealthyBackend(t *testing.T) {
	store := &mockStore{
		getQuotaStatsResp: map[string]QuotaStat{
			"b1": {BytesUsed: 500, BytesLimit: 1000},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	loc := ObjectLocation{BackendName: "b1", SizeBytes: 100}
	stats := map[string]QuotaStat{
		"b1": {BytesUsed: 500, BytesLimit: 1000},
	}
	score := mgr.OverReplicationCleaner.scoreCopy(&loc,stats)

	// Healthy, 50% utilized -> score = 2 + (1 - 0.5) = 2.5
	if score < 2.4 || score > 2.6 {
		t.Errorf("expected score ~2.5, got %f", score)
	}
}

func TestScoreCopy_UnknownBackend(t *testing.T) {
	store := &mockStore{}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	loc := ObjectLocation{BackendName: "nonexistent", SizeBytes: 100}
	score := mgr.OverReplicationCleaner.scoreCopy(&loc,nil)

	if score != 0 {
		t.Errorf("expected score 0 for unknown backend, got %f", score)
	}
}

func TestScoreCopy_DrainingBackend(t *testing.T) {
	store := &mockStore{}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})
	mgr.draining.Store("b1", &drainState{done: make(chan struct{})})

	loc := ObjectLocation{BackendName: "b1", SizeBytes: 100}
	score := mgr.OverReplicationCleaner.scoreCopy(&loc,nil)

	if score != 0 {
		t.Errorf("expected score 0 for draining backend, got %f", score)
	}
}

func TestScoreCopy_CircuitBrokenBackend(t *testing.T) {
	store := &mockStore{}
	mock := newMockBackend()
	mock.putErr = errors.New("backend down")
	cbBackend := NewCircuitBreakerBackend(mock, "b1", 1, time.Minute)
	// Trip the circuit breaker
	_, _ = cbBackend.PutObject(context.Background(), "k", nil, 0, "", nil)

	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]ObjectBackend{"b1": cbBackend},
		Store:           store,
		Order:           []string{"b1"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: "pack",
	})

	loc := ObjectLocation{BackendName: "b1", SizeBytes: 100}
	score := mgr.OverReplicationCleaner.scoreCopy(&loc,nil)

	if score != 1 {
		t.Errorf("expected score 1 for circuit-broken backend, got %f", score)
	}
}

func TestScoreCopy_NoQuotaData(t *testing.T) {
	store := &mockStore{}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	loc := ObjectLocation{BackendName: "b1", SizeBytes: 100}
	score := mgr.OverReplicationCleaner.scoreCopy(&loc,nil)

	// No quota data -> 2.5 (mid-range)
	if score != 2.5 {
		t.Errorf("expected score 2.5, got %f", score)
	}
}

// -------------------------------------------------------------------------
// Clean (top-level)
// -------------------------------------------------------------------------

func TestClean_FactorDisabled(t *testing.T) {
	store := &mockStore{}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	removed, err := mgr.OverReplicationCleaner.Clean(context.Background(), config.ReplicationConfig{
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

func TestClean_NoOverReplicatedObjects(t *testing.T) {
	store := &mockStore{getOverReplicatedResp: nil}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	removed, err := mgr.OverReplicationCleaner.Clean(context.Background(), config.ReplicationConfig{
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

func TestClean_QueryError(t *testing.T) {
	store := &mockStore{getOverReplicatedErr: errors.New("db down")}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, err := mgr.OverReplicationCleaner.Clean(context.Background(), config.ReplicationConfig{
		Factor:    2,
		BatchSize: 10,
	})
	if err == nil {
		t.Fatal("expected error from Clean")
	}
}

func TestClean_QuotaStatsError_StillCleansUp(t *testing.T) {
	// GetQuotaStats fails, but Clean should still proceed (scores without utilization).
	store := &mockStore{
		getOverReplicatedResp: []ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100},
			{ObjectKey: "key1", BackendName: "b2", SizeBytes: 100},
			{ObjectKey: "key1", BackendName: "b3", SizeBytes: 100},
		},
		getQuotaStatsErr: errors.New("db timeout"),
	}
	b1 := newMockBackend()
	b2 := newMockBackend()
	b3 := newMockBackend()
	mgr := newTestManager(store, map[string]*mockBackend{"b1": b1, "b2": b2, "b3": b3})

	removed, err := mgr.OverReplicationCleaner.Clean(context.Background(), config.ReplicationConfig{
		Factor:      2,
		BatchSize:   10,
		Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	// Should still remove 1 excess copy even without quota data
	if removed != 1 {
		t.Errorf("expected 1 removed, got %d", removed)
	}
}

func TestClean_RemovesExcessCopies(t *testing.T) {
	// Object "key1" has 3 copies but factor=2, so 1 should be removed.
	store := &mockStore{
		getOverReplicatedResp: []ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100},
			{ObjectKey: "key1", BackendName: "b2", SizeBytes: 100},
			{ObjectKey: "key1", BackendName: "b3", SizeBytes: 100},
		},
		getQuotaStatsResp: map[string]QuotaStat{
			"b1": {BytesUsed: 100, BytesLimit: 1000},
			"b2": {BytesUsed: 500, BytesLimit: 1000},
			"b3": {BytesUsed: 900, BytesLimit: 1000},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{
		"b1": newMockBackend(),
		"b2": newMockBackend(),
		"b3": newMockBackend(),
	})

	removed, err := mgr.OverReplicationCleaner.Clean(context.Background(), config.ReplicationConfig{
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

	// The most utilized backend (b3 at 90%) should have its copy removed.
	if len(store.removeExcessCopyCalls) != 1 {
		t.Fatalf("expected 1 RemoveExcessCopy call, got %d", len(store.removeExcessCopyCalls))
	}
	if store.removeExcessCopyCalls[0].backend != "b3" {
		t.Errorf("expected removal from b3 (most utilized), got %s", store.removeExcessCopyCalls[0].backend)
	}
}

func TestClean_RemoveExcessCopyError(t *testing.T) {
	store := &mockStore{
		getOverReplicatedResp: []ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100},
			{ObjectKey: "key1", BackendName: "b2", SizeBytes: 100},
			{ObjectKey: "key1", BackendName: "b3", SizeBytes: 100},
		},
		removeExcessCopyErr: errors.New("db error"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{
		"b1": newMockBackend(),
		"b2": newMockBackend(),
		"b3": newMockBackend(),
	})

	removed, err := mgr.OverReplicationCleaner.Clean(context.Background(), config.ReplicationConfig{
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

func TestClean_MultipleObjects(t *testing.T) {
	// Two objects, each with 3 copies, factor=2 -> remove 1 each = 2 total.
	store := &mockStore{
		getOverReplicatedResp: []ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100},
			{ObjectKey: "key1", BackendName: "b2", SizeBytes: 100},
			{ObjectKey: "key1", BackendName: "b3", SizeBytes: 100},
			{ObjectKey: "key2", BackendName: "b1", SizeBytes: 200},
			{ObjectKey: "key2", BackendName: "b2", SizeBytes: 200},
			{ObjectKey: "key2", BackendName: "b3", SizeBytes: 200},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{
		"b1": newMockBackend(),
		"b2": newMockBackend(),
		"b3": newMockBackend(),
	})

	removed, err := mgr.OverReplicationCleaner.Clean(context.Background(), config.ReplicationConfig{
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

func TestClean_BackendNotFoundDuringCleanup(t *testing.T) {
	// Object has copies on b1 and "gone" — "gone" is not in the manager's
	// backend map, so cleanObject should skip it and not panic.
	store := &mockStore{
		getOverReplicatedResp: []ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100},
			{ObjectKey: "key1", BackendName: "gone", SizeBytes: 100},
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{
		"b1": newMockBackend(),
	})

	removed, err := mgr.OverReplicationCleaner.Clean(context.Background(), config.ReplicationConfig{
		Factor:      2,
		BatchSize:   10,
		Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	// "gone" backend should be skipped (score 0, selected for removal, but
	// getBackend fails), so only the other excess copy (if any) is removed.
	// With 3 copies and factor 2, 1 excess. The "gone" copy scores lowest (0)
	// and is selected first, but removal fails at getBackend. No copies removed.
	if removed != 0 {
		t.Errorf("expected 0 removed (backend not found), got %d", removed)
	}
}

// -------------------------------------------------------------------------
// SetConfig / Config
// -------------------------------------------------------------------------

func TestSetConfig_Config(t *testing.T) {
	store := &mockStore{}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	// Initially nil
	if mgr.OverReplicationCleaner.Config() != nil {
		t.Fatal("expected nil config initially")
	}

	cfg := &config.ReplicationConfig{Factor: 3, BatchSize: 50, Concurrency: 5}
	mgr.OverReplicationCleaner.SetConfig(cfg)

	got := mgr.OverReplicationCleaner.Config()
	if got == nil {
		t.Fatal("expected non-nil config after SetConfig")
	}
	if got.Factor != 3 || got.BatchSize != 50 || got.Concurrency != 5 {
		t.Errorf("unexpected config: %+v", got)
	}
}

// -------------------------------------------------------------------------
// CountPending
// -------------------------------------------------------------------------

func TestCountPending_Success(t *testing.T) {
	store := &mockStore{countOverReplicatedResp: 42}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	count, err := mgr.OverReplicationCleaner.CountPending(context.Background(), 2)
	if err != nil {
		t.Fatalf("CountPending: %v", err)
	}
	if count != 42 {
		t.Errorf("expected 42, got %d", count)
	}
}

func TestCountPending_Error(t *testing.T) {
	store := &mockStore{countOverReplicatedErr: errors.New("db error")}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, err := mgr.OverReplicationCleaner.CountPending(context.Background(), 2)
	if err == nil {
		t.Fatal("expected error from CountPending")
	}
}

func TestClean_AdmissionBlocked(t *testing.T) {
	sem := make(chan struct{}, 1)
	sem <- struct{}{} // fill

	store := &mockStore{
		getOverReplicatedResp: []ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100},
			{ObjectKey: "key1", BackendName: "b2", SizeBytes: 100},
			{ObjectKey: "key1", BackendName: "b3", SizeBytes: 100},
		},
	}
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]ObjectBackend{"b1": newMockBackend(), "b2": newMockBackend(), "b3": newMockBackend()},
		Store:           store,
		Order:           []string{"b1", "b2", "b3"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: "pack",
		AdmissionSem:    sem,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	removed, err := mgr.OverReplicationCleaner.Clean(ctx, config.ReplicationConfig{
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
