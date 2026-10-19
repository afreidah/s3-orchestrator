// -------------------------------------------------------------------------------
// Usage Service Tests
//
// Author: Alex Freidah
//
// Covers what the service adds over the tracker and the store: the flush and
// reconcile forwards, the quota baseline refresh, the shared-counter check, and
// the flush configuration it holds for the reload hook.
// -------------------------------------------------------------------------------

package usage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// fakeStores records the usage batches it is handed and returns canned answers
// for the reconcile and the quota read, which is the whole persistence surface
// the service needs.
type fakeStores struct {
	applied        map[string]int64
	applyErr       error
	reconciled     map[string]int64
	reconcileErr   error
	reconcileCalls int
	baselines      []core.BackendQuotaUsage
	baselineCalls  int
	baselineErr    error
}

// newFakeStores returns a store that applies every batch and holds no quota
// rows.
func newFakeStores() *fakeStores {
	return &fakeStores{applied: make(map[string]int64)}
}

// sharedCounters is a local counter backend that also answers TakeShared, so
// the service sees a shared backend without a Redis behind it.
type sharedCounters struct {
	*counter.LocalCounterBackend
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// ApplyUsageBatch records each batch's API requests under its backend, or
// refuses it with the seeded error.
func (f *fakeStores) ApplyUsageBatch(_ context.Context, b *core.UsageBatch) error {
	if f.applyErr != nil {
		return f.applyErr
	}
	f.applied[b.Backend] += b.APIRequests
	return nil
}

// ReconcileUsage counts the call and returns the seeded result.
func (f *fakeStores) ReconcileUsage(_ context.Context) (map[string]int64, error) {
	f.reconcileCalls++
	return f.reconciled, f.reconcileErr
}

// GetQuotaStats answers with no stats; the service never reads them.
func (f *fakeStores) GetQuotaStats(_ context.Context) (map[string]core.QuotaStat, error) {
	return nil, nil
}

// ListBackendQuotaUsage answers the baseline refresh with whatever the test
// seeded.
func (f *fakeStores) ListBackendQuotaUsage(_ context.Context) ([]core.BackendQuotaUsage, error) {
	f.baselineCalls++
	return f.baselines, f.baselineErr
}

// TakeShared reports no staged batches.
func (sharedCounters) TakeShared(context.Context) []core.UsageBatch {
	return nil
}

// newService wires the service over a local counter backend holding one
// recorded API call per backend, so a flush has something to write.
func newService(t *testing.T, stores Stores) *Service {
	t.Helper()
	s, _ := newServiceWithQuota(t, stores)
	return s
}

// newServiceWithQuota is newService that also returns the quota tracker the
// service refreshes.
func newServiceWithQuota(t *testing.T, stores Stores) (*Service, *counter.QuotaTracker) {
	t.Helper()
	tracker := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1", "b2"}), nil)
	tracker.Record("b1", s3op.GetObject, 0, 0)
	tracker.Record("b2", s3op.GetObject, 0, 0)
	quota := counter.NewQuotaTracker()
	return New(&Deps{Usage: tracker, Quota: quota, Stores: stores}), quota
}

// TestFlushUsage_WritesEveryBackend verifies a flush writes the counters of
// every backend that recorded usage.
func TestFlushUsage_WritesEveryBackend(t *testing.T) {
	t.Parallel()
	stores := newFakeStores()
	s := newService(t, stores)

	if err := s.FlushUsage(context.Background()); err != nil {
		t.Fatalf("FlushUsage: %v", err)
	}
	if stores.applied["b1"] != 1 || stores.applied["b2"] != 1 {
		t.Errorf("applied = %v, want one request on each backend", stores.applied)
	}
}

// TestFlushUsage_SurfacesStoreErrorAndKeepsCounters pins that a failing write
// reaches the caller and that the counters it could not write are flushed by
// the next tick rather than lost.
func TestFlushUsage_SurfacesStoreErrorAndKeepsCounters(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("write failed")
	stores := newFakeStores()
	stores.applyErr = sentinel
	s := newService(t, stores)

	if err := s.FlushUsage(context.Background()); !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v", err, sentinel)
	}

	stores.applyErr = nil
	if err := s.FlushUsage(context.Background()); err != nil {
		t.Fatalf("retry FlushUsage: %v", err)
	}
	if stores.applied["b1"] != 1 || stores.applied["b2"] != 1 {
		t.Errorf("applied after retry = %v, want the kept request on each backend", stores.applied)
	}
}

// TestReconcileUsage_ForwardsStoreResult covers the corrected deltas on
// success and the quota baselines being reloaded after them.
func TestReconcileUsage_ForwardsStoreResult(t *testing.T) {
	t.Parallel()
	stores := newFakeStores()
	stores.reconciled = map[string]int64{"b1": -42}
	s := newService(t, stores)

	got, err := s.ReconcileUsage(context.Background())
	if err != nil {
		t.Fatalf("ReconcileUsage: %v", err)
	}
	if got["b1"] != -42 {
		t.Errorf("deltas = %v, want b1 = -42", got)
	}
	if stores.reconcileCalls != 1 || stores.baselineCalls != 1 {
		t.Errorf("reconcile calls = %d, baseline reads = %d; want 1 and 1", stores.reconcileCalls, stores.baselineCalls)
	}
}

// TestReconcileUsage_Errors covers both failure legs: a reconcile error stops
// before the refresh, and a refresh error is returned with the deltas.
func TestReconcileUsage_Errors(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("db down")

	stores := newFakeStores()
	stores.reconcileErr = sentinel
	if _, err := newService(t, stores).ReconcileUsage(context.Background()); !errors.Is(err, sentinel) {
		t.Errorf("reconcile failure: err = %v, want %v", err, sentinel)
	}
	if stores.baselineCalls != 0 {
		t.Errorf("baseline reads = %d after a failed reconcile, want 0", stores.baselineCalls)
	}

	stores = newFakeStores()
	stores.reconciled = map[string]int64{"b1": 7}
	stores.baselineErr = sentinel
	got, err := newService(t, stores).ReconcileUsage(context.Background())
	if !errors.Is(err, sentinel) || got["b1"] != 7 {
		t.Errorf("refresh failure: got %v, %v; want the deltas and %v", got, err, sentinel)
	}
}

// TestRefreshQuotaBaselines_LoadsTheRows verifies each stored row becomes the
// tracker's baseline for its backend.
func TestRefreshQuotaBaselines_LoadsTheRows(t *testing.T) {
	t.Parallel()
	stores := newFakeStores()
	stores.baselines = []core.BackendQuotaUsage{{BackendName: "b1", BytesLimit: 1000, BytesUsed: 400}}
	s, quota := newServiceWithQuota(t, stores)

	if err := s.RefreshQuotaBaselines(context.Background()); err != nil {
		t.Fatalf("RefreshQuotaBaselines: %v", err)
	}
	if got := quota.Available("b1"); got != 600 {
		t.Errorf("b1 available = %d, want 600", got)
	}
	if _, ok := quota.Baseline("b2"); ok {
		t.Error("b2 has a baseline the store never returned")
	}
}

// TestRefreshQuotaBaselines_StoreError verifies a failed read is returned and
// leaves the tracker as it was.
func TestRefreshQuotaBaselines_StoreError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("db down")
	stores := newFakeStores()
	stores.baselineErr = sentinel
	s, quota := newServiceWithQuota(t, stores)

	if err := s.RefreshQuotaBaselines(context.Background()); !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v", err, sentinel)
	}
	if _, ok := quota.Baseline("b1"); ok {
		t.Error("a failed read seeded a baseline")
	}
}

// TestRedisCounterConfigured_LocalBackend pins the answer for counters held in
// process memory, which no other instance shares.
func TestRedisCounterConfigured_LocalBackend(t *testing.T) {
	t.Parallel()
	s := newService(t, newFakeStores())

	if s.RedisCounterConfigured() {
		t.Error("RedisCounterConfigured = true for a local counter backend")
	}
}

// TestRedisCounterConfigured_SharedBackend pins the answer for any backend
// whose counters every instance charges.
func TestRedisCounterConfigured_SharedBackend(t *testing.T) {
	t.Parallel()
	shared := sharedCounters{counter.NewLocalCounterBackend([]string{"b1"})}
	s := New(&Deps{
		Usage:  counter.NewUsageTracker(shared, nil),
		Quota:  counter.NewQuotaTracker(),
		Stores: newFakeStores(),
	})

	if !s.RedisCounterConfigured() {
		t.Error("RedisCounterConfigured = false for a shared counter backend")
	}
}

// TestConfig_NilUntilStored covers the reload contract: the service starts
// without a flush config and returns whatever was last stored.
func TestConfig_NilUntilStored(t *testing.T) {
	t.Parallel()
	s := newService(t, newFakeStores())

	if got := s.Config(); got != nil {
		t.Fatalf("Config = %v before a store, want nil", got)
	}
	s.SetConfig(&config.UsageFlushConfig{Interval: 30 * time.Second})
	if got := s.Config(); got == nil || got.Interval != 30*time.Second {
		t.Errorf("Config = %v, want Interval 30s", got)
	}
}
