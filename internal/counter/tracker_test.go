// -------------------------------------------------------------------------------
// Usage Tracker Tests
//
// Author: Alex Freidah
//
// Tests for the atomic usage counter and periodic flush mechanism. Validates
// near-limit threshold calculations and counter accumulation behavior.
// -------------------------------------------------------------------------------

package counter

import (
	"context"
	"errors"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store"
)

func TestNewUsageTracker_NilLimits(t *testing.T) {
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), nil)
	// Should not panic; nil limits treated as empty map.
	if tracker.NearLimit(0.8) {
		t.Error("nil limits should never be near limit")
	}
}

func TestNearLimit_BelowThreshold(t *testing.T) {
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]store.UsageLimits{
		"b1": {APIRequestLimit: 1000, EgressByteLimit: 1000},
	})
	tracker.SetBaseline("b1", store.UsageStat{APIRequests: 100, EgressBytes: 100})

	if tracker.NearLimit(0.8) {
		t.Error("should not be near limit at 10% usage")
	}
}

func TestNearLimit_AboveThreshold(t *testing.T) {
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]store.UsageLimits{
		"b1": {APIRequestLimit: 1000},
	})
	tracker.SetBaseline("b1", store.UsageStat{APIRequests: 850})

	if !tracker.NearLimit(0.8) {
		t.Error("should be near limit at 85% usage")
	}
}

func TestNearLimit_NoLimitsConfigured(t *testing.T) {
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]store.UsageLimits{
		"b1": {}, // all zero = unlimited
	})
	tracker.SetBaseline("b1", store.UsageStat{APIRequests: 999999})

	if tracker.NearLimit(0.8) {
		t.Error("should return false when no limits are configured")
	}
}

func TestNearLimit_ZeroLimitDimension(t *testing.T) {
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]store.UsageLimits{
		"b1": {APIRequestLimit: 0, EgressByteLimit: 1000}, // API unlimited, egress limited
	})
	tracker.SetBaseline("b1", store.UsageStat{APIRequests: 999999, EgressBytes: 100})

	if tracker.NearLimit(0.8) {
		t.Error("should ignore unlimited API dimension; egress at 10% is not near limit")
	}
}

func TestNearLimit_UnflushedCounters(t *testing.T) {
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]store.UsageLimits{
		"b1": {EgressByteLimit: 1000},
	})
	tracker.SetBaseline("b1", store.UsageStat{EgressBytes: 700})
	tracker.Record("b1", 0, 150, 0) // unflushed egress pushes to 850/1000 = 85%

	if !tracker.NearLimit(0.8) {
		t.Error("should be near limit when baseline + unflushed exceeds threshold")
	}
}

func TestNearLimit_MultipleBackends(t *testing.T) {
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1", "b2"}), map[string]store.UsageLimits{
		"b1": {APIRequestLimit: 1000},
		"b2": {APIRequestLimit: 1000},
	})
	tracker.SetBaseline("b1", store.UsageStat{APIRequests: 100}) // 10% - fine
	tracker.SetBaseline("b2", store.UsageStat{APIRequests: 900}) // 90% - near limit

	if !tracker.NearLimit(0.8) {
		t.Error("should return true when any backend is near limit")
	}
}

// -------------------------------------------------------------------------
// WithinLimits
// -------------------------------------------------------------------------

func TestWithinLimits_AllWithinLimits(t *testing.T) {
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]store.UsageLimits{
		"b1": {APIRequestLimit: 1000, EgressByteLimit: 1000, IngressByteLimit: 1000},
	})
	if !tracker.WithinLimits("b1", 1, 1, 1) {
		t.Error("should be within limits at zero usage")
	}
}

func TestWithinLimits_APILimitExceeded(t *testing.T) {
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]store.UsageLimits{
		"b1": {APIRequestLimit: 100},
	})
	tracker.SetBaseline("b1", store.UsageStat{APIRequests: 99})

	if tracker.WithinLimits("b1", 2, 0, 0) {
		t.Error("should exceed API limit (99 + 2 > 100)")
	}
}

func TestWithinLimits_EgressLimitExceeded(t *testing.T) {
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]store.UsageLimits{
		"b1": {EgressByteLimit: 1000},
	})
	tracker.SetBaseline("b1", store.UsageStat{EgressBytes: 900})

	if tracker.WithinLimits("b1", 0, 200, 0) {
		t.Error("should exceed egress limit (900 + 200 > 1000)")
	}
}

func TestWithinLimits_IngressLimitExceeded(t *testing.T) {
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]store.UsageLimits{
		"b1": {IngressByteLimit: 500},
	})
	tracker.SetBaseline("b1", store.UsageStat{IngressBytes: 400})

	if tracker.WithinLimits("b1", 0, 0, 200) {
		t.Error("should exceed ingress limit (400 + 200 > 500)")
	}
}

func TestWithinLimits_NoLimitsConfigured(t *testing.T) {
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), nil)

	if !tracker.WithinLimits("b1", 999999, 999999, 999999) {
		t.Error("should be within limits when no limits configured")
	}
}

func TestWithinLimits_UnknownBackend(t *testing.T) {
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]store.UsageLimits{
		"b1": {APIRequestLimit: 100},
	})

	if !tracker.WithinLimits("unknown", 999999, 0, 0) {
		t.Error("unknown backend should be within limits (no config)")
	}
}

func TestWithinLimits_IncludesUnflushedCounters(t *testing.T) {
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]store.UsageLimits{
		"b1": {APIRequestLimit: 100},
	})
	tracker.SetBaseline("b1", store.UsageStat{APIRequests: 50})
	tracker.Record("b1", 40, 0, 0) // baseline 50 + unflushed 40 = 90 effective

	if !tracker.WithinLimits("b1", 5, 0, 0) {
		t.Error("90 + 5 = 95 should be within limit of 100")
	}
	if tracker.WithinLimits("b1", 15, 0, 0) {
		t.Error("90 + 15 = 105 should exceed limit of 100")
	}
}

// -------------------------------------------------------------------------
// BackendsWithinLimits
// -------------------------------------------------------------------------

func TestBackendsWithinLimits_FiltersCorrectly(t *testing.T) {
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1", "b2"}), map[string]store.UsageLimits{
		"b1": {APIRequestLimit: 10},
		"b2": {APIRequestLimit: 1000},
	})
	tracker.SetBaseline("b1", store.UsageStat{APIRequests: 10}) // at limit

	eligible := tracker.BackendsWithinLimits([]string{"b1", "b2"}, 1, 0, 0)
	if len(eligible) != 1 || eligible[0] != "b2" {
		t.Errorf("expected [b2], got %v", eligible)
	}
}

// -------------------------------------------------------------------------
// UpdateLimits / GetLimits
// -------------------------------------------------------------------------

func TestUpdateLimits_GetLimits_RoundTrip(t *testing.T) {
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), nil)
	newLimits := map[string]store.UsageLimits{
		"b1": {APIRequestLimit: 500},
	}
	tracker.UpdateLimits(newLimits)

	got := tracker.GetLimits()
	if got["b1"].APIRequestLimit != 500 {
		t.Errorf("GetLimits()[b1].APIRequestLimit = %d, want 500", got["b1"].APIRequestLimit)
	}
}

func TestGetLimits_ReturnsCopy(t *testing.T) {
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1"}), map[string]store.UsageLimits{
		"b1": {APIRequestLimit: 100},
	})
	got := tracker.GetLimits()
	delete(got, "b1")

	// Original should be unaffected
	original := tracker.GetLimits()
	if _, ok := original["b1"]; !ok {
		t.Error("GetLimits should return a copy, not the original map")
	}
}

// -------------------------------------------------------------------------
// ResetBaselines
// -------------------------------------------------------------------------

func TestResetBaselines(t *testing.T) {
	tracker := NewUsageTracker(NewLocalCounterBackend([]string{"b1", "b2"}), map[string]store.UsageLimits{
		"b1": {APIRequestLimit: 1000},
		"b2": {APIRequestLimit: 1000},
	})
	tracker.SetBaseline("b1", store.UsageStat{APIRequests: 500})
	tracker.SetBaseline("b2", store.UsageStat{APIRequests: 500})

	tracker.ResetBaselines([]string{"b1"})

	if !tracker.WithinLimits("b1", 999, 0, 0) {
		t.Error("b1 baseline should be reset to zero")
	}
}

// -------------------------------------------------------------------------
// FlushUsage
// -------------------------------------------------------------------------

func TestFlushUsage_SwapsAndFlushes(t *testing.T) {
	backend := NewLocalCounterBackend([]string{"b1"})
	tracker := NewUsageTracker(backend, nil)
	tracker.Record("b1", 10, 100, 50)

	var flushedAPI, flushedEgress, flushedIngress int64
	mockFlusher := &mockUsageFlusher{fn: func(name, period string, api, eg, ing int64) error {
		flushedAPI = api
		flushedEgress = eg
		flushedIngress = ing
		return nil
	}}

	err := tracker.FlushUsage(context.Background(), mockFlusher, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if flushedAPI != 10 || flushedEgress != 100 || flushedIngress != 50 {
		t.Errorf("flushed api=%d egress=%d ingress=%d, want 10/100/50", flushedAPI, flushedEgress, flushedIngress)
	}

	// Counters should be zero after flush
	all := backend.LoadAll("b1")
	if all.APIRequests != 0 || all.EgressBytes != 0 || all.IngressBytes != 0 {
		t.Error("counters should be zero after flush")
	}
}

func TestFlushUsage_SkipsBackendsInSkipMap(t *testing.T) {
	backend := NewLocalCounterBackend([]string{"b1"})
	tracker := NewUsageTracker(backend, nil)
	tracker.Record("b1", 10, 0, 0)

	called := false
	mockFlusher := &mockUsageFlusher{fn: func(_, _ string, _, _, _ int64) error {
		called = true
		return nil
	}}

	_ = tracker.FlushUsage(context.Background(), mockFlusher, map[string]bool{"b1": true})
	if called {
		t.Error("should skip backends in skip map")
	}
}

func TestFlushUsage_RestoresOnError(t *testing.T) {
	cb := NewLocalCounterBackend([]string{"b1"})
	tracker := NewUsageTracker(cb, nil)
	tracker.Record("b1", 10, 0, 0)

	mockFlusher := &mockUsageFlusher{fn: func(_, _ string, _, _, _ int64) error {
		return errors.New("db down")
	}}

	err := tracker.FlushUsage(context.Background(), mockFlusher, nil)
	if err == nil {
		t.Fatal("expected error")
	}

	// Counter should be restored
	if cb.Load("b1", FieldAPIRequests) != 10 {
		t.Errorf("counter should be restored after flush error, got %d", cb.Load("b1", FieldAPIRequests))
	}
}

// -------------------------------------------------------------------------
// Backend accessor
// -------------------------------------------------------------------------

func TestBackend_ReturnsUnderlyingBackend(t *testing.T) {
	cb := NewLocalCounterBackend([]string{"b1"})
	tracker := NewUsageTracker(cb, nil)
	if tracker.Backend() != cb {
		t.Error("Backend() should return the underlying counter backend")
	}
}

// -------------------------------------------------------------------------
// CurrentPeriod
// -------------------------------------------------------------------------

func TestCurrentPeriod_Format(t *testing.T) {
	p := CurrentPeriod()
	if len(p) != 7 || p[4] != '-' {
		t.Errorf("CurrentPeriod() = %q, want YYYY-MM format", p)
	}
}

// -------------------------------------------------------------------------
// Test helpers
// -------------------------------------------------------------------------

type mockUsageFlusher struct {
	fn func(name, period string, api, egress, ingress int64) error
}

func (m *mockUsageFlusher) FlushUsageDeltas(ctx context.Context, name, period string, api, egress, ingress int64) error {
	return m.fn(name, period, api, egress, ingress)
}
