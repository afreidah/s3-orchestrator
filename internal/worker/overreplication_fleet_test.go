// -------------------------------------------------------------------------------
// Over-Replication Cleaner Tests - Fleet-Level Behaviour
//
// Author: Alex Freidah
//
// Covers the cleaner against a live fleet - real backends, a real write
// coordinator, and the usage/admission policy the runtime enforces - for the
// paths whose behaviour depends on that machinery. The narrow-mock unit tests
// for scoring and config live in overreplication_test.go.

package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// removeExcessRecord captures one RemoveExcessCopy call.
type removeExcessRecord struct {
	key, backend string
	factor       int
}

// removeExcessTracker accumulates RemoveExcessCopy calls and returns the
// configured outcome. When err is nil the stub reports removed=true so
// the cleaner counts the call as a successful removal; tests asserting
// the no-op race outcome set removed=false explicitly.
type removeExcessTracker struct {
	mu      sync.Mutex
	calls   []removeExcessRecord
	err     error
	removed *bool
}

// stubRemoveExcessCopy returns a DoAndReturn that captures into rt.
func stubRemoveExcessCopy(rt *removeExcessTracker) func(context.Context, string, string, int) (bool, error) {
	return func(_ context.Context, key, backend string, factor int) (bool, error) {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		rt.calls = append(rt.calls, removeExcessRecord{key: key, backend: backend, factor: factor})
		if rt.err != nil {
			return false, rt.err
		}
		if rt.removed != nil {
			return *rt.removed, nil
		}
		return true, nil
	}
}

// TestScoreCopy_CircuitBrokenBackend pins the score for a tripped breaker.
func TestScoreCopy_CircuitBrokenBackend(t *testing.T) {
	t.Parallel()
	store := newPermissiveStore(t)
	mock := backendtest.NewInMemory()
	mock.PutErr = errors.New("backend down")
	cbBackend := backend.NewCircuitBreakerBackend(mock, backend.CircuitBreakerConfig{Name: "b1", Threshold: 1, Timeout: time.Minute})
	_, _ = cbBackend.PutObject(context.Background(), "k", nil, 0, "", nil)

	w := newOverRepFor(t, store, map[string]backend.ObjectBackend{"b1": cbBackend}, &fleetOpts{Order: []string{"b1"}})

	loc := core.ObjectLocation{BackendName: "b1", SizeBytes: 100}
	score := w.ScoreCopy(&loc, nil)

	if score != 1 {
		t.Errorf("expected score 1 for circuit-broken backend, got %f", score)
	}
}

// TestScoreCopy_NoQuotaData asserts the no-data fallback score.
func TestScoreCopy_NoQuotaData(t *testing.T) {
	t.Parallel()
	store := newPermissiveStore(t)
	w := newOverRepFor(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, &fleetOpts{})

	loc := core.ObjectLocation{BackendName: "b1", SizeBytes: 100}
	score := w.ScoreCopy(&loc, nil)

	if score != 2.5 {
		t.Errorf("expected score 2.5, got %f", score)
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

	w := newOverRepFor(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, &fleetOpts{})

	if _, err := w.Clean(context.Background(), config.ReplicationConfig{
		Factor:    2,
		BatchSize: 10,
	}, nil); err == nil {
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

	w := newOverRepFor(t, store, map[string]backend.ObjectBackend{
		"b1": backendtest.NewInMemory(),
		"b2": backendtest.NewInMemory(),
		"b3": backendtest.NewInMemory(),
	}, &fleetOpts{})

	sum, err := w.Clean(context.Background(), config.ReplicationConfig{
		Factor:      2,
		BatchSize:   10,
		Concurrency: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if sum.CopiesRemoved != 1 {
		t.Errorf("expected 1 removed, got %d", sum.CopiesRemoved)
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

	w := newOverRepFor(t, store, map[string]backend.ObjectBackend{
		"b1": backendtest.NewInMemory(),
		"b2": backendtest.NewInMemory(),
		"b3": backendtest.NewInMemory(),
	}, &fleetOpts{})

	sum, err := w.Clean(context.Background(), config.ReplicationConfig{
		Factor:      2,
		BatchSize:   10,
		Concurrency: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if sum.CopiesRemoved != 1 {
		t.Errorf("expected 1 removed, got %d", sum.CopiesRemoved)
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

	w := newOverRepFor(t, store, map[string]backend.ObjectBackend{
		"b1": backendtest.NewInMemory(),
		"b2": backendtest.NewInMemory(),
		"b3": backendtest.NewInMemory(),
	}, &fleetOpts{})

	sum, err := w.Clean(context.Background(), config.ReplicationConfig{
		Factor:      2,
		BatchSize:   10,
		Concurrency: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Clean should not return error for per-object failures: %v", err)
	}
	if sum.CopiesRemoved != 0 {
		t.Errorf("expected 0 removed (all failed), got %d", sum.CopiesRemoved)
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

	w := newOverRepFor(t, store, map[string]backend.ObjectBackend{
		"b1": backendtest.NewInMemory(),
		"b2": backendtest.NewInMemory(),
		"b3": backendtest.NewInMemory(),
	}, &fleetOpts{})

	sum, err := w.Clean(context.Background(), config.ReplicationConfig{
		Factor:      2,
		BatchSize:   10,
		Concurrency: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if sum.CopiesRemoved != 2 {
		t.Errorf("expected 2 removed, got %d", sum.CopiesRemoved)
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

	w := newOverRepFor(t, store, map[string]backend.ObjectBackend{
		"b1": backendtest.NewInMemory(),
	}, &fleetOpts{})

	sum, err := w.Clean(context.Background(), config.ReplicationConfig{
		Factor:      2,
		BatchSize:   10,
		Concurrency: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if sum.CopiesRemoved != 0 {
		t.Errorf("expected 0 removed (backend not found), got %d", sum.CopiesRemoved)
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

	w := newOverRepFor(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, &fleetOpts{})

	if _, err := w.CountPending(context.Background(), 2); err == nil {
		t.Fatal("expected error from CountPending")
	}
}

// TestClean_AdmissionBlocked asserts a saturated admission semaphore +
// cancelled ctx halts the
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

	w := newOverRepFor(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory(), "b2": backendtest.NewInMemory(), "b3": backendtest.NewInMemory()}, &fleetOpts{Order: []string{"b1", "b2", "b3"}})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sum, err := w.Clean(ctx, config.ReplicationConfig{
		Factor:      2,
		BatchSize:   10,
		Concurrency: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if sum.CopiesRemoved != 0 {
		t.Errorf("expected 0 removed when admission blocked, got %d", sum.CopiesRemoved)
	}
}
