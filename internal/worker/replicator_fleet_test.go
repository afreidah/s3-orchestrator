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
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// placeCopyTracker accumulates PlaceCopy calls and returns the configured
// size, or the configured error in its place.
type placeCopyTracker struct {
	mu    sync.Mutex
	calls []core.Placement
	size  int64
	err   error
}

// stubPlaceCopy returns a DoAndReturn capturing into pt. A refusal queues the
// replica's bytes under the placement's orphan reason, as the store does; any
// other error queues nothing.
func stubPlaceCopy(pt *placeCopyTracker) func(context.Context, *core.Placement) (int64, []core.CleanupItem, error) {
	return func(_ context.Context, p *core.Placement) (int64, []core.CleanupItem, error) {
		pt.mu.Lock()
		defer pt.mu.Unlock()
		pt.calls = append(pt.calls, *p)
		switch {
		case core.PlacementDeclined(pt.err):
			return 0, []core.CleanupItem{{
				ID: replicaCleanupID, BackendName: p.Intent.BackendName, ObjectKey: p.Intent.ObjectKey,
				StorageKey: p.Intent.StorageKey, Reason: p.Reasons.Orphan, SizeBytes: p.Intent.SizeBytes,
			}}, pt.err
		case pt.err != nil:
			return 0, nil, pt.err
		}
		return pt.size, nil, nil
	}
}

// replicaCleanupID is the id of the cleanup row a refused replica queues.
const replicaCleanupID = 9

// replicatorCompleteTracker captures the rows CompleteCleanupItems retired
// during a replicator test.
type replicatorCompleteTracker struct {
	mu  sync.Mutex
	ids []int64
}

// stubReplicatorComplete returns a DoAndReturn for CompleteCleanupItems that
// retires every row it is handed.
func stubReplicatorComplete(ct *replicatorCompleteTracker) func(context.Context, []core.CleanupItem) ([]int64, error) {
	return func(_ context.Context, items []core.CleanupItem) ([]int64, error) {
		ct.mu.Lock()
		defer ct.mu.Unlock()
		ct.ids = append(ct.ids, cleanupIDs(items)...)
		return cleanupIDs(items), nil
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
// eligible behaviour, and a PlaceCopy captured into pt.
func replicateSuccessStubs(store *storetest.MockMetadataStore, locations []core.ObjectLocation, pt *placeCopyTracker) {
	store.EXPECT().GetUnderReplicatedObjects(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(locations, nil).AnyTimes()
	store.EXPECT().GetUnderReplicatedObjectsExcluding(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(locations, nil).AnyTimes()
	store.EXPECT().PlaceCopy(gomock.Any(), gomock.Any()).
		DoAndReturn(stubPlaceCopy(pt)).AnyTimes()
}

// TestReplicate_Success drives the happy path.
func TestReplicate_Success(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b2 := backendtest.NewInMemory()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	rt := &placeCopyTracker{}
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
	if !b2.HasCopyOf("key1") {
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
// no copy: the claim refuses the target even though ranking proposed the
// backend, so nothing is copied to it.
func TestReplicate_FullTargetRecordsNoCopy(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetUnderReplicatedObjects(gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 50}}, nil).AnyTimes()
	// Every target declines the claim, which is how a full backend reports itself.
	store.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Any(), core.AcceptForWrite).Return(false, nil).AnyTimes()
	storetest.Permissive(store)

	src := backendtest.NewInMemory()
	_, _ = src.PutObject(context.Background(), "key1", bytes.NewReader([]byte("0123456789")), 10, "text/plain", nil)
	dst := backendtest.NewInMemory()
	w := newReplicatorFor(t, store, map[string]backend.ObjectBackend{"b1": src, "b2": dst},
		&fleetOpts{Order: []string{"b1", "b2"}})

	out, err := w.Replicate(context.Background(), config.ReplicationConfig{Factor: 2, BatchSize: 10}, nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if out.CopiesCreated != 0 {
		t.Errorf("outcome = %+v, want no copies and no errors when every target declines the claim", out)
	}
	if len(dst.Objects) != 0 {
		t.Errorf("target holds %d objects, want nothing copied to a declined target", len(dst.Objects))
	}
}

// TestReplicate_RefusedReplicaIsSettled verifies a replica whose placement is
// refused is placed under the replication reasons and has the cleanup row the
// refusal queued settled, so its bytes leave the target.
func TestReplicate_RefusedReplicaIsSettled(t *testing.T) {
	t.Parallel()
	src := backendtest.NewInMemory()
	_, _ = src.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	dst := backendtest.NewInMemory()

	pt := &placeCopyTracker{err: core.ErrTargetRefused}
	ct := &replicatorCompleteTracker{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	replicateSuccessStubs(store,
		[]core.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}}, pt)
	store.EXPECT().CompleteCleanupItems(gomock.Any(), gomock.Any()).
		DoAndReturn(stubReplicatorComplete(ct)).AnyTimes()
	storetest.Permissive(store)

	w := newReplicatorFor(t, store, map[string]backend.ObjectBackend{"b1": src, "b2": dst},
		&fleetOpts{Order: []string{"b1", "b2"}})
	if _, err := w.Replicate(context.Background(), config.ReplicationConfig{Factor: 2, BatchSize: 10}, nil); err != nil {
		t.Fatalf("Replicate: %v", err)
	}

	if len(pt.calls) != 1 || pt.calls[0].Reasons != writepath.ReplicationReasons {
		t.Fatalf("placements = %+v, want one under the replication reasons", pt.calls)
	}
	if dst.Has(pt.calls[0].Intent.StorageKey) {
		t.Error("the refused replica is still on the target")
	}
	if len(ct.ids) != 1 || ct.ids[0] != replicaCleanupID {
		t.Errorf("retired cleanup rows %v, want the refused replica's row", ct.ids)
	}
}

// TestReplicate_CopyFailureDiscardsTheIntent verifies a replica whose copy
// fails has its intent discarded under the replication orphan reason.
func TestReplicate_CopyFailureDiscardsTheIntent(t *testing.T) {
	t.Parallel()
	src := backendtest.NewInMemory()
	_, _ = src.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	dst := backendtest.NewInMemory()
	dst.PutErr = errors.New("put failed")

	var reasons []string
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	replicateSuccessStubs(store,
		[]core.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}}, &placeCopyTracker{})
	store.EXPECT().DiscardPending(gomock.Any(), intentOn("b2", "key1"), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ *core.PendingObject, reason string) ([]core.CleanupItem, error) {
			reasons = append(reasons, reason)
			return nil, nil
		})
	storetest.Permissive(store)

	w := newReplicatorFor(t, store, map[string]backend.ObjectBackend{"b1": src, "b2": dst},
		&fleetOpts{Order: []string{"b1", "b2"}})
	out, err := w.Replicate(context.Background(), config.ReplicationConfig{Factor: 2, BatchSize: 10}, nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if out.CopiesCreated != 0 {
		t.Errorf("created %d copies, want 0 when the copy fails", out.CopiesCreated)
	}
	if len(reasons) != 1 || reasons[0] != writepath.ReplicationReasons.Orphan {
		t.Errorf("discard reasons = %v, want one %s", reasons, writepath.ReplicationReasons.Orphan)
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
	source, err := w.CopyToReplica(context.Background(), "key1", "key1"+"!replica", copies, "b3")
	if err != nil {
		t.Fatalf("copyToReplica should failover: %v", err)
	}
	if source.BackendName != "b2" {
		t.Errorf("expected source=b2 (failover), got %q", source.BackendName)
	}
	if !b3.HasCopyOf("key1") {
		t.Error("expected key1 on target b3")
	}
}

// TestCopyToReplica_DoesNotMutateInputSlice asserts the caller's copies slice
// keeps its order when health sorting would reorder it (the first entry names
// a backend missing from the fleet, so it scores as unhealthy).
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

	if _, err := w.CopyToReplica(context.Background(), "key1", "key1"+"!replica", copies, "b3"); err != nil {
		t.Fatalf("CopyToReplica: %v", err)
	}

	after := []string{copies[0].BackendName, copies[1].BackendName}
	if before[0] != after[0] || before[1] != after[1] {
		t.Errorf("input slice mutated: before=%v after=%v", before, after)
	}
}

// TestReplicate_PlaceCopyFails_LeavesReplicaForReaper asserts a placement that
// fails outright queues nothing, so the replica stays under its intent for the
// pending reaper to resolve.
func TestReplicate_PlaceCopyFails_LeavesReplicaForReaper(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b2 := backendtest.NewInMemory()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	rt := &placeCopyTracker{err: errors.New("db error")}
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
	if !b2.HasCopyOf("key1") {
		t.Error("the unplaced replica was deleted; its intent leaves it to the reaper")
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
	if _, err := w.CopyToReplica(context.Background(), "key1", "key1"+"!replica", copies, "nonexistent"); err == nil {
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
	if _, err := w.CopyToReplica(context.Background(), "key1", "key1"+"!replica", copies, "b2"); err == nil {
		t.Fatal("expected error when target PutObject fails")
	}
}

// TestReplicateObject_NoTargetAvailable asserts a replicate run with no
// eligible target produces zero successes.
func TestReplicateObject_NoTargetAvailable(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	rt := &placeCopyTracker{}
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
// branch when the placement reports the source copy changed.
func TestReplicate_SourceGoneDuringReplication(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b2 := backendtest.NewInMemory()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	rt := &placeCopyTracker{err: core.ErrCopyChanged}
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
	if b2.HasCopyOf("key1") {
		t.Error("orphan should have been cleaned up from b2")
	}
}

// newTrippedCBBackend wraps a mock backend in a CircuitBreakerBackend
// and immediately trips the circuit with a failure that reaches no backend.
func newTrippedCBBackend(b *backendtest.InMemory, name string) *backend.CircuitBreakerBackend {
	cbb := backend.NewCircuitBreakerBackend(b, backend.CircuitBreakerConfig{Name: name, Threshold: 1, Timeout: time.Hour})
	b.HeadErr = errors.New("forced failure")
	_, _ = cbb.HeadObject(context.Background(), "tripwire")
	b.HeadErr = nil
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

	rt := &placeCopyTracker{}
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
	if b2.HasCopyOf("key1") {
		t.Error("unhealthy b2 should not have received a replica")
	}
	if !b3.HasCopyOf("key1") {
		t.Error("expected key1 on healthy b3")
	}
}

// TestReplicate_HealthAware_PrefersHealthySource asserts the source
// preference picks a copy it can read when one source is refusing reads.
func TestReplicate_HealthAware_PrefersHealthySource(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b2 := backendtest.NewInMemory()
	b3 := backendtest.NewInMemory()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	_, _ = b2.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	cbb1 := newTrippedCBBackend(b1, "b1")

	rt := &placeCopyTracker{}
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
		t.Fatalf("expected 1 PlaceCopy call, got %d", len(rt.calls))
	}
	if got := rt.calls[0]; got.Source.BackendName != "b2" || got.Intent.BackendName != "b3" || got.RemoveSource {
		t.Errorf("placement = source %q target %q remove %v, want b2 (healthy) -> b3 keeping the source",
			got.Source.BackendName, got.Intent.BackendName, got.RemoveSource)
	}
}

// TestReplicate_ChargesTheBytesTheCopyMoved verifies the usage a replica costs
// is what the stream copy moved between the source it read and the target,
// not a row's recorded size or the first listed copy's.
func TestReplicate_ChargesTheBytesTheCopyMoved(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b2 := backendtest.NewInMemory()
	b3 := backendtest.NewInMemory()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	_, _ = b2.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	cbb1 := newTrippedCBBackend(b1, "b1")

	rt := &placeCopyTracker{size: 200}
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

	if got := loadUsage(fleet.Usage().Backend(), "b2").EgressBytes; got != 4 {
		t.Errorf("source egress = %d, want the 4 bytes read from b2", got)
	}
	if got := loadUsage(fleet.Usage().Backend(), "b3").IngressBytes; got != 4 {
		t.Errorf("target ingress = %d, want the 4 bytes written to b3", got)
	}
	if got := loadUsage(fleet.Usage().Backend(), "b1").EgressBytes; got != 0 {
		t.Errorf("tripped b1 egress = %d, want 0", got)
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

// TestServesReads_NoCB asserts a non-CB-wrapped backend serves reads.
func TestServesReads_NoCB(t *testing.T) {
	t.Parallel()
	w := newReplicatorFor(t, newPermissiveStore(t), map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, &fleetOpts{})

	if !w.servesReads("b1") {
		t.Error("backend without CB wrapper should serve reads")
	}
}

// TestServesReads_UnknownBackend asserts an unknown backend serves nothing.
func TestServesReads_UnknownBackend(t *testing.T) {
	t.Parallel()
	w := newReplicatorFor(t, newPermissiveStore(t), map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, &fleetOpts{})

	if w.servesReads("nonexistent") {
		t.Error("unknown backend should not serve reads")
	}
}

// TestServesReads_CBClosed asserts a closed-circuit backend serves reads.
func TestServesReads_CBClosed(t *testing.T) {
	t.Parallel()
	cbb := backend.NewCircuitBreakerBackend(backendtest.NewInMemory(), backend.CircuitBreakerConfig{Name: "b1", Threshold: 3, Timeout: time.Minute})
	store := newPermissiveStore(t)
	w := newReplicatorFor(t, store, map[string]backend.ObjectBackend{"b1": cbb}, &fleetOpts{Order: []string{"b1"}})
	if !w.servesReads("b1") {
		t.Error("closed CB backend should serve reads")
	}
}

// TestServesReads_CBTripped asserts a backend with its breaker tripped does
// not serve reads.
func TestServesReads_CBTripped(t *testing.T) {
	t.Parallel()
	cbb := newTrippedCBBackend(backendtest.NewInMemory(), "b1")
	store := newPermissiveStore(t)
	w := newReplicatorFor(t, store, map[string]backend.ObjectBackend{"b1": cbb}, &fleetOpts{Order: []string{"b1"}})
	if w.servesReads("b1") {
		t.Error("tripped CB backend should not serve reads")
	}
}

// TestServesReads_OnlyWritesRefused asserts a backend refusing uploads alone
// still serves reads, and its copies keep counting toward the replica count.
func TestServesReads_OnlyWritesRefused(t *testing.T) {
	t.Parallel()
	inner := backendtest.NewInMemory()
	inner.PutErr = &backendtest.StatusError{Code: 403}
	cbb := backend.NewCircuitBreakerBackend(inner, backend.CircuitBreakerConfig{Name: "b1", Threshold: 1, Timeout: time.Minute})
	_, _ = cbb.PutObject(context.Background(), "k", bytes.NewReader([]byte("x")), 1, "", nil)
	w := newReplicatorFor(t, newPermissiveStore(t), map[string]backend.ObjectBackend{"b1": cbb}, &fleetOpts{Order: []string{"b1"}})

	if !w.servesReads("b1") {
		t.Error("backend refusing only uploads should serve reads")
	}
	if names := w.UnhealthyBackends(time.Nanosecond); len(names) != 0 {
		t.Errorf("UnhealthyBackends = %v, want none while reads are served", names)
	}
}
