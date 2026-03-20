// -------------------------------------------------------------------------------
// MetricsCollector Tests - Prometheus Gauge and Counter Updates
//
// Author: Alex Freidah
//
// Tests for RecordOperation (success/error labeling) and UpdateQuotaMetrics
// (quota stats, object counts, multipart counts, monthly usage, and error paths).
// -------------------------------------------------------------------------------

package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	st "github.com/afreidah/s3-orchestrator/internal/store"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// -------------------------------------------------------------------------
// RecordOperation
// -------------------------------------------------------------------------

func TestRecordOperation_Success(t *testing.T) {
	store := &mockStore{}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil)
	mc := NewMetricsCollector(store, usage, []string{"b1"}, func() int { return 0 })

	// Should not panic
	mc.RecordOperation("PutObject", "b1", time.Now(), nil)
}

func TestRecordOperation_Error(t *testing.T) {
	store := &mockStore{}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil)
	mc := NewMetricsCollector(store, usage, []string{"b1"}, func() int { return 0 })

	// Should not panic with error status
	mc.RecordOperation("GetObject", "b1", time.Now(), errors.New("backend down"))
}

// -------------------------------------------------------------------------
// UpdateQuotaMetrics
// -------------------------------------------------------------------------

func TestUpdateQuotaMetrics_Success(t *testing.T) {
	store := &mockStore{
		getQuotaStatsResp: map[string]st.QuotaStat{
			"b1": {BytesUsed: 500, BytesLimit: 1000},
			"b2": {BytesUsed: 0, BytesLimit: 0}, // unlimited
		},
		getObjectCountsResp: map[string]int64{
			"b1": 42,
		},
		getActiveMultipartResp: map[string]int64{
			"b1": 3,
		},
		getUsageForPeriodResp: map[string]st.UsageStat{
			"b1": {APIRequests: 100, EgressBytes: 5000, IngressBytes: 2000},
		},
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1", "b2"}), nil)
	mc := NewMetricsCollector(store, usage, []string{"b1", "b2"}, func() int { return 0 })

	err := mc.UpdateQuotaMetrics(context.Background())
	if err != nil {
		t.Fatalf("UpdateQuotaMetrics: %v", err)
	}
}

func TestUpdateQuotaMetrics_QuotaStatsError(t *testing.T) {
	store := &mockStore{
		getQuotaStatsErr: errors.New("db down"),
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil)
	mc := NewMetricsCollector(store, usage, []string{"b1"}, func() int { return 0 })

	err := mc.UpdateQuotaMetrics(context.Background())
	if err == nil {
		t.Fatal("expected error from GetQuotaStats failure")
	}
}

func TestUpdateQuotaMetrics_ObjectCountsError(t *testing.T) {
	store := &mockStore{
		getQuotaStatsResp:     map[string]st.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}},
		getObjectCountsErr:    errors.New("db error"),
		getUsageForPeriodResp: map[string]st.UsageStat{},
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}), nil)
	mc := NewMetricsCollector(store, usage, []string{"b1"}, func() int { return 0 })

	// Should not return error — object counts error is logged only
	err := mc.UpdateQuotaMetrics(context.Background())
	if err != nil {
		t.Fatalf("expected nil error (object counts error is non-fatal): %v", err)
	}
}

func TestUpdateQuotaMetrics_MultipartCountsError(t *testing.T) {
	store := &mockStore{
		getQuotaStatsResp:     map[string]st.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}},
		getObjectCountsResp:   map[string]int64{"b1": 5},
		getActiveMultipartErr: errors.New("db error"),
		getUsageForPeriodResp: map[string]st.UsageStat{},
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}), nil)
	mc := NewMetricsCollector(store, usage, []string{"b1"}, func() int { return 0 })

	err := mc.UpdateQuotaMetrics(context.Background())
	if err != nil {
		t.Fatalf("expected nil error (multipart counts error is non-fatal): %v", err)
	}
}

func TestUpdateQuotaMetrics_UsageForPeriodError(t *testing.T) {
	store := &mockStore{
		getQuotaStatsResp:      map[string]st.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}},
		getObjectCountsResp:    map[string]int64{"b1": 5},
		getActiveMultipartResp: map[string]int64{},
		getUsageForPeriodErr:   errors.New("db error"),
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}), nil)
	mc := NewMetricsCollector(store, usage, []string{"b1"}, func() int { return 0 })

	err := mc.UpdateQuotaMetrics(context.Background())
	if err != nil {
		t.Fatalf("expected nil error (usage error is non-fatal): %v", err)
	}
}

// -------------------------------------------------------------------------
// Replication pending gauge
// -------------------------------------------------------------------------

func TestUpdateQuotaMetrics_ReplicationPending(t *testing.T) {
	store := &mockStore{
		getQuotaStatsResp:      map[string]st.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}},
		getObjectCountsResp:    map[string]int64{"b1": 5},
		getActiveMultipartResp: map[string]int64{},
		getUsageForPeriodResp:  map[string]st.UsageStat{},
		getUnderReplicatedResp: []st.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100},
		},
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}), nil)
	mc := NewMetricsCollector(store, usage, []string{"b1"}, func() int { return 2 })

	err := mc.UpdateQuotaMetrics(context.Background())
	if err != nil {
		t.Fatalf("UpdateQuotaMetrics: %v", err)
	}
}

func TestUpdateQuotaMetrics_ReplicationPendingSkippedWhenDisabled(t *testing.T) {
	store := &mockStore{
		getQuotaStatsResp:      map[string]st.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}},
		getObjectCountsResp:    map[string]int64{"b1": 5},
		getActiveMultipartResp: map[string]int64{},
		getUsageForPeriodResp:  map[string]st.UsageStat{},
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}), nil)
	mc := NewMetricsCollector(store, usage, []string{"b1"}, func() int { return 0 })

	err := mc.UpdateQuotaMetrics(context.Background())
	if err != nil {
		t.Fatalf("UpdateQuotaMetrics: %v", err)
	}
}

func TestUpdateQuotaMetrics_ReplicationPendingQueryError(t *testing.T) {
	store := &mockStore{
		getQuotaStatsResp:      map[string]st.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}},
		getObjectCountsResp:    map[string]int64{"b1": 5},
		getActiveMultipartResp: map[string]int64{},
		getUsageForPeriodResp:  map[string]st.UsageStat{},
		getUnderReplicatedErr:  errors.New("db error"),
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}), nil)
	mc := NewMetricsCollector(store, usage, []string{"b1"}, func() int { return 2 })

	// Should not return error — under-replicated query error is non-fatal
	err := mc.UpdateQuotaMetrics(context.Background())
	if err != nil {
		t.Fatalf("expected nil error (under-replicated query error is non-fatal): %v", err)
	}
}

func TestUpdateQuotaMetrics_ReplicationFactorFromManager(t *testing.T) {
	store := &mockStore{
		getQuotaStatsResp:      map[string]st.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}},
		getObjectCountsResp:    map[string]int64{"b1": 5},
		getActiveMultipartResp: map[string]int64{},
		getUsageForPeriodResp:  map[string]st.UsageStat{},
		getUnderReplicatedResp: []st.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100},
		},
	}
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": newMockBackend()},
		Store:           store,
		Order:           []string{"b1"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: "pack",
	})

	// Without replication config — closure returns 0, skips query
	err := mgr.metrics.UpdateQuotaMetrics(context.Background())
	if err != nil {
		t.Fatalf("UpdateQuotaMetrics (no repl config): %v", err)
	}

	// With replication config — closure returns factor, queries DB
	mgr.Replicator.SetConfig(&config.ReplicationConfig{Factor: 2, BatchSize: 50})
	err = mgr.metrics.UpdateQuotaMetrics(context.Background())
	if err != nil {
		t.Fatalf("UpdateQuotaMetrics (with repl config): %v", err)
	}
}
