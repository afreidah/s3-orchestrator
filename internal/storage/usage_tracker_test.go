// -------------------------------------------------------------------------------
// Usage Tracker Tests
//
// Author: Alex Freidah
//
// Tests for the atomic usage counter and periodic flush mechanism. Validates
// near-limit threshold calculations and counter accumulation behavior.
// -------------------------------------------------------------------------------

package storage

import "testing"

func TestNearLimit_BelowThreshold(t *testing.T) {
	tracker := NewUsageTracker([]string{"b1"}, map[string]UsageLimits{
		"b1": {APIRequestLimit: 1000, EgressByteLimit: 1000},
	})
	tracker.SetBaseline("b1", UsageStat{APIRequests: 100, EgressBytes: 100})

	if tracker.NearLimit(0.8) {
		t.Error("should not be near limit at 10% usage")
	}
}

func TestNearLimit_AboveThreshold(t *testing.T) {
	tracker := NewUsageTracker([]string{"b1"}, map[string]UsageLimits{
		"b1": {APIRequestLimit: 1000},
	})
	tracker.SetBaseline("b1", UsageStat{APIRequests: 850})

	if !tracker.NearLimit(0.8) {
		t.Error("should be near limit at 85% usage")
	}
}

func TestNearLimit_NoLimitsConfigured(t *testing.T) {
	tracker := NewUsageTracker([]string{"b1"}, map[string]UsageLimits{
		"b1": {}, // all zero = unlimited
	})
	tracker.SetBaseline("b1", UsageStat{APIRequests: 999999})

	if tracker.NearLimit(0.8) {
		t.Error("should return false when no limits are configured")
	}
}

func TestNearLimit_ZeroLimitDimension(t *testing.T) {
	tracker := NewUsageTracker([]string{"b1"}, map[string]UsageLimits{
		"b1": {APIRequestLimit: 0, EgressByteLimit: 1000}, // API unlimited, egress limited
	})
	tracker.SetBaseline("b1", UsageStat{APIRequests: 999999, EgressBytes: 100})

	if tracker.NearLimit(0.8) {
		t.Error("should ignore unlimited API dimension; egress at 10% is not near limit")
	}
}

func TestNearLimit_UnflushedCounters(t *testing.T) {
	tracker := NewUsageTracker([]string{"b1"}, map[string]UsageLimits{
		"b1": {EgressByteLimit: 1000},
	})
	tracker.SetBaseline("b1", UsageStat{EgressBytes: 700})
	tracker.Record("b1", 0, 150, 0) // unflushed egress pushes to 850/1000 = 85%

	if !tracker.NearLimit(0.8) {
		t.Error("should be near limit when baseline + unflushed exceeds threshold")
	}
}

func TestNearLimit_MultipleBackends(t *testing.T) {
	tracker := NewUsageTracker([]string{"b1", "b2"}, map[string]UsageLimits{
		"b1": {APIRequestLimit: 1000},
		"b2": {APIRequestLimit: 1000},
	})
	tracker.SetBaseline("b1", UsageStat{APIRequests: 100}) // 10% - fine
	tracker.SetBaseline("b2", UsageStat{APIRequests: 900}) // 90% - near limit

	if !tracker.NearLimit(0.8) {
		t.Error("should return true when any backend is near limit")
	}
}
