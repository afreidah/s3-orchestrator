// -------------------------------------------------------------------------------
// MetricsCollector Tests - Prometheus Gauge and Counter Updates
//
// Author: Alex Freidah
//
// Tests for RecordOperation (success/error labeling) and UpdateQuotaMetrics
// (quota stats, object counts, multipart counts, monthly usage, and error paths).
// -------------------------------------------------------------------------------

package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// stubMetricsStore returns a Deps mock. Most tests in this file exercise the
// collector's own code path rather than specific store calls, so each states
// only the reads it depends on.
func stubMetricsStore(t *testing.T) *MockDeps {
	t.Helper()
	store := NewMockDeps(gomock.NewController(t))
	return store
}

// noReplication is the ReplicationFactor of a fleet with replication off.
func noReplication() int { return 0 }

// twoReplicas is the ReplicationFactor of a fleet keeping two copies.
func twoReplicas() int { return 2 }

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestRecordOperation_Success exercises the success label path.
func TestRecordOperation_Success(t *testing.T) {
	t.Parallel()
	store := stubMetricsStore(t)
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil)
	mc := New(&CollectorDeps{Store: store, Usage: usage, ReplicationFactor: noReplication})
	mc.RecordOperation("PutObject", "b1", time.Now(), nil)
}

// TestRecordOperation_Error exercises the error label path.
func TestRecordOperation_Error(t *testing.T) {
	t.Parallel()
	store := stubMetricsStore(t)
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil)
	mc := New(&CollectorDeps{Store: store, Usage: usage, ReplicationFactor: noReplication})
	mc.RecordOperation("GetObject", "b1", time.Now(), errors.New("backend down"))
}

// TestUpdateQuotaMetrics_Success drives every store call returning a
// non-empty result.
func TestUpdateQuotaMetrics_Success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockDeps(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{
			"b1": {BytesUsed: 500, BytesLimit: 1000},
			"b2": {BytesUsed: 0, BytesLimit: 0},
		}, nil).
		AnyTimes()
	store.EXPECT().LedgerStats(gomock.Any()).
		Return(core.LedgerStats{"b1": {Objects: 42}}, nil).
		AnyTimes()
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).
		Return(map[string]int64{"b1": 3}, nil).
		AnyTimes()
	store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).
		Return(map[string]core.WorkloadUsage{"b1": {"client": {
			UsageStat: core.UsageStat{APIRequests: 100, EgressBytes: 5000, IngressBytes: 2000},
			Pools:     core.PoolUsage{core.PoolAll: 100},
		}}}, nil).
		AnyTimes()

	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1", "b2"}), nil)
	mc := New(&CollectorDeps{Store: store, Usage: usage, ReplicationFactor: noReplication})

	if err := mc.UpdateQuotaMetrics(context.Background()); err != nil {
		t.Fatalf("UpdateQuotaMetrics: %v", err)
	}
}

// TestUpdateQuotaMetrics_PublishesPoolGauges pins the pair an operator reads
// to answer "which budget is about to refuse work": the count and the ceiling
// it is judged against, per pool. The ceiling is published rather than assumed
// so a dashboard does not have to carry a copy of the config.
func TestUpdateQuotaMetrics_PublishesPoolGauges(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockDeps(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{"b1": {BytesUsed: 1, BytesLimit: 1000}}, nil).AnyTimes()
	store.EXPECT().LedgerStats(gomock.Any()).Return(core.LedgerStats{}, nil).AnyTimes()
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).Return(map[string]int64{}, nil).AnyTimes()
	store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).
		Return(map[string]core.WorkloadUsage{"b1": {"client": {
			UsageStat: core.UsageStat{APIRequests: 40},
			Pools:     core.PoolUsage{"class_a": 30},
		}}}, nil).AnyTimes()

	lim, err := core.NewUsageLimits(0, 0, []core.PoolSpec{
		{Name: "class_a", Operations: []string{string(s3op.PutObject)}, Limit: 5000},
		{Name: "class_b", Operations: []string{string(s3op.GetObject)}, Limit: 50000},
	}, nil)
	if err != nil {
		t.Fatalf("build limits: %v", err)
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}), map[string]core.UsageLimits{"b1": lim})
	mc := New(&CollectorDeps{Store: store, Usage: usage, ReplicationFactor: noReplication})

	if err := mc.UpdateQuotaMetrics(context.Background()); err != nil {
		t.Fatalf("UpdateQuotaMetrics: %v", err)
	}

	if got := testutil.ToFloat64(telemetry.UsagePoolRequests.WithLabelValues("b1", "class_a")); got != 30 {
		t.Errorf("class_a requests gauge = %v, want 30", got)
	}
	if got := testutil.ToFloat64(telemetry.UsagePoolLimit.WithLabelValues("b1", "class_a")); got != 5000 {
		t.Errorf("class_a limit gauge = %v, want 5000", got)
	}
	// A pool nothing charged still publishes, at zero: a budget missing from
	// the dashboard reads as "not configured", which is a different answer.
	if got := testutil.ToFloat64(telemetry.UsagePoolRequests.WithLabelValues("b1", "class_b")); got != 0 {
		t.Errorf("class_b requests gauge = %v, want 0", got)
	}
	if got := testutil.ToFloat64(telemetry.UsagePoolLimit.WithLabelValues("b1", "class_b")); got != 50000 {
		t.Errorf("class_b limit gauge = %v, want 50000", got)
	}
}

// TestUpdateQuotaMetrics_SeedsBaselinesFromJoinedUsage verifies the pools read
// alongside the totals become the admission baseline, and that a later read
// with no rows, as at the start of a month, resets every baseline to zero.
func TestUpdateQuotaMetrics_SeedsBaselinesFromJoinedUsage(t *testing.T) {
	t.Parallel()
	const name = "seeded"
	lim, err := core.NewUsageLimits(0, 0, []core.PoolSpec{
		{Name: "writes", Operations: []string{string(s3op.PutObject)}, Limit: 30},
	}, nil)
	if err != nil {
		t.Fatalf("build limits: %v", err)
	}
	spent := map[string]core.WorkloadUsage{name: {"client": {
		UsageStat: core.UsageStat{APIRequests: 30},
		Pools:     core.PoolUsage{"writes": 30},
	}}}
	store := NewMockDeps(gomock.NewController(t))
	store.EXPECT().GetQuotaStats(gomock.Any()).Return(map[string]core.QuotaStat{}, nil).AnyTimes()
	store.EXPECT().LedgerStats(gomock.Any()).Return(core.LedgerStats{}, nil).AnyTimes()
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).Return(map[string]int64{}, nil).AnyTimes()
	gomock.InOrder(
		store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).Return(spent, nil),
		store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).Return(map[string]core.WorkloadUsage{}, nil),
	)

	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{name}), map[string]core.UsageLimits{name: lim})
	mc := New(&CollectorDeps{Store: store, Usage: usage, ReplicationFactor: noReplication})
	put := []s3op.Operation{s3op.PutObject}

	if err := mc.UpdateQuotaMetrics(context.Background()); err != nil {
		t.Fatalf("UpdateQuotaMetrics: %v", err)
	}
	if usage.WithinLimits(name, put, 0, 0) {
		t.Error("a PUT was admitted against a pool the stored rows show as spent")
	}

	if err := mc.UpdateQuotaMetrics(context.Background()); err != nil {
		t.Fatalf("UpdateQuotaMetrics: %v", err)
	}
	if !usage.WithinLimits(name, put, 0, 0) {
		t.Error("a PUT was refused after the period's rows came back empty")
	}
}

// TestUpdateQuotaMetrics_CapacityWarning exercises the capacity-warning
// branch.
func TestUpdateQuotaMetrics_CapacityWarning(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockDeps(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{
			"b1": {BytesUsed: 900, BytesLimit: 1000},
			"b2": {BytesUsed: 500, BytesLimit: 1000},
			"b3": {BytesUsed: 800, BytesLimit: 1000, OrphanBytes: 50},
		}, nil).
		AnyTimes()
	store.EXPECT().LedgerStats(gomock.Any()).Return(core.LedgerStats{}, nil).AnyTimes()
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).Return(map[string]int64{}, nil).AnyTimes()
	store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).Return(map[string]core.WorkloadUsage{}, nil).AnyTimes()

	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1", "b2", "b3"}), nil)
	mc := New(&CollectorDeps{Store: store, Usage: usage, ReplicationFactor: noReplication})
	if err := mc.UpdateQuotaMetrics(context.Background()); err != nil {
		t.Fatalf("UpdateQuotaMetrics: %v", err)
	}
}

// TestUpdateQuotaMetrics_QuotaStatsError surfaces the fatal quota-stats
// failure.
func TestUpdateQuotaMetrics_QuotaStatsError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockDeps(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).Return(nil, errors.New("db down")).AnyTimes()

	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil)
	mc := New(&CollectorDeps{Store: store, Usage: usage, ReplicationFactor: noReplication})
	if err := mc.UpdateQuotaMetrics(context.Background()); err == nil {
		t.Fatal("expected error from GetQuotaStats failure")
	}
}

// TestUpdateQuotaMetrics_ObjectCountsError swallows the non-fatal
// object-counts error.
func TestUpdateQuotaMetrics_ObjectCountsError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockDeps(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}}, nil).
		AnyTimes()
	store.EXPECT().LedgerStats(gomock.Any()).Return(nil, errors.New("db error")).AnyTimes()
	// The collector keeps gathering the rest even after one read fails.
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).Return(map[string]int64{}, nil).AnyTimes()
	store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).Return(map[string]core.WorkloadUsage{}, nil).AnyTimes()

	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}), nil)
	mc := New(&CollectorDeps{Store: store, Usage: usage, ReplicationFactor: noReplication})
	if err := mc.UpdateQuotaMetrics(context.Background()); err != nil {
		t.Fatalf("expected nil error (object counts error is non-fatal): %v", err)
	}
}

// TestUpdateQuotaMetrics_MultipartCountsError swallows the non-fatal
// multipart-counts error.
func TestUpdateQuotaMetrics_MultipartCountsError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockDeps(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}}, nil).
		AnyTimes()
	store.EXPECT().LedgerStats(gomock.Any()).Return(core.LedgerStats{"b1": {Objects: 5}}, nil).AnyTimes()
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).Return(nil, errors.New("db error")).AnyTimes()
	store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).Return(map[string]core.WorkloadUsage{}, nil).AnyTimes()

	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}), nil)
	mc := New(&CollectorDeps{Store: store, Usage: usage, ReplicationFactor: noReplication})
	if err := mc.UpdateQuotaMetrics(context.Background()); err != nil {
		t.Fatalf("expected nil error (multipart counts error is non-fatal): %v", err)
	}
}

// TestUpdateQuotaMetrics_UsageForPeriodError swallows the non-fatal
// usage-for-period error and keeps the baselines already loaded, so a failed
// read does not reopen budgets that are spent.
func TestUpdateQuotaMetrics_UsageForPeriodError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockDeps(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}}, nil).
		AnyTimes()
	store.EXPECT().LedgerStats(gomock.Any()).Return(core.LedgerStats{"b1": {Objects: 5}}, nil).AnyTimes()
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).Return(map[string]int64{}, nil).AnyTimes()
	store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).Return(nil, errors.New("db error")).AnyTimes()

	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}),
		map[string]core.UsageLimits{"b1": {EgressByteLimit: 100}})
	usage.SetBaselines(map[string]core.Usage{"b1": {UsageStat: core.UsageStat{EgressBytes: 100}}})
	mc := New(&CollectorDeps{Store: store, Usage: usage, ReplicationFactor: noReplication})
	if err := mc.UpdateQuotaMetrics(context.Background()); err != nil {
		t.Fatalf("expected nil error (usage error is non-fatal): %v", err)
	}
	if usage.WithinLimits("b1", []s3op.Operation{s3op.GetObject}, 1, 0) {
		t.Error("a failed usage read cleared the spent egress baseline")
	}
}

// TestUpdateQuotaMetrics_ReplicationPending exercises the
// replication-pending gauge update.
func TestUpdateQuotaMetrics_ReplicationPending(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockDeps(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}}, nil).
		AnyTimes()
	store.EXPECT().LedgerStats(gomock.Any()).Return(core.LedgerStats{"b1": {Objects: 5}}, nil).AnyTimes()
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).Return(map[string]int64{}, nil).AnyTimes()
	store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).Return(map[string]core.WorkloadUsage{}, nil).AnyTimes()
	store.EXPECT().CountReplicationBacklog(gomock.Any(), gomock.Any()).
		Return(core.ReplicationBacklog{Under: 1}, nil).
		AnyTimes()

	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}), nil)
	mc := New(&CollectorDeps{Store: store, Usage: usage, ReplicationFactor: twoReplicas})
	if err := mc.UpdateQuotaMetrics(context.Background()); err != nil {
		t.Fatalf("UpdateQuotaMetrics: %v", err)
	}
}

// TestUpdateQuotaMetrics_ReplicationPendingSkippedWhenDisabled asserts
// the under-replicated query is skipped when the closure returns 0.
func TestUpdateQuotaMetrics_ReplicationPendingSkippedWhenDisabled(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockDeps(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}}, nil).
		AnyTimes()
	store.EXPECT().LedgerStats(gomock.Any()).Return(core.LedgerStats{"b1": {Objects: 5}}, nil).AnyTimes()
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).Return(map[string]int64{}, nil).AnyTimes()
	store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).Return(map[string]core.WorkloadUsage{}, nil).AnyTimes()

	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}), nil)
	mc := New(&CollectorDeps{Store: store, Usage: usage, ReplicationFactor: noReplication})
	if err := mc.UpdateQuotaMetrics(context.Background()); err != nil {
		t.Fatalf("UpdateQuotaMetrics: %v", err)
	}
}

// TestUpdateQuotaMetrics_ReplicationPendingQueryError swallows the
// non-fatal under-replicated query error.
func TestUpdateQuotaMetrics_ReplicationPendingQueryError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockDeps(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}}, nil).
		AnyTimes()
	store.EXPECT().LedgerStats(gomock.Any()).Return(core.LedgerStats{"b1": {Objects: 5}}, nil).AnyTimes()
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).Return(map[string]int64{}, nil).AnyTimes()
	store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).Return(map[string]core.WorkloadUsage{}, nil).AnyTimes()
	store.EXPECT().CountReplicationBacklog(gomock.Any(), gomock.Any()).
		Return(core.ReplicationBacklog{}, errors.New("db error")).
		AnyTimes()

	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}), nil)
	mc := New(&CollectorDeps{Store: store, Usage: usage, ReplicationFactor: twoReplicas})
	if err := mc.UpdateQuotaMetrics(context.Background()); err != nil {
		t.Fatalf("expected nil error (under-replicated query error is non-fatal): %v", err)
	}
}

// TestMetricsCollector_OrphanBytesSubtractedFromAvailable confirms the
// metrics-collector reads the OrphanBytes field without panicking.
func TestMetricsCollector_OrphanBytesSubtractedFromAvailable(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{
			"b1": {BytesUsed: 200, BytesLimit: 1000, OrphanBytes: 100},
		}, nil).
		AnyTimes()
	storetest.Permissive(store)

	mc := New(&CollectorDeps{Store: store, Usage: counter.NewUsageTracker(nil, nil), ReplicationFactor: noReplication})
	if err := mc.UpdateQuotaMetrics(context.Background()); err != nil {
		t.Fatalf("UpdateQuotaMetrics: %v", err)
	}
}
