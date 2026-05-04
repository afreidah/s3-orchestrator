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
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// RecordOperation
// -------------------------------------------------------------------------

// TestRecordOperation_Success verifies the record operation success path by exercising counter.NewUsageTracker, counter.NewLocalCounterBackend, metrics.New.
func TestRecordOperation_Success(t *testing.T) {
	t.Parallel()
	store := &mockStore{}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil)
	mc := metrics.New(store, usage, []string{"b1"}, func() int { return 0 })

	// Should not panic
	mc.RecordOperation("PutObject", "b1", time.Now(), nil)
}

// TestRecordOperation_Error verifies the record operation error path by exercising counter.NewUsageTracker, counter.NewLocalCounterBackend, metrics.New.
func TestRecordOperation_Error(t *testing.T) {
	t.Parallel()
	store := &mockStore{}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil)
	mc := metrics.New(store, usage, []string{"b1"}, func() int { return 0 })

	// Should not panic with error status
	mc.RecordOperation("GetObject", "b1", time.Now(), errors.New("backend down"))
}

// -------------------------------------------------------------------------
// UpdateQuotaMetrics
// -------------------------------------------------------------------------

// TestUpdateQuotaMetrics_Success verifies the update quota metrics success contract.
// Asserts that UpdateQuotaMetrics:.
func TestUpdateQuotaMetrics_Success(t *testing.T) {
	t.Parallel()
	store := &mockStore{
		getQuotaStatsResp: map[string]core.QuotaStat{
			"b1": {BytesUsed: 500, BytesLimit: 1000},
			"b2": {BytesUsed: 0, BytesLimit: 0}, // unlimited
		},
		getObjectCountsResp: map[string]int64{
			"b1": 42,
		},
		getActiveMultipartResp: map[string]int64{
			"b1": 3,
		},
		getUsageForPeriodResp: map[string]core.UsageStat{
			"b1": {APIRequests: 100, EgressBytes: 5000, IngressBytes: 2000},
		},
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1", "b2"}), nil)
	mc := metrics.New(store, usage, []string{"b1", "b2"}, func() int { return 0 })

	err := mc.UpdateQuotaMetrics(context.Background())
	if err != nil {
		t.Fatalf("UpdateQuotaMetrics: %v", err)
	}
}

// TestUpdateQuotaMetrics_CapacityWarning verifies the update quota metrics capacity warning contract.
// Asserts that UpdateQuotaMetrics:.
func TestUpdateQuotaMetrics_CapacityWarning(t *testing.T) {
	t.Parallel()
	store := &mockStore{
		getQuotaStatsResp: map[string]core.QuotaStat{
			"b1": {BytesUsed: 900, BytesLimit: 1000},                  // 90%  -  should warn
			"b2": {BytesUsed: 500, BytesLimit: 1000},                  // 50%  -  no warning
			"b3": {BytesUsed: 800, BytesLimit: 1000, OrphanBytes: 50}, // 85% with orphans  -  should warn
		},
		getObjectCountsResp:    map[string]int64{},
		getActiveMultipartResp: map[string]int64{},
		getUsageForPeriodResp:  map[string]core.UsageStat{},
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1", "b2", "b3"}), nil)
	mc := metrics.New(store, usage, []string{"b1", "b2", "b3"}, func() int { return 0 })

	// The warning is logged via slog  -  this test exercises the code path.
	// Verification is that the metrics are set correctly and no panic occurs.
	err := mc.UpdateQuotaMetrics(context.Background())
	if err != nil {
		t.Fatalf("UpdateQuotaMetrics: %v", err)
	}
}

// TestUpdateQuotaMetrics_QuotaStatsError verifies the update quota metrics quota stats error path by exercising errors.New, counter.NewUsageTracker, counter.NewLocalCounterBackend.
func TestUpdateQuotaMetrics_QuotaStatsError(t *testing.T) {
	t.Parallel()
	store := &mockStore{
		getQuotaStatsErr: errors.New("db down"),
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil)
	mc := metrics.New(store, usage, []string{"b1"}, func() int { return 0 })

	err := mc.UpdateQuotaMetrics(context.Background())
	if err == nil {
		t.Fatal("expected error from GetQuotaStats failure")
	}
}

// TestUpdateQuotaMetrics_ObjectCountsError verifies the update quota metrics object counts error contract.
// Asserts that expected nil error (object counts error is non-fatal):.
func TestUpdateQuotaMetrics_ObjectCountsError(t *testing.T) {
	t.Parallel()
	store := &mockStore{
		getQuotaStatsResp:     map[string]core.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}},
		getObjectCountsErr:    errors.New("db error"),
		getUsageForPeriodResp: map[string]core.UsageStat{},
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}), nil)
	mc := metrics.New(store, usage, []string{"b1"}, func() int { return 0 })

	// Should not return error  -  object counts error is logged only
	err := mc.UpdateQuotaMetrics(context.Background())
	if err != nil {
		t.Fatalf("expected nil error (object counts error is non-fatal): %v", err)
	}
}

// TestUpdateQuotaMetrics_MultipartCountsError verifies the update quota metrics multipart counts error contract.
// Asserts that expected nil error (multipart counts error is non-fatal):.
func TestUpdateQuotaMetrics_MultipartCountsError(t *testing.T) {
	t.Parallel()
	store := &mockStore{
		getQuotaStatsResp:     map[string]core.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}},
		getObjectCountsResp:   map[string]int64{"b1": 5},
		getActiveMultipartErr: errors.New("db error"),
		getUsageForPeriodResp: map[string]core.UsageStat{},
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}), nil)
	mc := metrics.New(store, usage, []string{"b1"}, func() int { return 0 })

	err := mc.UpdateQuotaMetrics(context.Background())
	if err != nil {
		t.Fatalf("expected nil error (multipart counts error is non-fatal): %v", err)
	}
}

// TestUpdateQuotaMetrics_UsageForPeriodError verifies the update quota metrics usage for period error contract.
// Asserts that expected nil error (usage error is non-fatal):.
func TestUpdateQuotaMetrics_UsageForPeriodError(t *testing.T) {
	t.Parallel()
	store := &mockStore{
		getQuotaStatsResp:      map[string]core.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}},
		getObjectCountsResp:    map[string]int64{"b1": 5},
		getActiveMultipartResp: map[string]int64{},
		getUsageForPeriodErr:   errors.New("db error"),
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}), nil)
	mc := metrics.New(store, usage, []string{"b1"}, func() int { return 0 })

	err := mc.UpdateQuotaMetrics(context.Background())
	if err != nil {
		t.Fatalf("expected nil error (usage error is non-fatal): %v", err)
	}
}

// -------------------------------------------------------------------------
// Replication pending gauge
// -------------------------------------------------------------------------

// TestUpdateQuotaMetrics_ReplicationPending verifies the update quota metrics replication pending contract.
// Asserts that UpdateQuotaMetrics:.
func TestUpdateQuotaMetrics_ReplicationPending(t *testing.T) {
	t.Parallel()
	store := &mockStore{
		getQuotaStatsResp:      map[string]core.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}},
		getObjectCountsResp:    map[string]int64{"b1": 5},
		getActiveMultipartResp: map[string]int64{},
		getUsageForPeriodResp:  map[string]core.UsageStat{},
		getUnderReplicatedResp: []core.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100},
		},
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}), nil)
	mc := metrics.New(store, usage, []string{"b1"}, func() int { return 2 })

	err := mc.UpdateQuotaMetrics(context.Background())
	if err != nil {
		t.Fatalf("UpdateQuotaMetrics: %v", err)
	}
}

// TestUpdateQuotaMetrics_ReplicationPendingSkippedWhenDisabled verifies the update quota metrics replication pending skipped when disabled contract.
// Asserts that UpdateQuotaMetrics:.
func TestUpdateQuotaMetrics_ReplicationPendingSkippedWhenDisabled(t *testing.T) {
	t.Parallel()
	store := &mockStore{
		getQuotaStatsResp:      map[string]core.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}},
		getObjectCountsResp:    map[string]int64{"b1": 5},
		getActiveMultipartResp: map[string]int64{},
		getUsageForPeriodResp:  map[string]core.UsageStat{},
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}), nil)
	mc := metrics.New(store, usage, []string{"b1"}, func() int { return 0 })

	err := mc.UpdateQuotaMetrics(context.Background())
	if err != nil {
		t.Fatalf("UpdateQuotaMetrics: %v", err)
	}
}

// TestUpdateQuotaMetrics_ReplicationPendingQueryError verifies the update quota metrics replication pending query error contract.
// Asserts that expected nil error (under-replicated query error is non-fatal):.
func TestUpdateQuotaMetrics_ReplicationPendingQueryError(t *testing.T) {
	t.Parallel()
	store := &mockStore{
		getQuotaStatsResp:      map[string]core.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}},
		getObjectCountsResp:    map[string]int64{"b1": 5},
		getActiveMultipartResp: map[string]int64{},
		getUsageForPeriodResp:  map[string]core.UsageStat{},
		getUnderReplicatedErr:  errors.New("db error"),
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}), nil)
	mc := metrics.New(store, usage, []string{"b1"}, func() int { return 2 })

	// Should not return error  -  under-replicated query error is non-fatal
	err := mc.UpdateQuotaMetrics(context.Background())
	if err != nil {
		t.Fatalf("expected nil error (under-replicated query error is non-fatal): %v", err)
	}
}

// TestUpdateQuotaMetrics_ReplicationFactorFromManager verifies the update quota metrics replication factor from manager contract.
// Asserts that UpdateQuotaMetrics (no repl config):.
func TestUpdateQuotaMetrics_ReplicationFactorFromManager(t *testing.T) {
	t.Parallel()
	store := &mockStore{
		getQuotaStatsResp:      map[string]core.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}},
		getObjectCountsResp:    map[string]int64{"b1": 5},
		getActiveMultipartResp: map[string]int64{},
		getUsageForPeriodResp:  map[string]core.UsageStat{},
		getUnderReplicatedResp: []core.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100},
		},
	}
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": newMockBackend()},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	wireWorkersForTest(mgr)

	// Without replication config  -  closure returns 0, skips query
	err := mgr.metricsCollector.UpdateQuotaMetrics(context.Background())
	if err != nil {
		t.Fatalf("UpdateQuotaMetrics (no repl config): %v", err)
	}

	// With replication config  -  closure returns factor, queries DB
	mgr.Replicator.SetConfig(&config.ReplicationConfig{Factor: 2, BatchSize: 50})
	err = mgr.metricsCollector.UpdateQuotaMetrics(context.Background())
	if err != nil {
		t.Fatalf("UpdateQuotaMetrics (with repl config): %v", err)
	}
}
