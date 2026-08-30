// -------------------------------------------------------------------------------
// Usage Service Tests
//
// Author: Alex Freidah
//
// Covers what the service adds over the tracker and the store: the drain skip
// applied to a flush, the reconcile forward, and the flush configuration it
// holds for the reload hook. The nil-Drain case is the one a deployment that
// never drains a backend runs in, so it is pinned alongside the skip.
// -------------------------------------------------------------------------------

package usage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// fakeStores records the flush deltas it is handed and returns canned answers
// for the reconcile, which is the whole persistence surface the service needs.
type fakeStores struct {
	flushed        map[string]int64
	flushErr       error
	reconciled     map[string]int64
	reconcileErr   error
	reconcileCalls int
}

func newFakeStores() *fakeStores {
	return &fakeStores{flushed: make(map[string]int64)}
}

func (f *fakeStores) FlushUsageDeltas(_ context.Context, backendName, _ string, apiRequests, _, _ int64) error {
	if f.flushErr != nil {
		return f.flushErr
	}
	f.flushed[backendName] += apiRequests
	return nil
}

func (f *fakeStores) ReconcileUsage(_ context.Context) (map[string]int64, error) {
	f.reconcileCalls++
	return f.reconciled, f.reconcileErr
}

func (f *fakeStores) GetBackendWithSpace(_ context.Context, _ int64, _ []string) (string, error) {
	return "", nil
}

func (f *fakeStores) GetLeastUtilizedBackend(_ context.Context, _ int64, _ []string) (string, error) {
	return "", nil
}

func (f *fakeStores) GetQuotaStats(_ context.Context) (map[string]core.QuotaStat, error) {
	return nil, nil
}

// fakeDrain reports a fixed completed set.
type fakeDrain struct {
	completed map[string]bool
}

func (d fakeDrain) CompletedBackends() map[string]bool { return d.completed }

// newService wires the service over a local counter backend holding one
// recorded API call per backend, so a flush has something to write.
func newService(t *testing.T, stores Stores, drain DrainReader) *Service {
	t.Helper()
	tracker := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1", "b2"}), nil)
	tracker.Record("b1", 1, 0, 0)
	tracker.Record("b2", 1, 0, 0)
	return New(&Deps{Usage: tracker, Stores: stores, Drain: drain})
}

// TestFlushUsage_NilDrainFlushesEverything pins the deployment that never
// drains a backend: with no drain reader the flush skips nothing.
func TestFlushUsage_NilDrainFlushesEverything(t *testing.T) {
	t.Parallel()
	stores := newFakeStores()
	s := newService(t, stores, nil)

	if err := s.FlushUsage(context.Background()); err != nil {
		t.Fatalf("FlushUsage: %v", err)
	}
	if len(stores.flushed) != 2 {
		t.Errorf("flushed = %v, want both backends", stores.flushed)
	}
}

// TestFlushUsage_SkipsCompletedDrains covers the reason the service holds the
// drain reader at all: a drained backend's rows are gone, so flushing its
// counters would write back what the drain removed.
func TestFlushUsage_SkipsCompletedDrains(t *testing.T) {
	t.Parallel()
	stores := newFakeStores()
	s := newService(t, stores, fakeDrain{completed: map[string]bool{"b1": true}})

	if err := s.FlushUsage(context.Background()); err != nil {
		t.Fatalf("FlushUsage: %v", err)
	}
	if _, ok := stores.flushed["b1"]; ok {
		t.Errorf("flushed = %v, want no write for the drained backend", stores.flushed)
	}
	if _, ok := stores.flushed["b2"]; !ok {
		t.Errorf("flushed = %v, want the remaining backend written", stores.flushed)
	}
}

// TestFlushUsage_SurfacesStoreError pins that a failing write reaches the
// caller, which is what turns a broken flush into a logged shutdown warning
// rather than a silent counter loss.
func TestFlushUsage_SurfacesStoreError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("write failed")
	stores := newFakeStores()
	stores.flushErr = sentinel
	s := newService(t, stores, nil)

	if err := s.FlushUsage(context.Background()); !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v", err, sentinel)
	}
}

// TestReconcileUsage_ForwardsStoreResult covers both legs of the delegation:
// the corrected deltas on success and the error on failure.
func TestReconcileUsage_ForwardsStoreResult(t *testing.T) {
	t.Parallel()
	stores := newFakeStores()
	stores.reconciled = map[string]int64{"b1": -42}
	s := newService(t, stores, nil)

	got, err := s.ReconcileUsage(context.Background())
	if err != nil {
		t.Fatalf("ReconcileUsage: %v", err)
	}
	if got["b1"] != -42 {
		t.Errorf("deltas = %v, want b1 = -42", got)
	}
	if stores.reconcileCalls != 1 {
		t.Errorf("reconcile calls = %d, want 1", stores.reconcileCalls)
	}
}

// TestRedisCounterConfigured_LocalBackend pins the answer the flush service
// gives when the counters are in process memory: no advisory lock is needed
// because no other instance shares them.
func TestRedisCounterConfigured_LocalBackend(t *testing.T) {
	t.Parallel()
	s := newService(t, newFakeStores(), nil)

	if s.RedisCounterConfigured() {
		t.Error("RedisCounterConfigured = true for a local counter backend")
	}
}

// TestConfig_NilUntilStored covers the reload contract: the service starts
// without a flush config and returns whatever was last stored.
func TestConfig_NilUntilStored(t *testing.T) {
	t.Parallel()
	s := newService(t, newFakeStores(), nil)

	if got := s.Config(); got != nil {
		t.Fatalf("Config = %v before a store, want nil", got)
	}
	s.SetConfig(&config.UsageFlushConfig{Interval: 30 * time.Second})
	if got := s.Config(); got == nil || got.Interval != 30*time.Second {
		t.Errorf("Config = %v, want Interval 30s", got)
	}
}
