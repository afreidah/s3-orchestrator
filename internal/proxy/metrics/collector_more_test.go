// -------------------------------------------------------------------------------
// MetricsCollector Tests - Prometheus Gauge and Counter Updates
//
// Author: Alex Freidah
//
// Tests for RefreshGauges: the fleet figures, monthly usage and limits, the
// usage baselines limit checks compare against, and the error paths.
// -------------------------------------------------------------------------------

package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
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

// stubFleetReads answers every fleet read on store with stats and ledger, each
// queue count with its own depth from 1 to 6, and empty results otherwise, with
// the capacity lock free. Expectations a test set first take precedence, so it
// stubs only the reads it cares about.
func stubFleetReads(store *MockDeps, stats map[string]core.QuotaStat, ledger core.LedgerStats) {
	a := gomock.Any()
	store.EXPECT().PendingDepth(a).Return(int64(1), nil).AnyTimes()
	store.EXPECT().CleanupQueueDepth(a).Return(int64(2), nil).AnyTimes()
	store.EXPECT().CleanupDLQDepth(a).Return(int64(3), nil).AnyTimes()
	store.EXPECT().NotificationQueueDepth(a).Return(int64(4), nil).AnyTimes()
	store.EXPECT().CountReplicationQueue(a).Return(core.ReplicationBacklog{Under: 5, Over: 6}, nil).AnyTimes()
	store.EXPECT().GetQuotaStats(a).Return(stats, nil).AnyTimes()
	store.EXPECT().LedgerStats(a).Return(ledger, nil).AnyTimes()
	store.EXPECT().GetActiveMultipartCounts(a).Return(map[string]int64{}, nil).AnyTimes()
	store.EXPECT().GetLeastRecentlyScrubbedObjects(a, a, a, a).Return(nil, nil).AnyTimes()
	store.EXPECT().GetUsageSince(a, a).Return(map[string]core.WorkloadUsage{}, nil).AnyTimes()
	store.EXPECT().WithAdvisoryLock(a, a, a).DoAndReturn(
		func(ctx context.Context, _ int64, fn func(context.Context) error) (bool, error) {
			return true, fn(ctx)
		}).AnyTimes()
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestRefreshGauges_PublishesTheFleet verifies a refresh with no quota read of
// its own reads one and publishes the per-backend figures, zeroing a backend
// the ledger no longer lists, and each queue's depth to its own gauge.
func TestRefreshGauges_PublishesTheFleet(t *testing.T) {
	store := NewMockDeps(gomock.NewController(t))
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).Return(map[string]int64{"b1": 3}, nil).AnyTimes()
	stubFleetReads(store, map[string]core.QuotaStat{
		"b1": {BytesUsed: 500, BytesLimit: 1000},
		"b2": {BytesUsed: 0, BytesLimit: 0},
	}, core.LedgerStats{"b1": {Objects: 42}})
	telemetry.ObjectCount.WithLabelValues("b2").Set(7)

	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1", "b2"}), nil)
	if err := testCollector(CollectorDeps{Store: store, Usage: usage}).RefreshGauges(context.Background(), nil); err != nil {
		t.Fatalf("RefreshGauges: %v", err)
	}

	if got := testutil.ToFloat64(telemetry.ObjectCount.WithLabelValues("b1")); got != 42 {
		t.Errorf("b1 objects gauge = %v, want 42", got)
	}
	if got := testutil.ToFloat64(telemetry.ObjectCount.WithLabelValues("b2")); got != 0 {
		t.Errorf("b2 objects gauge = %v, want 0 once the ledger lists none", got)
	}
	if got := testutil.ToFloat64(telemetry.ActiveMultipartUploads.WithLabelValues("b1")); got != 3 {
		t.Errorf("b1 multipart gauge = %v, want 3", got)
	}
	for gauge, want := range map[prometheus.Gauge]float64{
		telemetry.PendingIntentsDepth: 1, telemetry.CleanupQueueDepth: 2, telemetry.CleanupDLQDepth: 3,
		telemetry.NotificationQueueDepth: 4, telemetry.ReplicationPending: 5, telemetry.OverReplicationPending: 6,
	} {
		if got := testutil.ToFloat64(gauge); got != want {
			t.Errorf("%s = %v, want %v", gauge.Desc(), got, want)
		}
	}
}

// TestRefreshGauges_PublishesLimitGauges pins what an operator reads to answer
// "which limit is about to refuse work": each request budget's count and
// ceiling, and the egress and ingress limits the byte gauges are judged
// against. Limits are published rather than assumed so a dashboard does not
// have to carry a copy of the config; an unlimited one reads 0.
func TestRefreshGauges_PublishesLimitGauges(t *testing.T) {
	store := NewMockDeps(gomock.NewController(t))
	store.EXPECT().GetUsageSince(gomock.Any(), gomock.Any()).
		Return(map[string]core.WorkloadUsage{"b1": {"client": {
			UsageStat: core.UsageStat{APIRequests: 40},
			Pools:     core.PoolUsage{"class_a": 30},
		}}}, nil).AnyTimes()
	stubFleetReads(store, map[string]core.QuotaStat{"b1": {BytesUsed: 1, BytesLimit: 1000}}, core.LedgerStats{})

	lim, err := core.NewUsageLimits(7000, 0, []core.PoolSpec{
		{Name: "class_a", Operations: []string{string(s3op.PutObject)}, Limit: 5000},
		{Name: "class_b", Operations: []string{string(s3op.GetObject)}, Limit: 50000},
	}, nil)
	if err != nil {
		t.Fatalf("build limits: %v", err)
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}), map[string]core.UsageLimits{"b1": lim})
	if err := testCollector(CollectorDeps{Store: store, Usage: usage}).RefreshGauges(context.Background(), nil); err != nil {
		t.Fatalf("RefreshGauges: %v", err)
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
	if got := testutil.ToFloat64(telemetry.UsageEgressLimitBytes.WithLabelValues("b1")); got != 7000 {
		t.Errorf("egress limit gauge = %v, want 7000", got)
	}
	if got := testutil.ToFloat64(telemetry.UsageIngressLimitBytes.WithLabelValues("b1")); got != 0 {
		t.Errorf("ingress limit gauge = %v, want 0 for an unlimited backend", got)
	}
	if got, want := testutil.ToFloat64(telemetry.UsageCycleResetTimestamp.WithLabelValues("b1")), float64(lim.Period.Cycle(time.Now()).Next.Unix()); got != want {
		t.Errorf("cycle reset gauge = %v, want %v", got, want)
	}
}

// TestRefreshGauges_SeedsBaselinesFromJoinedUsage verifies the pools read
// alongside the totals become the admission baseline, and that a later read
// with no rows, as at the start of a month, resets every baseline to zero.
func TestRefreshGauges_SeedsBaselinesFromJoinedUsage(t *testing.T) {
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
	gomock.InOrder(
		store.EXPECT().GetUsageSince(gomock.Any(), gomock.Any()).Return(spent, nil),
		store.EXPECT().GetUsageSince(gomock.Any(), gomock.Any()).Return(map[string]core.WorkloadUsage{}, nil),
	)
	stubFleetReads(store, map[string]core.QuotaStat{}, core.LedgerStats{})

	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{name}), map[string]core.UsageLimits{name: lim})
	mc := testCollector(CollectorDeps{Store: store, Usage: usage})

	if err := mc.RefreshGauges(context.Background(), nil); err != nil {
		t.Fatalf("RefreshGauges: %v", err)
	}
	if usage.WithinLimits(name, s3op.PutObject, 0, 0) {
		t.Error("a PUT was admitted against a pool the stored rows show as spent")
	}

	if err := mc.RefreshGauges(context.Background(), nil); err != nil {
		t.Fatalf("RefreshGauges: %v", err)
	}
	if !usage.WithinLimits(name, s3op.PutObject, 0, 0) {
		t.Error("a PUT was refused after the period's rows came back empty")
	}
}

// TestRefreshGauges_QuotaStatsError surfaces a failed quota read when the
// caller made none.
func TestRefreshGauges_QuotaStatsError(t *testing.T) {
	t.Parallel()
	store := NewMockDeps(gomock.NewController(t))
	store.EXPECT().GetQuotaStats(gomock.Any()).Return(nil, errors.New("db down")).AnyTimes()

	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil)
	if err := testCollector(CollectorDeps{Store: store, Usage: usage}).RefreshGauges(context.Background(), nil); err == nil {
		t.Fatal("expected error from GetQuotaStats failure")
	}
}

// TestRefreshGauges_FleetReadErrors verifies a failed fleet read is returned
// while the usage baselines are still reloaded, since limit checks depend on
// them whatever the gauges show.
func TestRefreshGauges_FleetReadErrors(t *testing.T) {
	t.Parallel()
	a := gomock.Any()
	boom := errors.New("db error")
	for _, tc := range []struct {
		name string
		fail func(*MockDeps)
	}{
		{"ledger", func(m *MockDeps) { m.EXPECT().LedgerStats(a).Return(nil, boom).AnyTimes() }},
		{"multipart", func(m *MockDeps) { m.EXPECT().GetActiveMultipartCounts(a).Return(nil, boom).AnyTimes() }},
		{"scrub head", func(m *MockDeps) { m.EXPECT().GetLeastRecentlyScrubbedObjects(a, a, a, a).Return(nil, boom).AnyTimes() }},
		{"queue depth", func(m *MockDeps) { m.EXPECT().PendingDepth(a).Return(int64(0), boom).AnyTimes() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := NewMockDeps(gomock.NewController(t))
			tc.fail(store)
			store.EXPECT().GetUsageSince(a, a).
				Return(map[string]core.WorkloadUsage{"b1": {"client": {UsageStat: core.UsageStat{EgressBytes: 100}}}}, nil).Times(1)
			stubFleetReads(store, map[string]core.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}}, core.LedgerStats{})

			usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}),
				map[string]core.UsageLimits{"b1": {EgressByteLimit: 100}})
			if err := testCollector(CollectorDeps{Store: store, Usage: usage}).RefreshGauges(context.Background(), nil); !errors.Is(err, boom) {
				t.Errorf("RefreshGauges = %v, want the %s failure", err, tc.name)
			}
			if usage.WithinLimits("b1", s3op.GetObject, 1, 0) {
				t.Error("the usage baseline was not reloaded after the fleet read failed")
			}
		})
	}
}

// TestRefreshGauges_UsageForPeriodError swallows the non-fatal usage read error
// and keeps the baselines already loaded, so a failed read does not reopen
// budgets that are spent.
func TestRefreshGauges_UsageForPeriodError(t *testing.T) {
	t.Parallel()
	store := NewMockDeps(gomock.NewController(t))
	store.EXPECT().GetUsageSince(gomock.Any(), gomock.Any()).Return(nil, errors.New("db error")).AnyTimes()
	stubFleetReads(store, map[string]core.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}}, core.LedgerStats{"b1": {Objects: 5}})

	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}),
		map[string]core.UsageLimits{"b1": {EgressByteLimit: 100}})
	usage.SetBaselines(map[string]core.Usage{"b1": {UsageStat: core.UsageStat{EgressBytes: 100}}})
	if err := testCollector(CollectorDeps{Store: store, Usage: usage}).RefreshGauges(context.Background(), nil); err != nil {
		t.Fatalf("expected nil error (usage error is non-fatal): %v", err)
	}
	if usage.WithinLimits("b1", s3op.GetObject, 1, 0) {
		t.Error("a failed usage read cleared the spent egress baseline")
	}
}

// TestMetricsCollector_OrphanBytesSubtractedFromAvailable verifies the
// available-bytes gauge leaves room for the orphans still to delete.
func TestMetricsCollector_OrphanBytesSubtractedFromAvailable(t *testing.T) {
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	storetest.Permissive(store)

	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"orphaned"}), nil)
	stats := map[string]core.QuotaStat{"orphaned": {BytesUsed: 200, BytesLimit: 1000, OrphanBytes: 100}}
	if err := testCollector(CollectorDeps{Store: store, Usage: usage}).RefreshGauges(context.Background(), stats); err != nil {
		t.Fatalf("RefreshGauges: %v", err)
	}
	if got := testutil.ToFloat64(telemetry.QuotaBytesAvailable.WithLabelValues("orphaned")); got != 700 {
		t.Errorf("available gauge = %v, want 700 once the orphans are subtracted", got)
	}
}
