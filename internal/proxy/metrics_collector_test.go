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

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// stubMetricsStore returns a permissive MockMetadataStore plus the test
// helper that registers it. Most tests in this file don't drive specific
// store calls; they exercise the metrics-collector code path itself.
func stubMetricsStore(t *testing.T) *storetest.MockMetadataStore {
	t.Helper()
	m := storetest.NewMockMetadataStore(gomock.NewController(t))
	storetest.Permissive(m)
	return m
}

// TestRecordOperation_Success exercises the success label path.
func TestRecordOperation_Success(t *testing.T) {
	t.Parallel()
	store := stubMetricsStore(t)
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil)
	mc := metrics.New(metrics.CollectorDeps{Store: store, Usage: usage, BackendNames: []string{"b1"}, ReplicationFactor: func() int { return 0 }})
	mc.RecordOperation("PutObject", "b1", time.Now(), nil)
}

// TestRecordOperation_Error exercises the error label path.
func TestRecordOperation_Error(t *testing.T) {
	t.Parallel()
	store := stubMetricsStore(t)
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil)
	mc := metrics.New(metrics.CollectorDeps{Store: store, Usage: usage, BackendNames: []string{"b1"}, ReplicationFactor: func() int { return 0 }})
	mc.RecordOperation("GetObject", "b1", time.Now(), errors.New("backend down"))
}

// TestUpdateQuotaMetrics_Success drives every store call returning a
// non-empty result.
func TestUpdateQuotaMetrics_Success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{
			"b1": {BytesUsed: 500, BytesLimit: 1000},
			"b2": {BytesUsed: 0, BytesLimit: 0},
		}, nil).
		AnyTimes()
	store.EXPECT().GetObjectCounts(gomock.Any()).
		Return(map[string]int64{"b1": 42}, nil).
		AnyTimes()
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).
		Return(map[string]int64{"b1": 3}, nil).
		AnyTimes()
	store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).
		Return(map[string]core.UsageStat{
			"b1": {APIRequests: 100, EgressBytes: 5000, IngressBytes: 2000},
		}, nil).
		AnyTimes()
	storetest.Permissive(store)

	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1", "b2"}), nil)
	mc := metrics.New(metrics.CollectorDeps{Store: store, Usage: usage, BackendNames: []string{"b1", "b2"}, ReplicationFactor: func() int { return 0 }})

	if err := mc.UpdateQuotaMetrics(context.Background()); err != nil {
		t.Fatalf("UpdateQuotaMetrics: %v", err)
	}
}

// TestUpdateQuotaMetrics_CapacityWarning exercises the capacity-warning
// branch.
func TestUpdateQuotaMetrics_CapacityWarning(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{
			"b1": {BytesUsed: 900, BytesLimit: 1000},
			"b2": {BytesUsed: 500, BytesLimit: 1000},
			"b3": {BytesUsed: 800, BytesLimit: 1000, OrphanBytes: 50},
		}, nil).
		AnyTimes()
	store.EXPECT().GetObjectCounts(gomock.Any()).Return(map[string]int64{}, nil).AnyTimes()
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).Return(map[string]int64{}, nil).AnyTimes()
	store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).Return(map[string]core.UsageStat{}, nil).AnyTimes()
	storetest.Permissive(store)

	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1", "b2", "b3"}), nil)
	mc := metrics.New(metrics.CollectorDeps{Store: store, Usage: usage, BackendNames: []string{"b1", "b2", "b3"}, ReplicationFactor: func() int { return 0 }})
	if err := mc.UpdateQuotaMetrics(context.Background()); err != nil {
		t.Fatalf("UpdateQuotaMetrics: %v", err)
	}
}

// TestUpdateQuotaMetrics_QuotaStatsError surfaces the fatal quota-stats
// failure.
func TestUpdateQuotaMetrics_QuotaStatsError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).Return(nil, errors.New("db down")).AnyTimes()
	storetest.Permissive(store)

	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil)
	mc := metrics.New(metrics.CollectorDeps{Store: store, Usage: usage, BackendNames: []string{"b1"}, ReplicationFactor: func() int { return 0 }})
	if err := mc.UpdateQuotaMetrics(context.Background()); err == nil {
		t.Fatal("expected error from GetQuotaStats failure")
	}
}

// TestUpdateQuotaMetrics_ObjectCountsError swallows the non-fatal
// object-counts error.
func TestUpdateQuotaMetrics_ObjectCountsError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}}, nil).
		AnyTimes()
	store.EXPECT().GetObjectCounts(gomock.Any()).Return(nil, errors.New("db error")).AnyTimes()
	store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).Return(map[string]core.UsageStat{}, nil).AnyTimes()
	storetest.Permissive(store)

	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}), nil)
	mc := metrics.New(metrics.CollectorDeps{Store: store, Usage: usage, BackendNames: []string{"b1"}, ReplicationFactor: func() int { return 0 }})
	if err := mc.UpdateQuotaMetrics(context.Background()); err != nil {
		t.Fatalf("expected nil error (object counts error is non-fatal): %v", err)
	}
}

// TestUpdateQuotaMetrics_MultipartCountsError swallows the non-fatal
// multipart-counts error.
func TestUpdateQuotaMetrics_MultipartCountsError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}}, nil).
		AnyTimes()
	store.EXPECT().GetObjectCounts(gomock.Any()).Return(map[string]int64{"b1": 5}, nil).AnyTimes()
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).Return(nil, errors.New("db error")).AnyTimes()
	store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).Return(map[string]core.UsageStat{}, nil).AnyTimes()
	storetest.Permissive(store)

	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}), nil)
	mc := metrics.New(metrics.CollectorDeps{Store: store, Usage: usage, BackendNames: []string{"b1"}, ReplicationFactor: func() int { return 0 }})
	if err := mc.UpdateQuotaMetrics(context.Background()); err != nil {
		t.Fatalf("expected nil error (multipart counts error is non-fatal): %v", err)
	}
}

// TestUpdateQuotaMetrics_UsageForPeriodError swallows the non-fatal
// usage-for-period error.
func TestUpdateQuotaMetrics_UsageForPeriodError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}}, nil).
		AnyTimes()
	store.EXPECT().GetObjectCounts(gomock.Any()).Return(map[string]int64{"b1": 5}, nil).AnyTimes()
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).Return(map[string]int64{}, nil).AnyTimes()
	store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).Return(nil, errors.New("db error")).AnyTimes()
	storetest.Permissive(store)

	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}), nil)
	mc := metrics.New(metrics.CollectorDeps{Store: store, Usage: usage, BackendNames: []string{"b1"}, ReplicationFactor: func() int { return 0 }})
	if err := mc.UpdateQuotaMetrics(context.Background()); err != nil {
		t.Fatalf("expected nil error (usage error is non-fatal): %v", err)
	}
}

// TestUpdateQuotaMetrics_ReplicationPending exercises the
// replication-pending gauge update.
func TestUpdateQuotaMetrics_ReplicationPending(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}}, nil).
		AnyTimes()
	store.EXPECT().GetObjectCounts(gomock.Any()).Return(map[string]int64{"b1": 5}, nil).AnyTimes()
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).Return(map[string]int64{}, nil).AnyTimes()
	store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).Return(map[string]core.UsageStat{}, nil).AnyTimes()
	store.EXPECT().GetUnderReplicatedObjects(gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100}}, nil).
		AnyTimes()
	storetest.Permissive(store)

	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}), nil)
	mc := metrics.New(metrics.CollectorDeps{Store: store, Usage: usage, BackendNames: []string{"b1"}, ReplicationFactor: func() int { return 2 }})
	if err := mc.UpdateQuotaMetrics(context.Background()); err != nil {
		t.Fatalf("UpdateQuotaMetrics: %v", err)
	}
}

// TestUpdateQuotaMetrics_ReplicationPendingSkippedWhenDisabled asserts
// the under-replicated query is skipped when the closure returns 0.
func TestUpdateQuotaMetrics_ReplicationPendingSkippedWhenDisabled(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}}, nil).
		AnyTimes()
	store.EXPECT().GetObjectCounts(gomock.Any()).Return(map[string]int64{"b1": 5}, nil).AnyTimes()
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).Return(map[string]int64{}, nil).AnyTimes()
	store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).Return(map[string]core.UsageStat{}, nil).AnyTimes()
	storetest.Permissive(store)

	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}), nil)
	mc := metrics.New(metrics.CollectorDeps{Store: store, Usage: usage, BackendNames: []string{"b1"}, ReplicationFactor: func() int { return 0 }})
	if err := mc.UpdateQuotaMetrics(context.Background()); err != nil {
		t.Fatalf("UpdateQuotaMetrics: %v", err)
	}
}

// TestUpdateQuotaMetrics_ReplicationPendingQueryError swallows the
// non-fatal under-replicated query error.
func TestUpdateQuotaMetrics_ReplicationPendingQueryError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}}, nil).
		AnyTimes()
	store.EXPECT().GetObjectCounts(gomock.Any()).Return(map[string]int64{"b1": 5}, nil).AnyTimes()
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).Return(map[string]int64{}, nil).AnyTimes()
	store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).Return(map[string]core.UsageStat{}, nil).AnyTimes()
	store.EXPECT().GetUnderReplicatedObjects(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, errors.New("db error")).
		AnyTimes()
	storetest.Permissive(store)

	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}), nil)
	mc := metrics.New(metrics.CollectorDeps{Store: store, Usage: usage, BackendNames: []string{"b1"}, ReplicationFactor: func() int { return 2 }})
	if err := mc.UpdateQuotaMetrics(context.Background()); err != nil {
		t.Fatalf("expected nil error (under-replicated query error is non-fatal): %v", err)
	}
}

// TestUpdateQuotaMetrics_ReplicationFactorFromManager confirms the
// closure-driven factor lookup works with and without replication
// configured on the manager.
func TestUpdateQuotaMetrics_ReplicationFactorFromManager(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}}, nil).
		AnyTimes()
	store.EXPECT().GetObjectCounts(gomock.Any()).Return(map[string]int64{"b1": 5}, nil).AnyTimes()
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).Return(map[string]int64{}, nil).AnyTimes()
	store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).Return(map[string]core.UsageStat{}, nil).AnyTimes()
	store.EXPECT().GetUnderReplicatedObjects(gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100}}, nil).
		AnyTimes()
	storetest.Permissive(store)

	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Storage: StorageDeps{
			Backends: map[string]backend.ObjectBackend{"b1": newMockBackend()},
			Order:    []string{"b1"},
		},
		Stores: StoreDeps{
			Metadata:  testStoresFromMock(store),
			Dashboard: store,
		},
		Policies: PolicyConfig{
			CacheTTL:        5 * time.Second,
			BackendTimeout:  30 * time.Second,
			RoutingStrategy: config.RoutingPack,
		},
		Operations: OperationalDeps{
			Metrics: store,
		},
	})
	workers := wireWorkersForTest(mgr, store)

	if err := mgr.Runtime().MetricsCollector().UpdateQuotaMetrics(context.Background()); err != nil {
		t.Fatalf("UpdateQuotaMetrics (no repl config): %v", err)
	}

	workers.Replicator.SetConfig(&config.ReplicationConfig{Factor: 2, BatchSize: 50})
	if err := mgr.Runtime().MetricsCollector().UpdateQuotaMetrics(context.Background()); err != nil {
		t.Fatalf("UpdateQuotaMetrics (with repl config): %v", err)
	}
}
