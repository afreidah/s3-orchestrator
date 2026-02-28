// -------------------------------------------------------------------------------
// MetricsCollector Tests - Prometheus Gauge and Counter Updates
//
// Author: Alex Freidah
//
// Tests for RecordOperation (success/error labeling) and UpdateQuotaMetrics
// (quota stats, object counts, multipart counts, monthly usage, and error paths).
// -------------------------------------------------------------------------------

package storage

import (
	"context"
	"errors"
	"testing"
	"time"
)

// -------------------------------------------------------------------------
// RecordOperation
// -------------------------------------------------------------------------

func TestRecordOperation_Success(t *testing.T) {
	store := &mockStore{}
	usage := NewUsageTracker(nil, nil)
	mc := NewMetricsCollector(store, usage, []string{"b1"})

	// Should not panic
	mc.RecordOperation("PutObject", "b1", time.Now(), nil)
}

func TestRecordOperation_Error(t *testing.T) {
	store := &mockStore{}
	usage := NewUsageTracker(nil, nil)
	mc := NewMetricsCollector(store, usage, []string{"b1"})

	// Should not panic with error status
	mc.RecordOperation("GetObject", "b1", time.Now(), errors.New("backend down"))
}

// -------------------------------------------------------------------------
// UpdateQuotaMetrics
// -------------------------------------------------------------------------

func TestUpdateQuotaMetrics_Success(t *testing.T) {
	store := &mockStore{
		getQuotaStatsResp: map[string]QuotaStat{
			"b1": {BytesUsed: 500, BytesLimit: 1000},
			"b2": {BytesUsed: 0, BytesLimit: 0}, // unlimited
		},
		getObjectCountsResp: map[string]int64{
			"b1": 42,
		},
		getActiveMultipartResp: map[string]int64{
			"b1": 3,
		},
		getUsageForPeriodResp: map[string]UsageStat{
			"b1": {APIRequests: 100, EgressBytes: 5000, IngressBytes: 2000},
		},
	}
	usage := NewUsageTracker([]string{"b1", "b2"}, nil)
	mc := NewMetricsCollector(store, usage, []string{"b1", "b2"})

	err := mc.UpdateQuotaMetrics(context.Background())
	if err != nil {
		t.Fatalf("UpdateQuotaMetrics: %v", err)
	}
}

func TestUpdateQuotaMetrics_QuotaStatsError(t *testing.T) {
	store := &mockStore{
		getQuotaStatsErr: errors.New("db down"),
	}
	usage := NewUsageTracker(nil, nil)
	mc := NewMetricsCollector(store, usage, []string{"b1"})

	err := mc.UpdateQuotaMetrics(context.Background())
	if err == nil {
		t.Fatal("expected error from GetQuotaStats failure")
	}
}

func TestUpdateQuotaMetrics_ObjectCountsError(t *testing.T) {
	store := &mockStore{
		getQuotaStatsResp:  map[string]QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}},
		getObjectCountsErr: errors.New("db error"),
		getUsageForPeriodResp: map[string]UsageStat{},
	}
	usage := NewUsageTracker([]string{"b1"}, nil)
	mc := NewMetricsCollector(store, usage, []string{"b1"})

	// Should not return error — object counts error is logged only
	err := mc.UpdateQuotaMetrics(context.Background())
	if err != nil {
		t.Fatalf("expected nil error (object counts error is non-fatal): %v", err)
	}
}

func TestUpdateQuotaMetrics_MultipartCountsError(t *testing.T) {
	store := &mockStore{
		getQuotaStatsResp:    map[string]QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}},
		getObjectCountsResp:  map[string]int64{"b1": 5},
		getActiveMultipartErr: errors.New("db error"),
		getUsageForPeriodResp: map[string]UsageStat{},
	}
	usage := NewUsageTracker([]string{"b1"}, nil)
	mc := NewMetricsCollector(store, usage, []string{"b1"})

	err := mc.UpdateQuotaMetrics(context.Background())
	if err != nil {
		t.Fatalf("expected nil error (multipart counts error is non-fatal): %v", err)
	}
}

func TestUpdateQuotaMetrics_UsageForPeriodError(t *testing.T) {
	store := &mockStore{
		getQuotaStatsResp:    map[string]QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}},
		getObjectCountsResp:  map[string]int64{"b1": 5},
		getActiveMultipartResp: map[string]int64{},
		getUsageForPeriodErr: errors.New("db error"),
	}
	usage := NewUsageTracker([]string{"b1"}, nil)
	mc := NewMetricsCollector(store, usage, []string{"b1"})

	err := mc.UpdateQuotaMetrics(context.Background())
	if err != nil {
		t.Fatalf("expected nil error (usage error is non-fatal): %v", err)
	}
}
