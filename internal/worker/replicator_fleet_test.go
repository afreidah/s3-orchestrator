// -------------------------------------------------------------------------------
// Replicator Tests - Replica Creation and Failover
//
// Author: Alex Freidah
//
// Tests for background replication: finding under-replicated objects, copying
// data between backends with failover, conditional replica recording, and
// orphan cleanup on failure.
// -------------------------------------------------------------------------------

package worker

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

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

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

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
	store := newPermissiveStore(t)
	w := newReplicatorFor(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, &fleetOpts{})

	sum, err := w.Replicate(context.Background(), config.ReplicationConfig{
		Factor:    2,
		BatchSize: 10,
	}, nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if sum.CopiesCreated != 0 {
		t.Errorf("expected 0 created, got %d", sum.CopiesCreated)
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

	w := newReplicatorFor(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, &fleetOpts{})

	if _, err := w.Replicate(context.Background(), config.ReplicationConfig{
		Factor:    2,
		BatchSize: 10,
	}, nil); err == nil {
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
	store.EXPECT().RecordReplica(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubRecordReplica(rt)).AnyTimes()
}

// TestReplicate_Success drives the happy path.
func TestReplicate_Success(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b2 := backendtest.NewInMemory()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	rt := &recordReplicaTracker{inserted: true}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	replicateSuccessStubs(store,
		[]core.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}},
		rt)
	storetest.Permissive(store)

	w := newReplicatorFor(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, &fleetOpts{Order: []string{"b1", "b2"}})

	sum, err := w.Replicate(context.Background(), config.ReplicationConfig{
		Factor:    2,
		BatchSize: 10,
	}, nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if sum.CopiesCreated != 1 {
		t.Errorf("expected 1 created, got %d", sum.CopiesCreated)
	}
	if !b2.Has("key1") {
		t.Error("expected key1 on b2 after replication")
	}
}

// TestFindReplicaTarget_ExcludesExistingCopies pins the exclusion
// filter.
func TestFindReplicaTarget_ExcludesExistingCopies(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	storetest.Permissive(store)

	w := newReplicatorFor(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory(), "b2": backendtest.NewInMemory(), "b3": backendtest.NewInMemory()}, &fleetOpts{Order: []string{"b1", "b2", "b3"}})

	exclusion := map[string]bool{"b1": true, "b2": true}
	target := w.FindReplicaTarget(context.Background(), "key1", 50, exclusion)
	if target != "b3" {
		t.Errorf("expected b3, got %q", target)
	}
}

// TestReplicate_FullTargetRecordsNoCopy asserts a target without room produces
// no copy.
//
// The refusal is the conditional insert declining the row, not the ranking
// passing the backend over: ranking only proposes an order, and a backend that
// looked roomy when it was ranked can be full by the time the copy lands. The
// bytes written to it become an orphan, which the cleanup queue owns.
func TestReplicate_FullTargetRecordsNoCopy(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetUnderReplicatedObjects(gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 50}}, nil).AnyTimes()
	// Every target declines, which is how a full backend reports itself.
	store.EXPECT().RecordReplica(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(int64(0), false, nil).AnyTimes()
	storetest.Permissive(store)

	src := backendtest.NewInMemory()
	_, _ = src.PutObject(context.Background(), "key1", bytes.NewReader([]byte("0123456789")), 10, "text/plain", nil)
	w := newReplicatorFor(t, store, map[string]backend.ObjectBackend{
		"b1": src, "b2": backendtest.NewInMemory(),
	}, &fleetOpts{Order: []string{"b1", "b2"}})

	out, err := w.Replicate(context.Background(), config.ReplicationConfig{Factor: 2, BatchSize: 10}, nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if out.CopiesCreated != 0 {
		t.Errorf("created %d copies, want 0 when every target declines the row", out.CopiesCreated)
	}
}

// TestCopyToReplica_FailoverToSecondCopy asserts a Get failure on the
// first source falls through to the second.
func TestCopyToReplica_FailoverToSecondCopy(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b1.GetErr = errors.New("backend down")
	b2 := backendtest.NewInMemory()
	_, _ = b2.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	b3 := backendtest.NewInMemory()

	store := newPermissiveStore(t)
	w := newReplicatorFor(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2, "b3": b3}, &fleetOpts{Order: []string{"b1", "b2", "b3"}})

	copies := []core.ObjectLocation{
		{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
		{ObjectKey: "key1", BackendName: "b2", SizeBytes: 4},
	}
	source, err := w.CopyToReplica(context.Background(), "key1", copies, "b3")
	if err != nil {
		t.Fatalf("copyToReplica should failover: %v", err)
	}
	if source.BackendName != "b2" {
		t.Errorf("expected source=b2 (failover), got %q", source.BackendName)
	}
	if !b3.Has("key1") {
		t.Error("expected key1 on target b3")
	}
}

// TestCopyToReplica_DoesNotMutateInputSlice pins issue #904: the caller's
// copies slice must keep the order it was passed in. The previous
// implementation sorted in place, so an outer loop that reused the same
// slice across iterations saw the post-sort order on every call after
// the first. Builds an input where IsBackendHealthy disagrees with the
// input order (the first entry references a backend missing from the
// fleet so it scores as unhealthy) and asserts the slice is unchanged
// after the call.
func TestCopyToReplica_DoesNotMutateInputSlice(t *testing.T) {
	t.Parallel()
	b2 := backendtest.NewInMemory()
	_, _ = b2.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	b3 := backendtest.NewInMemory()

	store := newPermissiveStore(t)
	w := newReplicatorFor(t, store, map[string]backend.ObjectBackend{"b2": b2, "b3": b3}, &fleetOpts{Order: []string{"b2", "b3"}})

	copies := []core.ObjectLocation{
		{ObjectKey: "key1", BackendName: "gone", SizeBytes: 4},
		{ObjectKey: "key1", BackendName: "b2", SizeBytes: 4},
	}
	before := []string{copies[0].BackendName, copies[1].BackendName}

	if _, err := w.CopyToReplica(context.Background(), "key1", copies, "b3"); err != nil {
		t.Fatalf("CopyToReplica: %v", err)
	}

	after := []string{copies[0].BackendName, copies[1].BackendName}
	if before[0] != after[0] || before[1] != after[1] {
		t.Errorf("input slice mutated: before=%v after=%v", before, after)
	}
}

// TestCleanupOrphan_Success deletes an orphan from the source backend.
func TestCleanupOrphan_Success(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	_, _ = b1.PutObject(context.Background(), "orphan", bytes.NewReader([]byte("x")), 1, "", nil)

	w := newReplicatorFor(t, newPermissiveStore(t), map[string]backend.ObjectBackend{"b1": b1}, &fleetOpts{})

	w.CleanupOrphan(context.Background(), "b1", "orphan", 1)
	if b1.Has("orphan") {
		t.Error("expected orphan to be deleted")
	}
}

// TestCleanupOrphan_BackendNotFound asserts the no-op on a missing
// backend.
func TestCleanupOrphan_BackendNotFound(t *testing.T) {
	t.Parallel()
	w := newReplicatorFor(t, newPermissiveStore(t), map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, &fleetOpts{})

	w.CleanupOrphan(context.Background(), "unknown", "orphan", 1)
}

// TestCleanupOrphan_DeleteFailure_EnqueuesCleanup asserts a backend
// delete failure enqueues a replication_orphan cleanup row.
func TestCleanupOrphan_DeleteFailure_EnqueuesCleanup(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b1.DeleteErr = errors.New("delete failed")

	et := &replicatorEnqueueTracker{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubReplicatorEnqueue(et)).AnyTimes()
	storetest.Permissive(store)

	w := newReplicatorFor(t, store, map[string]backend.ObjectBackend{"b1": b1}, &fleetOpts{})

	w.CleanupOrphan(context.Background(), "b1", "orphan", 1)

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
	b1 := backendtest.NewInMemory()
	b2 := backendtest.NewInMemory()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	rt := &recordReplicaTracker{err: errors.New("db error")}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	replicateSuccessStubs(store,
		[]core.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}},
		rt)
	storetest.Permissive(store)

	w := newReplicatorFor(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, &fleetOpts{Order: []string{"b1", "b2"}})

	sum, err := w.Replicate(context.Background(), config.ReplicationConfig{
		Factor:    2,
		BatchSize: 10,
	}, nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if sum.CopiesCreated != 0 {
		t.Errorf("expected 0 created (record failed), got %d", sum.CopiesCreated)
	}
	if b2.Has("key1") {
		t.Error("orphan should have been cleaned up from b2")
	}
}

// TestCopyToReplica_TargetBackendNotFound surfaces an unknown-target
// failure.
func TestCopyToReplica_TargetBackendNotFound(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	w := newReplicatorFor(t, newPermissiveStore(t), map[string]backend.ObjectBackend{"b1": b1}, &fleetOpts{})

	copies := []core.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}}
	if _, err := w.CopyToReplica(context.Background(), "key1", copies, "nonexistent"); err == nil {
		t.Fatal("expected error when target backend not found")
	}
}

// TestCopyToReplica_TargetWriteFails surfaces a target PutObject
// failure.
func TestCopyToReplica_TargetWriteFails(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	b2 := backendtest.NewInMemory()
	b2.PutErr = errors.New("write failed")

	store := newPermissiveStore(t)
	w := newReplicatorFor(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, &fleetOpts{Order: []string{"b1", "b2"}})

	copies := []core.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}}
	if _, err := w.CopyToReplica(context.Background(), "key1", copies, "b2"); err == nil {
		t.Fatal("expected error when target PutObject fails")
	}
}

// TestReplicateObject_NoTargetAvailable asserts a replicate run with no
// eligible target produces zero successes.
func TestReplicateObject_NoTargetAvailable(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	rt := &recordReplicaTracker{inserted: true}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	replicateSuccessStubs(store,
		[]core.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}},
		rt)
	storetest.Permissive(store)

	w := newReplicatorFor(t, store, map[string]backend.ObjectBackend{"b1": b1}, &fleetOpts{Order: []string{"b1"}})

	sum, err := w.Replicate(context.Background(), config.ReplicationConfig{
		Factor:    2,
		BatchSize: 10,
	}, nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if sum.CopiesCreated != 0 {
		t.Errorf("expected 0 created (no target), got %d", sum.CopiesCreated)
	}
}

// TestReplicate_SourceGoneDuringReplication asserts the orphan-cleanup
// branch when the source-row inserted=false.
func TestReplicate_SourceGoneDuringReplication(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b2 := backendtest.NewInMemory()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	rt := &recordReplicaTracker{inserted: false}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	replicateSuccessStubs(store,
		[]core.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}},
		rt)
	storetest.Permissive(store)

	w := newReplicatorFor(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, &fleetOpts{Order: []string{"b1", "b2"}})

	sum, err := w.Replicate(context.Background(), config.ReplicationConfig{
		Factor:    2,
		BatchSize: 10,
	}, nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if sum.CopiesCreated != 0 {
		t.Errorf("expected 0 created (source gone), got %d", sum.CopiesCreated)
	}
	if b2.Has("key1") {
		t.Error("orphan should have been cleaned up from b2")
	}
}

// newTrippedCBBackend wraps a mock backend in a CircuitBreakerBackend
// and immediately trips the circuit.
func newTrippedCBBackend(b *backendtest.InMemory, name string) *backend.CircuitBreakerBackend {
	cbb := backend.NewCircuitBreakerBackend(b, backend.CircuitBreakerConfig{Name: name, Threshold: 1, Timeout: time.Hour})
	_ = cbb.PostCheck(errors.New("forced failure"))
	return cbb
}

// TestReplicate_HealthAware_SkipsUnhealthyTarget asserts unhealthy
// backends are excluded from target selection.
func TestReplicate_HealthAware_SkipsUnhealthyTarget(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b2 := backendtest.NewInMemory()
	b3 := backendtest.NewInMemory()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	cbb2 := newTrippedCBBackend(b2, "b2")

	rt := &recordReplicaTracker{inserted: true}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	replicateSuccessStubs(store,
		[]core.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}},
		rt)
	storetest.Permissive(store)

	w := newReplicatorFor(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": cbb2, "b3": b3}, &fleetOpts{Order: []string{"b1", "b2", "b3"}})

	sum, err := w.Replicate(context.Background(), config.ReplicationConfig{
		Factor:             2,
		BatchSize:          10,
		UnhealthyThreshold: 0,
	}, nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if sum.CopiesCreated != 1 {
		t.Errorf("expected 1 created, got %d", sum.CopiesCreated)
	}
	if b2.Has("key1") {
		t.Error("unhealthy b2 should not have received a replica")
	}
	if !b3.Has("key1") {
		t.Error("expected key1 on healthy b3")
	}
}

// TestReplicate_HealthAware_PrefersHealthySource asserts the source
// preference picks a healthy copy when one source is circuit-broken.
func TestReplicate_HealthAware_PrefersHealthySource(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b2 := backendtest.NewInMemory()
	b3 := backendtest.NewInMemory()
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

	fleet, coord := newFleet(t, store, map[string]backend.ObjectBackend{"b1": cbb1, "b2": b2, "b3": b3},
		&fleetOpts{Order: []string{"b1", "b2", "b3"}})
	w := newTestReplicator(fleet, coord, store)

	sum, err := w.Replicate(context.Background(), config.ReplicationConfig{
		Factor:             3,
		BatchSize:          10,
		UnhealthyThreshold: 0,
	}, nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if sum.CopiesCreated != 1 {
		t.Errorf("expected 1 created, got %d", sum.CopiesCreated)
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
	b1 := backendtest.NewInMemory()
	b2 := backendtest.NewInMemory()
	b3 := backendtest.NewInMemory()
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

	fleet, coord := newFleet(t, store, map[string]backend.ObjectBackend{"b1": cbb1, "b2": b2, "b3": b3},
		&fleetOpts{Order: []string{"b1", "b2", "b3"}})
	w := newTestReplicator(fleet, coord, store)

	sum, err := w.Replicate(context.Background(), config.ReplicationConfig{
		Factor:             3,
		BatchSize:          10,
		UnhealthyThreshold: 0,
	}, nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if sum.CopiesCreated != 1 {
		t.Fatalf("expected 1 created, got %d", sum.CopiesCreated)
	}

	if got := fleet.Usage().Backend().Load("b2", counter.FieldEgressBytes); got != 200 {
		t.Errorf("source egress = %d, want 200 (recorded size, not copies[0].SizeBytes)", got)
	}
	if got := fleet.Usage().Backend().Load("b3", counter.FieldIngressBytes); got != 200 {
		t.Errorf("target ingress = %d, want 200 (recorded size, not copies[0].SizeBytes)", got)
	}
}

// TestReplicate_HealthAware_BelowThreshold asserts the threshold gating.
func TestReplicate_HealthAware_BelowThreshold(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b2 := backendtest.NewInMemory()
	cbb2 := newTrippedCBBackend(b2, "b2")

	store := newPermissiveStore(t)
	w := newReplicatorFor(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": cbb2}, &fleetOpts{Order: []string{"b1", "b2"}})

	sum, err := w.Replicate(context.Background(), config.ReplicationConfig{
		Factor:             2,
		BatchSize:          10,
		UnhealthyThreshold: time.Hour,
	}, nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if sum.CopiesCreated != 0 {
		t.Errorf("expected 0 created (below threshold), got %d", sum.CopiesCreated)
	}
}

// TestUnhealthyBackends_NoCB asserts an empty slice when no backend has
// a circuit breaker.
func TestUnhealthyBackends_NoCB(t *testing.T) {
	t.Parallel()
	w := newReplicatorFor(t, newPermissiveStore(t), map[string]backend.ObjectBackend{
		"b1": backendtest.NewInMemory(),
		"b2": backendtest.NewInMemory(),
	}, &fleetOpts{})

	if names := w.UnhealthyBackends(0); len(names) != 0 {
		t.Errorf("expected empty, got %v", names)
	}
}

// TestIsBackendHealthy_NoCB asserts a non-CB-wrapped backend is treated
// as healthy.
func TestIsBackendHealthy_NoCB(t *testing.T) {
	t.Parallel()
	w := newReplicatorFor(t, newPermissiveStore(t), map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, &fleetOpts{})

	if !w.IsBackendHealthy("b1") {
		t.Error("backend without CB wrapper should be healthy")
	}
}

// TestIsBackendHealthy_UnknownBackend asserts an unknown backend is not
// healthy.
func TestIsBackendHealthy_UnknownBackend(t *testing.T) {
	t.Parallel()
	w := newReplicatorFor(t, newPermissiveStore(t), map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, &fleetOpts{})

	if w.IsBackendHealthy("nonexistent") {
		t.Error("unknown backend should not be healthy")
	}
}

// TestIsBackendHealthy_CBHealthy asserts a closed-circuit backend
// reports healthy.
func TestIsBackendHealthy_CBHealthy(t *testing.T) {
	t.Parallel()
	cbb := backend.NewCircuitBreakerBackend(backendtest.NewInMemory(), backend.CircuitBreakerConfig{Name: "b1", Threshold: 3, Timeout: time.Minute})
	store := newPermissiveStore(t)
	w := newReplicatorFor(t, store, map[string]backend.ObjectBackend{"b1": cbb}, &fleetOpts{Order: []string{"b1"}})
	if !w.IsBackendHealthy("b1") {
		t.Error("healthy CB backend should report healthy")
	}
}

// TestIsBackendHealthy_CBUnhealthy asserts a tripped CB reports
// unhealthy.
func TestIsBackendHealthy_CBUnhealthy(t *testing.T) {
	t.Parallel()
	cbb := newTrippedCBBackend(backendtest.NewInMemory(), "b1")
	store := newPermissiveStore(t)
	w := newReplicatorFor(t, store, map[string]backend.ObjectBackend{"b1": cbb}, &fleetOpts{Order: []string{"b1"}})
	if w.IsBackendHealthy("b1") {
		t.Error("tripped CB backend should report unhealthy")
	}
}
