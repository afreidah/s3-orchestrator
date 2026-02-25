// -------------------------------------------------------------------------------
// Manager Tests - Upload ID Generation and String Utilities
//
// Author: Alex Freidah
//
// Unit tests for the backend manager's utility functions including upload ID
// generation uniqueness and length validation.
// -------------------------------------------------------------------------------

package storage

import (
	"sync"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

func TestGenerateUploadID(t *testing.T) {
	id := GenerateUploadID()

	// Should be 32 hex chars (16 bytes)
	if len(id) != 32 {
		t.Errorf("GenerateUploadID() length = %d, want 32", len(id))
	}

	// Should be unique
	id2 := GenerateUploadID()
	if id == id2 {
		t.Error("GenerateUploadID() should produce unique IDs")
	}
}

// -------------------------------------------------------------------------
// Close (idempotent)
// -------------------------------------------------------------------------

func TestClose_Idempotent(t *testing.T) {
	mgr := newUsageManager([]string{"b1"}, &mockStore{})

	// Calling Close twice should not panic
	mgr.Close()
	mgr.Close()
}

// -------------------------------------------------------------------------
// UpdateUsageLimits
// -------------------------------------------------------------------------

func TestUpdateUsageLimits_SwapsLimits(t *testing.T) {
	limits := map[string]UsageLimits{
		"b1": {APIRequestLimit: 100},
	}
	mgr := newUsageManagerWithLimits([]string{"b1"}, &mockStore{}, limits)

	// Initially within limits
	if !mgr.usage.WithinLimits("b1", 50, 0, 0) {
		t.Fatal("should be within initial limits")
	}

	// Update to a much lower limit
	mgr.UpdateUsageLimits(map[string]UsageLimits{
		"b1": {APIRequestLimit: 10},
	})

	// Now 50 should exceed the new limit
	if mgr.usage.WithinLimits("b1", 50, 0, 0) {
		t.Error("should exceed updated limit of 10")
	}
	// But 5 should still be within limits
	if !mgr.usage.WithinLimits("b1", 5, 0, 0) {
		t.Error("should be within updated limit of 10")
	}
}

// -------------------------------------------------------------------------
// SetRebalanceConfig / RebalanceConfig
// -------------------------------------------------------------------------

func TestRebalanceConfig_RoundTrip(t *testing.T) {
	mgr := newUsageManager([]string{"b1"}, &mockStore{})

	// Initially nil
	if mgr.RebalanceConfig() != nil {
		t.Error("expected nil initial rebalance config")
	}

	cfg := &config.RebalanceConfig{
		Enabled:   true,
		Strategy:  "spread",
		Interval:  2 * time.Hour,
		BatchSize: 50,
		Threshold: 0.2,
	}
	mgr.SetRebalanceConfig(cfg)

	got := mgr.RebalanceConfig()
	if got == nil {
		t.Fatal("expected non-nil rebalance config")
	}
	if got.Strategy != "spread" || got.BatchSize != 50 || got.Threshold != 0.2 {
		t.Errorf("rebalance config mismatch: %+v", got)
	}
}

// -------------------------------------------------------------------------
// SetReplicationConfig / ReplicationConfig
// -------------------------------------------------------------------------

func TestReplicationConfig_RoundTrip(t *testing.T) {
	mgr := newUsageManager([]string{"b1"}, &mockStore{})

	// Initially nil
	if mgr.ReplicationConfig() != nil {
		t.Error("expected nil initial replication config")
	}

	cfg := &config.ReplicationConfig{
		Factor:         2,
		WorkerInterval: 10 * time.Minute,
		BatchSize:      25,
	}
	mgr.SetReplicationConfig(cfg)

	got := mgr.ReplicationConfig()
	if got == nil {
		t.Fatal("expected non-nil replication config")
	}
	if got.Factor != 2 || got.WorkerInterval != 10*time.Minute || got.BatchSize != 25 {
		t.Errorf("replication config mismatch: %+v", got)
	}
}

// -------------------------------------------------------------------------
// Concurrent Safety
// -------------------------------------------------------------------------

func TestUpdateUsageLimits_ConcurrentAccess(t *testing.T) {
	limits := map[string]UsageLimits{
		"b1": {APIRequestLimit: 1000},
	}
	mgr := newUsageManagerWithLimits([]string{"b1"}, &mockStore{}, limits)

	var wg sync.WaitGroup
	const goroutines = 50

	// Concurrent readers
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = mgr.usage.WithinLimits("b1", 1, 0, 0)
			}
		}()
	}

	// Concurrent writers
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				mgr.UpdateUsageLimits(map[string]UsageLimits{
					"b1": {APIRequestLimit: int64(n*100 + j)},
				})
			}
		}(i)
	}

	wg.Wait()
	// Test passes if no race detector violations
}
