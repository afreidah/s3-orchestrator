// -------------------------------------------------------------------------------
// Replicator Tests - Replica Creation and Failover
//
// Author: Alex Freidah
//
// Tests for background replication: finding under-replicated objects, copying
// data between backends with failover, conditional replica recording, and
// orphan cleanup on failure.
// -------------------------------------------------------------------------------

package proxy

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	st "github.com/afreidah/s3-orchestrator/internal/store"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// -------------------------------------------------------------------------
// st.GroupByKey
// -------------------------------------------------------------------------

func TestGroupByKey_Groups(t *testing.T) {
	t.Parallel()
	locations := []st.ObjectLocation{
		{ObjectKey: "a", BackendName: "b1"},
		{ObjectKey: "a", BackendName: "b2"},
		{ObjectKey: "b", BackendName: "b1"},
	}
	grouped := st.GroupByKey(locations)
	if len(grouped) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(grouped))
	}
	if len(grouped["a"]) != 2 {
		t.Errorf("expected 2 copies of 'a', got %d", len(grouped["a"]))
	}
	if len(grouped["b"]) != 1 {
		t.Errorf("expected 1 copy of 'b', got %d", len(grouped["b"]))
	}
}

func TestGroupByKey_Empty(t *testing.T) {
	t.Parallel()
	grouped := st.GroupByKey(nil)
	if len(grouped) != 0 {
		t.Errorf("expected 0 groups, got %d", len(grouped))
	}
}

// -------------------------------------------------------------------------
// Replicate (top-level)
// -------------------------------------------------------------------------

func TestReplicate_NoUnderReplicatedObjects(t *testing.T) {
	t.Parallel()
	store := &mockStore{getUnderReplicatedResp: nil}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	created, err := mgr.Replicator.Replicate(context.Background(), config.ReplicationConfig{
		Factor:    2,
		BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if created != 0 {
		t.Errorf("expected 0 created, got %d", created)
	}
}

func TestReplicate_QueryError(t *testing.T) {
	t.Parallel()
	store := &mockStore{getUnderReplicatedErr: errors.New("db down")}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, err := mgr.Replicator.Replicate(context.Background(), config.ReplicationConfig{
		Factor:    2,
		BatchSize: 10,
	})
	if err == nil {
		t.Fatal("expected error from GetUnderReplicatedObjects failure")
	}
}

func TestReplicate_QuotaStatsError(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	store := &mockStore{
		getUnderReplicatedResp: []st.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
		},
		getQuotaStatsErr: errors.New("db down"),
	}
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": b1, "b2": newMockBackend()},
		Store:           store,
		Order:           []string{"b1", "b2"},
		CacheTTL:        5 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})

	_, err := mgr.Replicator.Replicate(context.Background(), config.ReplicationConfig{
		Factor:    2,
		BatchSize: 10,
	})
	if err == nil {
		t.Fatal("expected error from GetQuotaStats failure")
	}
}

func TestReplicate_Success(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b2 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	store := &mockStore{
		getUnderReplicatedResp: []st.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
		},
		getQuotaStatsResp: map[string]st.QuotaStat{
			"b1": {BytesUsed: 100, BytesLimit: 1000},
			"b2": {BytesUsed: 100, BytesLimit: 1000},
		},
		recordReplicaInserted: true,
	}
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": b1, "b2": b2},
		Store:           store,
		Order:           []string{"b1", "b2"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})

	created, err := mgr.Replicator.Replicate(context.Background(), config.ReplicationConfig{
		Factor:    2,
		BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if created != 1 {
		t.Errorf("expected 1 created, got %d", created)
	}
	// Data should have been copied to b2
	if !b2.hasObject("key1") {
		t.Error("expected key1 on b2 after replication")
	}
}

// -------------------------------------------------------------------------
// findReplicaTarget
// -------------------------------------------------------------------------

func TestFindReplicaTarget_ExcludesExistingCopies(t *testing.T) {
	t.Parallel()
	store := &mockStore{}
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": newMockBackend(), "b2": newMockBackend(), "b3": newMockBackend()},
		Store:           store,
		Order:           []string{"b1", "b2", "b3"},
		CacheTTL:        5 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})

	stats := map[string]st.QuotaStat{
		"b1": {BytesUsed: 100, BytesLimit: 1000},
		"b2": {BytesUsed: 100, BytesLimit: 1000},
		"b3": {BytesUsed: 100, BytesLimit: 1000},
	}

	// b1 and b2 already have copies
	exclusion := map[string]bool{"b1": true, "b2": true}
	target := mgr.Replicator.FindReplicaTarget(context.Background(), stats, "key1", 50, exclusion)
	if target != "b3" {
		t.Errorf("expected b3, got %q", target)
	}
}

func TestFindReplicaTarget_SkipsFullBackends(t *testing.T) {
	t.Parallel()
	store := &mockStore{}
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": newMockBackend(), "b2": newMockBackend()},
		Store:           store,
		Order:           []string{"b1", "b2"},
		CacheTTL:        5 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})

	stats := map[string]st.QuotaStat{
		"b1": {BytesUsed: 100, BytesLimit: 1000},
		"b2": {BytesUsed: 999, BytesLimit: 1000}, // only 1 byte free
	}

	exclusion := map[string]bool{"b1": true}
	target := mgr.Replicator.FindReplicaTarget(context.Background(), stats, "key1", 50, exclusion)
	if target != "" {
		t.Errorf("expected empty (no space), got %q", target)
	}
}

func TestFindReplicaTarget_EmptyStats(t *testing.T) {
	t.Parallel()
	store := &mockStore{}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	target := mgr.Replicator.FindReplicaTarget(context.Background(), map[string]st.QuotaStat{}, "key1", 50, map[string]bool{})
	if target != "" {
		t.Errorf("expected empty with no quota stats, got %q", target)
	}
}

// -------------------------------------------------------------------------
// copyToReplica
// -------------------------------------------------------------------------

func TestCopyToReplica_Success(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b2 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": b1, "b2": b2},
		Store:           &mockStore{},
		Order:           []string{"b1", "b2"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})

	copies := []st.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}}
	source, err := mgr.Replicator.CopyToReplica(context.Background(), "key1", copies, "b2")
	if err != nil {
		t.Fatalf("copyToReplica: %v", err)
	}
	if source != "b1" {
		t.Errorf("expected source=b1, got %q", source)
	}
	if !b2.hasObject("key1") {
		t.Error("expected key1 on target b2")
	}
}

func TestCopyToReplica_FailoverToSecondCopy(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b1.getErr = errors.New("backend down")
	b2 := newMockBackend()
	_, _ = b2.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	b3 := newMockBackend()

	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": b1, "b2": b2, "b3": b3},
		Store:           &mockStore{},
		Order:           []string{"b1", "b2", "b3"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})

	copies := []st.ObjectLocation{
		{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
		{ObjectKey: "key1", BackendName: "b2", SizeBytes: 4},
	}
	source, err := mgr.Replicator.CopyToReplica(context.Background(), "key1", copies, "b3")
	if err != nil {
		t.Fatalf("copyToReplica should failover: %v", err)
	}
	if source != "b2" {
		t.Errorf("expected source=b2 (failover), got %q", source)
	}
	if !b3.hasObject("key1") {
		t.Error("expected key1 on target b3")
	}
}

func TestCopyToReplica_AllSourcesFail(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b1.getErr = errors.New("down")
	b2 := newMockBackend()

	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": b1, "b2": b2},
		Store:           &mockStore{},
		Order:           []string{"b1", "b2"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})

	copies := []st.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}}
	_, err := mgr.Replicator.CopyToReplica(context.Background(), "key1", copies, "b2")
	if err == nil {
		t.Fatal("expected error when all source copies fail")
	}
}

// -------------------------------------------------------------------------
// cleanupOrphan
// -------------------------------------------------------------------------

func TestCleanupOrphan_Success(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "orphan", bytes.NewReader([]byte("x")), 1, "", nil)

	mgr := newTestManager(&mockStore{}, map[string]*mockBackend{"b1": b1})

	mgr.Replicator.CleanupOrphan(context.Background(), "b1", "orphan", 1)
	if b1.hasObject("orphan") {
		t.Error("expected orphan to be deleted")
	}
}

func TestCleanupOrphan_BackendNotFound(t *testing.T) {
	t.Parallel()
	mgr := newTestManager(&mockStore{}, map[string]*mockBackend{"b1": newMockBackend()})

	// Should not panic for unknown backend
	mgr.Replicator.CleanupOrphan(context.Background(), "unknown", "orphan", 1)
}

func TestCleanupOrphan_DeleteFailure_EnqueuesCleanup(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b1.delErr = errors.New("delete failed")
	store := &mockStore{}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": b1})

	mgr.Replicator.CleanupOrphan(context.Background(), "b1", "orphan", 1)

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.enqueueCleanupCalls) != 1 {
		t.Fatalf("expected 1 enqueue call, got %d", len(store.enqueueCleanupCalls))
	}
	if store.enqueueCleanupCalls[0].reason != "replication_orphan" {
		t.Errorf("expected reason=replication_orphan, got %q", store.enqueueCleanupCalls[0].reason)
	}
}

// -------------------------------------------------------------------------
// Replicate edge cases
// -------------------------------------------------------------------------

func TestReplicate_RecordReplicaFails_CleansUpOrphan(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b2 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	store := &mockStore{
		getUnderReplicatedResp: []st.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
		},
		getQuotaStatsResp: map[string]st.QuotaStat{
			"b1": {BytesUsed: 100, BytesLimit: 1000},
			"b2": {BytesUsed: 100, BytesLimit: 1000},
		},
		recordReplicaErr: errors.New("db error"),
	}
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": b1, "b2": b2},
		Store:           store,
		Order:           []string{"b1", "b2"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})

	created, err := mgr.Replicator.Replicate(context.Background(), config.ReplicationConfig{
		Factor:    2,
		BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if created != 0 {
		t.Errorf("expected 0 created (record failed), got %d", created)
	}
	// Orphan should have been cleaned up from b2
	if b2.hasObject("key1") {
		t.Error("orphan should have been cleaned up from b2")
	}
}

func TestCopyToReplica_TargetBackendNotFound(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	mgr := newTestManager(&mockStore{}, map[string]*mockBackend{"b1": b1})

	copies := []st.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}}
	_, err := mgr.Replicator.CopyToReplica(context.Background(), "key1", copies, "nonexistent")
	if err == nil {
		t.Fatal("expected error when target backend not found")
	}
}

func TestCopyToReplica_TargetWriteFails(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	b2 := newMockBackend()
	b2.putErr = errors.New("write failed")

	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": b1, "b2": b2},
		Store:           &mockStore{},
		Order:           []string{"b1", "b2"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})

	copies := []st.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}}
	_, err := mgr.Replicator.CopyToReplica(context.Background(), "key1", copies, "b2")
	if err == nil {
		t.Fatal("expected error when target PutObject fails")
	}
}

func TestReplicateObject_NoTargetAvailable(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	store := &mockStore{
		getUnderReplicatedResp: []st.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
		},
		getQuotaStatsResp: map[string]st.QuotaStat{
			// Only b1 has space, but it already holds the copy
			"b1": {BytesUsed: 100, BytesLimit: 1000},
		},
		recordReplicaInserted: true,
	}
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": b1},
		Store:           store,
		Order:           []string{"b1"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})

	created, err := mgr.Replicator.Replicate(context.Background(), config.ReplicationConfig{
		Factor:    2,
		BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if created != 0 {
		t.Errorf("expected 0 created (no target), got %d", created)
	}
}

func TestReplicate_SourceGoneDuringReplication(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b2 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	store := &mockStore{
		getUnderReplicatedResp: []st.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
		},
		getQuotaStatsResp: map[string]st.QuotaStat{
			"b1": {BytesUsed: 100, BytesLimit: 1000},
			"b2": {BytesUsed: 100, BytesLimit: 1000},
		},
		recordReplicaInserted: false, // source was deleted during replication
	}
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": b1, "b2": b2},
		Store:           store,
		Order:           []string{"b1", "b2"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})

	created, err := mgr.Replicator.Replicate(context.Background(), config.ReplicationConfig{
		Factor:    2,
		BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if created != 0 {
		t.Errorf("expected 0 created (source gone), got %d", created)
	}
	// Orphan should have been cleaned up from b2
	if b2.hasObject("key1") {
		t.Error("orphan should have been cleaned up from b2")
	}
}

// -------------------------------------------------------------------------
// Health-aware replication
// -------------------------------------------------------------------------

// newTrippedCBBackend wraps a mock backend in a CircuitBreakerBackend and
// immediately trips the circuit breaker.
func newTrippedCBBackend(b *mockBackend, name string) *backend.CircuitBreakerBackend {
	cbb := backend.NewCircuitBreakerBackend(b, name, 1, time.Hour)
	_ = cbb.PostCheck(errors.New("forced failure"))
	return cbb
}

func TestReplicate_HealthAware_SkipsUnhealthyTarget(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b2 := newMockBackend()
	b3 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	// b2 is circuit-broken — should not be selected as target
	cbb2 := newTrippedCBBackend(b2, "b2")

	store := &mockStore{
		getUnderReplicatedExcludingResp: []st.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
		},
		getQuotaStatsResp: map[string]st.QuotaStat{
			"b1": {BytesUsed: 100, BytesLimit: 1000},
			"b2": {BytesUsed: 100, BytesLimit: 1000},
			"b3": {BytesUsed: 100, BytesLimit: 1000},
		},
		recordReplicaInserted: true,
	}
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": b1, "b2": cbb2, "b3": b3},
		Store:           store,
		Order:           []string{"b1", "b2", "b3"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})

	created, err := mgr.Replicator.Replicate(context.Background(), config.ReplicationConfig{
		Factor:             2,
		BatchSize:          10,
		UnhealthyThreshold: 0, // immediate — any open CB counts
	})
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if created != 1 {
		t.Errorf("expected 1 created, got %d", created)
	}
	if b2.hasObject("key1") {
		t.Error("unhealthy b2 should not have received a replica")
	}
	if !b3.hasObject("key1") {
		t.Error("expected key1 on healthy b3")
	}
}

func TestReplicate_HealthAware_PrefersHealthySource(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b2 := newMockBackend()
	b3 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	_, _ = b2.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	// b1 is circuit-broken — b2 should be preferred as source
	cbb1 := newTrippedCBBackend(b1, "b1")

	store := &mockStore{
		getUnderReplicatedExcludingResp: []st.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
			{ObjectKey: "key1", BackendName: "b2", SizeBytes: 4},
		},
		getQuotaStatsResp: map[string]st.QuotaStat{
			"b1": {BytesUsed: 100, BytesLimit: 1000},
			"b2": {BytesUsed: 100, BytesLimit: 1000},
			"b3": {BytesUsed: 100, BytesLimit: 1000},
		},
		recordReplicaInserted: true,
	}
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": cbb1, "b2": b2, "b3": b3},
		Store:           store,
		Order:           []string{"b1", "b2", "b3"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})

	created, err := mgr.Replicator.Replicate(context.Background(), config.ReplicationConfig{
		Factor:             3,
		BatchSize:          10,
		UnhealthyThreshold: 0,
	})
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if created != 1 {
		t.Errorf("expected 1 created, got %d", created)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.recordReplicaCalls) != 1 {
		t.Fatalf("expected 1 RecordReplica call, got %d", len(store.recordReplicaCalls))
	}
	if store.recordReplicaCalls[0].sourceBackend != "b2" {
		t.Errorf("expected source=b2 (healthy), got %q", store.recordReplicaCalls[0].sourceBackend)
	}
}

func TestReplicate_HealthAware_BelowThreshold(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b2 := newMockBackend()

	// b2 is circuit-broken but threshold is very high — should use normal query
	cbb2 := newTrippedCBBackend(b2, "b2")

	store := &mockStore{
		getUnderReplicatedResp: nil, // normal query: fully replicated
	}
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": b1, "b2": cbb2},
		Store:           store,
		Order:           []string{"b1", "b2"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})

	created, err := mgr.Replicator.Replicate(context.Background(), config.ReplicationConfig{
		Factor:             2,
		BatchSize:          10,
		UnhealthyThreshold: time.Hour, // CB just opened — below threshold
	})
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if created != 0 {
		t.Errorf("expected 0 created (below threshold), got %d", created)
	}
}

func TestUnhealthyBackends_NoCB(t *testing.T) {
	t.Parallel()
	mgr := newTestManager(&mockStore{}, map[string]*mockBackend{
		"b1": newMockBackend(),
		"b2": newMockBackend(),
	})
	names := mgr.Replicator.UnhealthyBackends(0)
	if len(names) != 0 {
		t.Errorf("expected empty, got %v", names)
	}
}

func TestIsBackendHealthy_NoCB(t *testing.T) {
	t.Parallel()
	mgr := newTestManager(&mockStore{}, map[string]*mockBackend{"b1": newMockBackend()})
	if !mgr.Replicator.IsBackendHealthy("b1") {
		t.Error("backend without CB wrapper should be healthy")
	}
}

func TestIsBackendHealthy_UnknownBackend(t *testing.T) {
	t.Parallel()
	mgr := newTestManager(&mockStore{}, map[string]*mockBackend{"b1": newMockBackend()})
	if mgr.Replicator.IsBackendHealthy("nonexistent") {
		t.Error("unknown backend should not be healthy")
	}
}

func TestIsBackendHealthy_CBHealthy(t *testing.T) {
	t.Parallel()
	cbb := backend.NewCircuitBreakerBackend(newMockBackend(), "b1", 3, time.Minute)
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": cbb},
		Store:           &mockStore{},
		Order:           []string{"b1"},
		CacheTTL:        5 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	if !mgr.Replicator.IsBackendHealthy("b1") {
		t.Error("healthy CB backend should report healthy")
	}
}

func TestIsBackendHealthy_CBUnhealthy(t *testing.T) {
	t.Parallel()
	cbb := newTrippedCBBackend(newMockBackend(), "b1")
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": cbb},
		Store:           &mockStore{},
		Order:           []string{"b1"},
		CacheTTL:        5 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	if mgr.Replicator.IsBackendHealthy("b1") {
		t.Error("tripped CB backend should report unhealthy")
	}
}
