// -------------------------------------------------------------------------------
// Manager Tests - Upload ID Generation and String Utilities
//
// Author: Alex Freidah
//
// Unit tests for the backend manager's utility functions including upload ID
// generation uniqueness and length validation.
// -------------------------------------------------------------------------------

package proxy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// -------------------------------------------------------------------------
// CountActiveMultipartUploads delegation
// -------------------------------------------------------------------------

// newMockMultipartCountStore returns a fresh MockMetadataStore with a
// strict CountActiveMultipartUploads expectation registered FIRST, then
// Permissive() applied as the catch-all. Mirrors the ordering documented
// on storetest.Permissive: specific stubs must precede the catch-all so
// Times()-bounded expectations actually fire.
func newMockMultipartCountStore(t *testing.T, wantPrefix string, retCount int64, retErr error) *storetest.MockMetadataStore {
	t.Helper()
	m := storetest.NewMockMetadataStore(gomock.NewController(t))
	m.EXPECT().
		CountActiveMultipartUploads(gomock.Any(), wantPrefix).
		Return(retCount, retErr).
		Times(1)
	storetest.Permissive(m)
	return m
}

// TestCountActiveMultipartUploads_DelegatesToStore pins that the typed
// accessor on BackendManager forwards the prefix and return values from
// the underlying MultipartStore unchanged.
func TestCountActiveMultipartUploads_DelegatesToStore(t *testing.T) {
	t.Parallel()
	store := newMockMultipartCountStore(t, "bucket/", 7, nil)
	mgr := newUsageManager(t, []string{"b1"}, store)
	defer mgr.Close()

	got, err := mgr.CountActiveMultipartUploads(context.Background(), "bucket/")
	if err != nil {
		t.Fatalf("CountActiveMultipartUploads: %v", err)
	}
	if got != 7 {
		t.Errorf("got %d, want 7", got)
	}
}

// TestCountActiveMultipartUploads_PropagatesError pins that store errors
// surface to the caller without rewrapping.
func TestCountActiveMultipartUploads_PropagatesError(t *testing.T) {
	t.Parallel()
	storeErr := errors.New("boom")
	store := newMockMultipartCountStore(t, "x/", 0, storeErr)
	mgr := newUsageManager(t, []string{"b1"}, store)
	defer mgr.Close()

	if _, err := mgr.CountActiveMultipartUploads(context.Background(), "x/"); !errors.Is(err, storeErr) {
		t.Errorf("err = %v, want %v", err, storeErr)
	}
}

// -------------------------------------------------------------------------
// Close (idempotent)
// -------------------------------------------------------------------------

// TestClose_Idempotent verifies the close idempotent path by exercising mgr.Close.
func TestClose_Idempotent(t *testing.T) {
	t.Parallel()
	mgr := newUsageManager(t, []string{"b1"}, newPermissiveMock(t))

	// Calling Close twice should not panic
	mgr.Close()
	mgr.Close()
}

// -------------------------------------------------------------------------
// UpdateUsageLimits
// -------------------------------------------------------------------------

// TestUpdateUsageLimits_SwapsLimits verifies the update usage limits swaps limits path by exercising mgr.UpdateUsageLimits.
func TestUpdateUsageLimits_SwapsLimits(t *testing.T) {
	t.Parallel()
	limits := map[string]core.UsageLimits{
		"b1": {APIRequestLimit: 100},
	}
	mgr := newUsageManagerWithLimits(t, []string{"b1"}, newPermissiveMock(t), limits)

	// Initially within limits
	if !mgr.Usage().WithinLimits("b1", 50, 0, 0) {
		t.Fatal("should be within initial limits")
	}

	// Update to a much lower limit
	mgr.UpdateUsageLimits(map[string]core.UsageLimits{
		"b1": {APIRequestLimit: 10},
	})

	// Now 50 should exceed the new limit
	if mgr.Usage().WithinLimits("b1", 50, 0, 0) {
		t.Error("should exceed updated limit of 10")
	}
	// But 5 should still be within limits
	if !mgr.Usage().WithinLimits("b1", 5, 0, 0) {
		t.Error("should be within updated limit of 10")
	}
}

// -------------------------------------------------------------------------
// Rebalancer.SetConfig / Rebalancer.Config
// -------------------------------------------------------------------------

// TestRebalanceConfig_RoundTrip verifies the rebalance config round trip contract.
// Asserts that rebalance config mismatch: v.
func TestRebalanceConfig_RoundTrip(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	mgr := newUsageManager(t, []string{"b1"}, store)
	workers := wireWorkersForTest(mgr, store)

	// Initially nil
	if workers.Rebalancer.Config() != nil {
		t.Error("expected nil initial rebalance config")
	}

	cfg := &config.RebalanceConfig{
		Enabled:   true,
		Strategy:  "spread",
		Interval:  2 * time.Hour,
		BatchSize: 50,
		Threshold: 0.2,
	}
	workers.Rebalancer.SetConfig(cfg)

	got := workers.Rebalancer.Config()
	if got == nil {
		t.Fatal("expected non-nil rebalance config")
	}
	if got.Strategy != "spread" || got.BatchSize != 50 || got.Threshold != 0.2 {
		t.Errorf("rebalance config mismatch: %+v", got)
	}
}

// -------------------------------------------------------------------------
// Replicator.SetConfig / Replicator.Config
// -------------------------------------------------------------------------

// TestReplicationConfig_RoundTrip verifies the replication config round trip contract.
// Asserts that replication config mismatch: v.
func TestReplicationConfig_RoundTrip(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	mgr := newUsageManager(t, []string{"b1"}, store)
	workers := wireWorkersForTest(mgr, store)

	// Initially nil
	if workers.Replicator.Config() != nil {
		t.Error("expected nil initial replication config")
	}

	cfg := &config.ReplicationConfig{
		Factor:         2,
		WorkerInterval: 10 * time.Minute,
		BatchSize:      25,
	}
	workers.Replicator.SetConfig(cfg)

	got := workers.Replicator.Config()
	if got == nil {
		t.Fatal("expected non-nil replication config")
	}
	if got.Factor != 2 || got.WorkerInterval != 10*time.Minute || got.BatchSize != 25 {
		t.Errorf("replication config mismatch: %+v", got)
	}
}

// -------------------------------------------------------------------------
// SetUsageFlushConfig / UsageFlushConfig
// -------------------------------------------------------------------------

// TestUsageFlushConfig_RoundTrip verifies the usage flush config round trip contract.
// Asserts that interval = , want 5m.
func TestUsageFlushConfig_RoundTrip(t *testing.T) {
	t.Parallel()
	mgr := newUsageManager(t, []string{"b1"}, newPermissiveMock(t))

	if mgr.UsageFlushConfig() != nil {
		t.Error("expected nil initial usage flush config")
	}

	cfg := &config.UsageFlushConfig{
		Interval: 5 * time.Minute,
	}
	mgr.SetUsageFlushConfig(cfg)

	got := mgr.UsageFlushConfig()
	if got == nil {
		t.Fatal("expected non-nil usage flush config")
	}
	if got.Interval != 5*time.Minute {
		t.Errorf("interval = %v, want 5m", got.Interval)
	}
}

// -------------------------------------------------------------------------
// SetLifecycleConfig / LifecycleConfig
// -------------------------------------------------------------------------

// TestLifecycleConfig_RoundTrip verifies the lifecycle config round trip contract.
// Asserts that lifecycle config mismatch: v.
func TestLifecycleConfig_RoundTrip(t *testing.T) {
	t.Parallel()
	mgr := newUsageManager(t, []string{"b1"}, newPermissiveMock(t))

	if mgr.LifecycleConfig() != nil {
		t.Error("expected nil initial lifecycle config")
	}

	cfg := &config.LifecycleConfig{
		Rules: []config.LifecycleRule{
			{Prefix: "tmp/", ExpirationDays: 7},
		},
	}
	mgr.SetLifecycleConfig(cfg)

	got := mgr.LifecycleConfig()
	if got == nil {
		t.Fatal("expected non-nil lifecycle config")
	}
	if len(got.Rules) != 1 || got.Rules[0].Prefix != "tmp/" {
		t.Errorf("lifecycle config mismatch: %+v", got)
	}
}

// -------------------------------------------------------------------------
// NearUsageLimit
// -------------------------------------------------------------------------

// TestNearUsageLimit_BelowThreshold verifies the near usage limit below threshold path by exercising mgr.NearUsageLimit.
func TestNearUsageLimit_BelowThreshold(t *testing.T) {
	t.Parallel()
	limits := map[string]core.UsageLimits{
		"b1": {APIRequestLimit: 1000},
	}
	mgr := newUsageManagerWithLimits(t, []string{"b1"}, newPermissiveMock(t), limits)

	// No usage baseline set  -  should be well below threshold
	if mgr.NearUsageLimit(0.8) {
		t.Error("should not be near limit with zero usage")
	}
}

// TestNearUsageLimit_AboveThreshold verifies the near usage limit above threshold path by exercising mgr.NearUsageLimit.
func TestNearUsageLimit_AboveThreshold(t *testing.T) {
	t.Parallel()
	limits := map[string]core.UsageLimits{
		"b1": {APIRequestLimit: 100},
	}
	mgr := newUsageManagerWithLimits(t, []string{"b1"}, newPermissiveMock(t), limits)

	// Set baseline at 90% of limit
	mgr.Usage().SetBaseline("b1", core.UsageStat{APIRequests: 90})

	if !mgr.NearUsageLimit(0.8) {
		t.Error("should be near limit at 90% usage with 80% threshold")
	}
}

// -------------------------------------------------------------------------
// ClearCache
// -------------------------------------------------------------------------

// TestClearCache_RemovesAllEntries verifies the clear cache removes all entries path by exercising mgr.ClearCache.
func TestClearCache_RemovesAllEntries(t *testing.T) {
	t.Parallel()
	mgr := newUsageManager(t, []string{"b1"}, newPermissiveMock(t))

	mgr.objectManager.LocationCache().Set("key1", "b1")
	mgr.objectManager.LocationCache().Set("key2", "b1")

	mgr.ClearCache()

	if _, ok := mgr.objectManager.LocationCache().Get("key1"); ok {
		t.Error("expected key1 cache miss after ClearCache")
	}
	if _, ok := mgr.objectManager.LocationCache().Get("key2"); ok {
		t.Error("expected key2 cache miss after ClearCache")
	}
}

// -------------------------------------------------------------------------
// Concurrent Safety
// -------------------------------------------------------------------------

// TestUpdateUsageLimits_ConcurrentAccess verifies the update usage limits concurrent access path by exercising wg.Add, wg.Done, mgr.UpdateUsageLimits.
func TestUpdateUsageLimits_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	limits := map[string]core.UsageLimits{
		"b1": {APIRequestLimit: 1000},
	}
	mgr := newUsageManagerWithLimits(t, []string{"b1"}, newPermissiveMock(t), limits)

	var wg sync.WaitGroup
	const goroutines = 50

	// Concurrent readers
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range 100 {
				_ = mgr.Usage().WithinLimits("b1", 1, 0, 0)
			}
		}()
	}

	// Concurrent writers
	wg.Add(goroutines)
	for i := range goroutines {
		go func(n int) {
			defer wg.Done()
			for j := range 100 {
				mgr.UpdateUsageLimits(map[string]core.UsageLimits{
					"b1": {APIRequestLimit: int64(n*100 + j)},
				})
			}
		}(i)
	}

	wg.Wait()
	// Test passes if no race detector violations
}

// -------------------------------------------------------------------------
// NewBackendManager constructor validation
// -------------------------------------------------------------------------

// TestNewBackendManager_NilConfig pins the nil-pointer guard. The
// constructor panics via must.NotNil so a DI wiring bug surfaces at
// assembly rather than NPE'ing on first use.
func TestNewBackendManager_NilConfig(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("expected panic on nil config")
		}
	}()
	NewBackendManager(nil)
}

// TestNewBackendManager_RequiredDepsPanic pins each required-dep nil
// guard. Negative-duration validation is no longer the constructor's
// responsibility - the config validator owns that - so this table only
// covers the dep-shape branches.
func TestNewBackendManager_RequiredDepsPanic(t *testing.T) {
	t.Parallel()
	mock := newPermissiveMock(t)

	cases := []struct {
		name string
		cfg  *BackendManagerConfig
	}{
		{"no stores", &BackendManagerConfig{}},
		{"no dashboard", &BackendManagerConfig{Stores: mock}},
		{"no metrics", &BackendManagerConfig{Stores: mock, Dashboard: mock}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Error("expected panic on missing dep")
				}
			}()
			NewBackendManager(tc.cfg)
		})
	}
}

// -------------------------------------------------------------------------
// ClearDrainState (nil-guard + wired path)
// -------------------------------------------------------------------------

// TestClearDrainState_NoDrainManager pins the nil-guard: a manager
// constructed without WireDrain must not panic on ClearDrainState.
func TestClearDrainState_NoDrainManager(t *testing.T) {
	t.Parallel()
	mock := newPermissiveMock(t)
	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Stores:          mock,
		Dashboard:       mock,
		Metrics:         mock,
		RoutingStrategy: config.RoutingPack,
	})
	defer mgr.Close()
	if mgr.drainManager != nil {
		t.Fatal("expected DrainManager nil prior to WireDrain")
	}
	mgr.ClearDrainState()
}

// TestClearDrainState_ClearsWiredDrain pins the through-call path: when a
// drain manager has been wired, ClearDrainState reaches into it without
// panicking.
func TestClearDrainState_ClearsWiredDrain(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	mgr := newUsageManager(t, []string{"b1"}, store)
	workers := wireWorkersForTest(mgr, store)
	_ = workers
	mgr.ClearDrainState()
}
