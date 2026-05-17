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
	"sync"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// recordReplicaRecord tracks one RecordReplica invocation.
type recordReplicaRecord struct {
	key, targetBackend, sourceBackend string
}

// recordReplicaTracker accumulates RecordReplica calls and configurable
// returns.
type recordReplicaTracker struct {
	mu       sync.Mutex
	calls    []recordReplicaRecord
	size     int64
	inserted bool
	err      error
}

// stubRecordReplica returns a DoAndReturn capturing into rt.
func stubRecordReplica(rt *recordReplicaTracker) func(context.Context, string, string, string) (int64, bool, error) {
	return func(_ context.Context, key, target, source string) (int64, bool, error) {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		rt.calls = append(rt.calls, recordReplicaRecord{key: key, targetBackend: target, sourceBackend: source})
		return rt.size, rt.inserted, rt.err
	}
}

// replicatorEnqueueTracker captures EnqueueCleanup calls during a
// replicator test.
type replicatorEnqueueTracker struct {
	mu    sync.Mutex
	calls []core.CleanupItem
}

// stubReplicatorEnqueue returns a DoAndReturn for EnqueueCleanup.
func stubReplicatorEnqueue(et *replicatorEnqueueTracker) func(context.Context, string, string, string, int64) error {
	return func(_ context.Context, backend, key, reason string, size int64) error {
		et.mu.Lock()
		defer et.mu.Unlock()
		et.calls = append(et.calls, core.CleanupItem{
			BackendName: backend, ObjectKey: key, Reason: reason, SizeBytes: size,
		})
		return nil
	}
}

// TestGroupByKey_Groups verifies the group-by-key contract.
func TestGroupByKey_Groups(t *testing.T) {
	t.Parallel()
	locations := []core.ObjectLocation{
		{ObjectKey: "a", BackendName: "b1"},
		{ObjectKey: "a", BackendName: "b2"},
		{ObjectKey: "b", BackendName: "b1"},
	}
	grouped := core.GroupByKey(locations)
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

// TestGroupByKey_Empty asserts the empty input contract.
func TestGroupByKey_Empty(t *testing.T) {
	t.Parallel()
	if grouped := core.GroupByKey(nil); len(grouped) != 0 {
		t.Errorf("expected 0 groups, got %d", len(grouped))
	}
}

// TestReplicate_NoUnderReplicatedObjects asserts the no-work case.
func TestReplicate_NoUnderReplicatedObjects(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	created, err := workers.Replicator.Replicate(context.Background(), config.ReplicationConfig{
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

// TestReplicate_QueryError surfaces a store-side query failure.
func TestReplicate_QueryError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetUnderReplicatedObjects(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, errors.New("db down")).AnyTimes()
	storetest.Permissive(store)

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	if _, err := workers.Replicator.Replicate(context.Background(), config.ReplicationConfig{
		Factor:    2,
		BatchSize: 10,
	}); err == nil {
		t.Fatal("expected error from GetUnderReplicatedObjects failure")
	}
}

// replicateSuccessStubs wires the per-call stubs replicator success
// tests share: under-replicated returns, GetBackendWithSpace chosen-from-
// eligible behaviour, and a successful RecordReplica.
func replicateSuccessStubs(store *storetest.MockMetadataStore, locations []core.ObjectLocation, rt *recordReplicaTracker) {
	store.EXPECT().GetUnderReplicatedObjects(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(locations, nil).AnyTimes()
	store.EXPECT().GetUnderReplicatedObjectsExcluding(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(locations, nil).AnyTimes()
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ int64, eligible []string) (string, error) {
			if len(eligible) > 0 {
				return eligible[0], nil
			}
			return "", core.ErrNoSpaceAvailable
		}).AnyTimes()
	store.EXPECT().GetLeastUtilizedBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ int64, eligible []string) (string, error) {
			if len(eligible) > 0 {
				return eligible[0], nil
			}
			return "", core.ErrNoSpaceAvailable
		}).AnyTimes()
	store.EXPECT().RecordReplica(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubRecordReplica(rt)).AnyTimes()
}

// TestReplicate_Success drives the happy path.
func TestReplicate_Success(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b2 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	rt := &recordReplicaTracker{inserted: true}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	replicateSuccessStubs(store,
		[]core.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}},
		rt)
	storetest.Permissive(store)

	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": b1, "b2": b2},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1", "b2"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers

	created, err := workers.Replicator.Replicate(context.Background(), config.ReplicationConfig{
		Factor:    2,
		BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if created != 1 {
		t.Errorf("expected 1 created, got %d", created)
	}
	if !b2.hasObject("key1") {
		t.Error("expected key1 on b2 after replication")
	}
}

// TestFindReplicaTarget_ExcludesExistingCopies pins the exclusion
// filter.
func TestFindReplicaTarget_ExcludesExistingCopies(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ int64, eligible []string) (string, error) {
			if len(eligible) > 0 {
				return eligible[0], nil
			}
			return "", nil
		}).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": newMockBackend(), "b2": newMockBackend(), "b3": newMockBackend()},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1", "b2", "b3"},
		CacheTTL:        5 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers

	exclusion := map[string]bool{"b1": true, "b2": true}
	target := workers.Replicator.FindReplicaTarget(context.Background(), "key1", 50, exclusion)
	if target != "b3" {
		t.Errorf("expected b3, got %q", target)
	}
}

// TestFindReplicaTarget_SkipsFullBackends asserts a too-full backend is
// skipped.
func TestFindReplicaTarget_SkipsFullBackends(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": newMockBackend(), "b2": newMockBackend()},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1", "b2"},
		CacheTTL:        5 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers

	exclusion := map[string]bool{"b1": true}
	if target := workers.Replicator.FindReplicaTarget(context.Background(), "key1", 50, exclusion); target != "" {
		t.Errorf("expected empty (no space), got %q", target)
	}
}

// TestSelectReplicaTarget_NoSpaceAvailable asserts the no-space short-
// circuit.
func TestSelectReplicaTarget_NoSpaceAvailable(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
		Return("", core.ErrNoSpaceAvailable).AnyTimes()
	store.EXPECT().GetLeastUtilizedBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		Return("", core.ErrNoSpaceAvailable).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": newMockBackend(), "b2": newMockBackend()},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1", "b2"},
		CacheTTL:        5 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers

	exclusion := map[string]bool{"b1": true}
	if target := workers.Replicator.FindReplicaTarget(context.Background(), "key1", 50, exclusion); target != "" {
		t.Errorf("expected empty (no space available), got %q", target)
	}
}

// TestFindReplicaTarget_EmptyStats handles a missing stats map.
func TestFindReplicaTarget_EmptyStats(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	if target := workers.Replicator.FindReplicaTarget(context.Background(), "key1", 50, map[string]bool{}); target != "" {
		t.Errorf("expected empty with no quota stats, got %q", target)
	}
}

// TestCopyToReplica_Success drives the happy stream-copy path.
func TestCopyToReplica_Success(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b2 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	store := newPermissiveMock(t)
	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": b1, "b2": b2},
		Stores:          testStoresFromMock(store),
		Order:           []string{"b1", "b2"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers

	copies := []core.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}}
	source, _, err := workers.Replicator.CopyToReplica(context.Background(), "key1", copies, "b2")
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

// TestCopyToReplica_FailoverToSecondCopy asserts a Get failure on the
// first source falls through to the second.
func TestCopyToReplica_FailoverToSecondCopy(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b1.getErr = errors.New("backend down")
	b2 := newMockBackend()
	_, _ = b2.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	b3 := newMockBackend()

	store := newPermissiveMock(t)
	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": b1, "b2": b2, "b3": b3},
		Stores:          testStoresFromMock(store),
		Order:           []string{"b1", "b2", "b3"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers

	copies := []core.ObjectLocation{
		{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
		{ObjectKey: "key1", BackendName: "b2", SizeBytes: 4},
	}
	source, _, err := workers.Replicator.CopyToReplica(context.Background(), "key1", copies, "b3")
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

// TestCopyToReplica_AllSourcesFail surfaces failure when every source
// errors.
func TestCopyToReplica_AllSourcesFail(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b1.getErr = errors.New("down")
	b2 := newMockBackend()

	store := newPermissiveMock(t)
	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": b1, "b2": b2},
		Stores:          testStoresFromMock(store),
		Order:           []string{"b1", "b2"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers

	copies := []core.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}}
	if _, _, err := workers.Replicator.CopyToReplica(context.Background(), "key1", copies, "b2"); err == nil {
		t.Fatal("expected error when all source copies fail")
	}
}

// TestCleanupOrphan_Success deletes an orphan from the source backend.
func TestCleanupOrphan_Success(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "orphan", bytes.NewReader([]byte("x")), 1, "", nil)

	_, workers := newTestManagerWithWorkers(t, newPermissiveMock(t), map[string]*mockBackend{"b1": b1})

	workers.Replicator.CleanupOrphan(context.Background(), "b1", "orphan", 1)
	if b1.hasObject("orphan") {
		t.Error("expected orphan to be deleted")
	}
}

// TestCleanupOrphan_BackendNotFound asserts the no-op on a missing
// backend.
func TestCleanupOrphan_BackendNotFound(t *testing.T) {
	t.Parallel()
	_, workers := newTestManagerWithWorkers(t, newPermissiveMock(t), map[string]*mockBackend{"b1": newMockBackend()})
	workers.Replicator.CleanupOrphan(context.Background(), "unknown", "orphan", 1)
}

// TestCleanupOrphan_DeleteFailure_EnqueuesCleanup asserts a backend
// delete failure enqueues a replication_orphan cleanup row.
func TestCleanupOrphan_DeleteFailure_EnqueuesCleanup(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b1.delErr = errors.New("delete failed")

	et := &replicatorEnqueueTracker{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubReplicatorEnqueue(et)).AnyTimes()
	storetest.Permissive(store)

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": b1})
	workers.Replicator.CleanupOrphan(context.Background(), "b1", "orphan", 1)

	if len(et.calls) != 1 {
		t.Fatalf("expected 1 enqueue call, got %d", len(et.calls))
	}
	if et.calls[0].Reason != "replication_orphan" {
		t.Errorf("expected reason=replication_orphan, got %q", et.calls[0].Reason)
	}
}

// TestReplicate_RecordReplicaFails_CleansUpOrphan asserts the
// orphan-cleanup branch when RecordReplica returns an error.
func TestReplicate_RecordReplicaFails_CleansUpOrphan(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b2 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	rt := &recordReplicaTracker{err: errors.New("db error")}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	replicateSuccessStubs(store,
		[]core.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}},
		rt)
	storetest.Permissive(store)

	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": b1, "b2": b2},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1", "b2"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers

	created, err := workers.Replicator.Replicate(context.Background(), config.ReplicationConfig{
		Factor:    2,
		BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if created != 0 {
		t.Errorf("expected 0 created (record failed), got %d", created)
	}
	if b2.hasObject("key1") {
		t.Error("orphan should have been cleaned up from b2")
	}
}

// TestCopyToReplica_TargetBackendNotFound surfaces an unknown-target
// failure.
func TestCopyToReplica_TargetBackendNotFound(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	_, workers := newTestManagerWithWorkers(t, newPermissiveMock(t), map[string]*mockBackend{"b1": b1})

	copies := []core.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}}
	if _, _, err := workers.Replicator.CopyToReplica(context.Background(), "key1", copies, "nonexistent"); err == nil {
		t.Fatal("expected error when target backend not found")
	}
}

// TestCopyToReplica_TargetWriteFails surfaces a target PutObject
// failure.
func TestCopyToReplica_TargetWriteFails(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	b2 := newMockBackend()
	b2.putErr = errors.New("write failed")

	store := newPermissiveMock(t)
	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": b1, "b2": b2},
		Stores:          testStoresFromMock(store),
		Order:           []string{"b1", "b2"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers

	copies := []core.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}}
	if _, _, err := workers.Replicator.CopyToReplica(context.Background(), "key1", copies, "b2"); err == nil {
		t.Fatal("expected error when target PutObject fails")
	}
}

// TestReplicateObject_NoTargetAvailable asserts a replicate run with no
// eligible target produces zero successes.
func TestReplicateObject_NoTargetAvailable(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	rt := &recordReplicaTracker{inserted: true}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	replicateSuccessStubs(store,
		[]core.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}},
		rt)
	storetest.Permissive(store)

	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": b1},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers

	created, err := workers.Replicator.Replicate(context.Background(), config.ReplicationConfig{
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

// TestReplicate_SourceGoneDuringReplication asserts the orphan-cleanup
// branch when the source-row inserted=false.
func TestReplicate_SourceGoneDuringReplication(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b2 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	rt := &recordReplicaTracker{inserted: false}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	replicateSuccessStubs(store,
		[]core.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}},
		rt)
	storetest.Permissive(store)

	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": b1, "b2": b2},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1", "b2"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers

	created, err := workers.Replicator.Replicate(context.Background(), config.ReplicationConfig{
		Factor:    2,
		BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if created != 0 {
		t.Errorf("expected 0 created (source gone), got %d", created)
	}
	if b2.hasObject("key1") {
		t.Error("orphan should have been cleaned up from b2")
	}
}

// newTrippedCBBackend wraps a mock backend in a CircuitBreakerBackend
// and immediately trips the circuit.
func newTrippedCBBackend(b *mockBackend, name string) *backend.CircuitBreakerBackend {
	cbb := backend.NewCircuitBreakerBackend(b, name, 1, time.Hour)
	_ = cbb.PostCheck(errors.New("forced failure"))
	return cbb
}

// TestReplicate_HealthAware_SkipsUnhealthyTarget asserts unhealthy
// backends are excluded from target selection.
func TestReplicate_HealthAware_SkipsUnhealthyTarget(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b2 := newMockBackend()
	b3 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	cbb2 := newTrippedCBBackend(b2, "b2")

	rt := &recordReplicaTracker{inserted: true}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	replicateSuccessStubs(store,
		[]core.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}},
		rt)
	storetest.Permissive(store)

	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": b1, "b2": cbb2, "b3": b3},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1", "b2", "b3"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers

	created, err := workers.Replicator.Replicate(context.Background(), config.ReplicationConfig{
		Factor:             2,
		BatchSize:          10,
		UnhealthyThreshold: 0,
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

// TestReplicate_HealthAware_PrefersHealthySource asserts the source
// preference picks a healthy copy when one source is circuit-broken.
func TestReplicate_HealthAware_PrefersHealthySource(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b2 := newMockBackend()
	b3 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	_, _ = b2.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	cbb1 := newTrippedCBBackend(b1, "b1")

	rt := &recordReplicaTracker{inserted: true}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	replicateSuccessStubs(store,
		[]core.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
			{ObjectKey: "key1", BackendName: "b2", SizeBytes: 4},
		},
		rt)
	storetest.Permissive(store)

	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": cbb1, "b2": b2, "b3": b3},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1", "b2", "b3"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers

	created, err := workers.Replicator.Replicate(context.Background(), config.ReplicationConfig{
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

	if len(rt.calls) != 1 {
		t.Fatalf("expected 1 RecordReplica call, got %d", len(rt.calls))
	}
	if rt.calls[0].sourceBackend != "b2" {
		t.Errorf("expected source=b2 (healthy), got %q", rt.calls[0].sourceBackend)
	}
}

// TestReplicate_UsesRecordedSize_NotFirstCopy regression-tests the
// recorded-size accounting fix.
func TestReplicate_UsesRecordedSize_NotFirstCopy(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b2 := newMockBackend()
	b3 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	_, _ = b2.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	cbb1 := newTrippedCBBackend(b1, "b1")

	rt := &recordReplicaTracker{inserted: true, size: 200}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	replicateSuccessStubs(store,
		[]core.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 999},
			{ObjectKey: "key1", BackendName: "b2", SizeBytes: 200},
		},
		rt)
	storetest.Permissive(store)

	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": cbb1, "b2": b2, "b3": b3},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1", "b2", "b3"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers

	created, err := workers.Replicator.Replicate(context.Background(), config.ReplicationConfig{
		Factor:             3,
		BatchSize:          10,
		UnhealthyThreshold: 0,
	})
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if created != 1 {
		t.Fatalf("expected 1 created, got %d", created)
	}

	if got := mgr.Usage().Backend().Load("b2", counter.FieldEgressBytes); got != 200 {
		t.Errorf("source egress = %d, want 200 (recorded size, not copies[0].SizeBytes)", got)
	}
	if got := mgr.Usage().Backend().Load("b3", counter.FieldIngressBytes); got != 200 {
		t.Errorf("target ingress = %d, want 200 (recorded size, not copies[0].SizeBytes)", got)
	}
}

// TestReplicate_HealthAware_BelowThreshold asserts the threshold gating.
func TestReplicate_HealthAware_BelowThreshold(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b2 := newMockBackend()
	cbb2 := newTrippedCBBackend(b2, "b2")

	store := newPermissiveMock(t)
	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": b1, "b2": cbb2},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1", "b2"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers

	created, err := workers.Replicator.Replicate(context.Background(), config.ReplicationConfig{
		Factor:             2,
		BatchSize:          10,
		UnhealthyThreshold: time.Hour,
	})
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if created != 0 {
		t.Errorf("expected 0 created (below threshold), got %d", created)
	}
}

// TestUnhealthyBackends_NoCB asserts an empty slice when no backend has
// a circuit breaker.
func TestUnhealthyBackends_NoCB(t *testing.T) {
	t.Parallel()
	_, workers := newTestManagerWithWorkers(t, newPermissiveMock(t), map[string]*mockBackend{
		"b1": newMockBackend(),
		"b2": newMockBackend(),
	})
	if names := workers.Replicator.UnhealthyBackends(0); len(names) != 0 {
		t.Errorf("expected empty, got %v", names)
	}
}

// TestIsBackendHealthy_NoCB asserts a non-CB-wrapped backend is treated
// as healthy.
func TestIsBackendHealthy_NoCB(t *testing.T) {
	t.Parallel()
	_, workers := newTestManagerWithWorkers(t, newPermissiveMock(t), map[string]*mockBackend{"b1": newMockBackend()})
	if !workers.Replicator.IsBackendHealthy("b1") {
		t.Error("backend without CB wrapper should be healthy")
	}
}

// TestIsBackendHealthy_UnknownBackend asserts an unknown backend is not
// healthy.
func TestIsBackendHealthy_UnknownBackend(t *testing.T) {
	t.Parallel()
	_, workers := newTestManagerWithWorkers(t, newPermissiveMock(t), map[string]*mockBackend{"b1": newMockBackend()})
	if workers.Replicator.IsBackendHealthy("nonexistent") {
		t.Error("unknown backend should not be healthy")
	}
}

// TestIsBackendHealthy_CBHealthy asserts a closed-circuit backend
// reports healthy.
func TestIsBackendHealthy_CBHealthy(t *testing.T) {
	t.Parallel()
	cbb := backend.NewCircuitBreakerBackend(newMockBackend(), "b1", 3, time.Minute)
	store := newPermissiveMock(t)
	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": cbb},
		Stores:          testStoresFromMock(store),
		Order:           []string{"b1"},
		CacheTTL:        5 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers
	if !workers.Replicator.IsBackendHealthy("b1") {
		t.Error("healthy CB backend should report healthy")
	}
}

// TestIsBackendHealthy_CBUnhealthy asserts a tripped CB reports
// unhealthy.
func TestIsBackendHealthy_CBUnhealthy(t *testing.T) {
	t.Parallel()
	cbb := newTrippedCBBackend(newMockBackend(), "b1")
	store := newPermissiveMock(t)
	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": cbb},
		Stores:          testStoresFromMock(store),
		Order:           []string{"b1"},
		CacheTTL:        5 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers
	if workers.Replicator.IsBackendHealthy("b1") {
		t.Error("tripped CB backend should report unhealthy")
	}
}
