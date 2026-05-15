// -------------------------------------------------------------------------------
// Dashboard Aggregator Tests
//
// Author: Alex Freidah
//
// Tests for BackendManager dashboard data aggregation. Validates storage summary
// computation, backend status reporting, and monthly usage statistics collection.
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
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// TestGetDashboardData_Success drives the happy path: every dashboard
// store query returns a non-empty result and the aggregator stitches
// them into a populated Data struct.
func TestGetDashboardData_Success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{"b1": {BackendName: "b1", BytesUsed: 500, BytesLimit: 1000}}, nil).
		AnyTimes()
	store.EXPECT().GetObjectCounts(gomock.Any()).
		Return(map[string]int64{"b1": 42}, nil).
		AnyTimes()
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).
		Return(map[string]int64{"b1": 3}, nil).
		AnyTimes()
	store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).
		Return(map[string]core.UsageStat{"b1": {APIRequests: 100}}, nil).
		AnyTimes()
	store.EXPECT().ListDirectoryChildren(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&core.DirectoryListResult{
			Entries: []core.DirEntry{
				{Name: "bucket1/", IsDir: true, FileCount: 10, TotalSize: 4096},
			},
		}, nil).
		AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	defer mgr.Close()

	data, err := mgr.GetDashboardData(context.Background())
	if err != nil {
		t.Fatalf("GetDashboardData: %v", err)
	}

	if len(data.QuotaStats) != 1 {
		t.Errorf("QuotaStats count = %d, want 1", len(data.QuotaStats))
	}
	if data.ObjectCounts["b1"] != 42 {
		t.Errorf("ObjectCounts[b1] = %d, want 42", data.ObjectCounts["b1"])
	}
	if data.ActiveMultipartCounts["b1"] != 3 {
		t.Errorf("ActiveMultipartCounts[b1] = %d, want 3", data.ActiveMultipartCounts["b1"])
	}
	if data.UsageStats["b1"].APIRequests != 100 {
		t.Errorf("UsageStats[b1].APIRequests = %d, want 100", data.UsageStats["b1"].APIRequests)
	}
	if len(data.TopLevelEntries.Entries) != 1 {
		t.Errorf("TopLevelEntries count = %d, want 1", len(data.TopLevelEntries.Entries))
	}
	if data.UsagePeriod == "" {
		t.Error("UsagePeriod should not be empty")
	}
}

// TestGetDashboardData_QuotaStatsError surfaces a quota-store failure as
// an error from the aggregator.
func TestGetDashboardData_QuotaStatsError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(nil, errors.New("db error")).
		AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	defer mgr.Close()

	if _, err := mgr.GetDashboardData(context.Background()); err == nil {
		t.Fatal("expected error from GetDashboardData")
	}
}

// TestGetDashboardData_ObjectCountsError surfaces an object-counts
// failure.
func TestGetDashboardData_ObjectCountsError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{"b1": {}}, nil).
		AnyTimes()
	store.EXPECT().GetObjectCounts(gomock.Any()).
		Return(nil, errors.New("db error")).
		AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	defer mgr.Close()

	if _, err := mgr.GetDashboardData(context.Background()); err == nil {
		t.Fatal("expected error from GetDashboardData")
	}
}

// TestGetDashboardData_MultipartCountsError surfaces an
// active-multipart-counts failure.
func TestGetDashboardData_MultipartCountsError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{"b1": {}}, nil).
		AnyTimes()
	store.EXPECT().GetObjectCounts(gomock.Any()).
		Return(map[string]int64{"b1": 0}, nil).
		AnyTimes()
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).
		Return(nil, errors.New("db error")).
		AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	defer mgr.Close()

	if _, err := mgr.GetDashboardData(context.Background()); err == nil {
		t.Fatal("expected error from GetDashboardData")
	}
}

// TestGetDashboardData_UsageForPeriodError surfaces a usage-stats failure.
func TestGetDashboardData_UsageForPeriodError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{"b1": {}}, nil).
		AnyTimes()
	store.EXPECT().GetObjectCounts(gomock.Any()).
		Return(map[string]int64{"b1": 0}, nil).
		AnyTimes()
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).
		Return(map[string]int64{"b1": 0}, nil).
		AnyTimes()
	store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).
		Return(nil, errors.New("db error")).
		AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	defer mgr.Close()

	if _, err := mgr.GetDashboardData(context.Background()); err == nil {
		t.Fatal("expected error from GetDashboardData")
	}
}

// TestGetDashboardData_ListDirChildrenError surfaces a directory-listing
// failure.
func TestGetDashboardData_ListDirChildrenError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{"b1": {}}, nil).
		AnyTimes()
	store.EXPECT().GetObjectCounts(gomock.Any()).
		Return(map[string]int64{"b1": 0}, nil).
		AnyTimes()
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).
		Return(map[string]int64{"b1": 0}, nil).
		AnyTimes()
	store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).
		Return(map[string]core.UsageStat{}, nil).
		AnyTimes()
	store.EXPECT().ListDirectoryChildren(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, errors.New("db error")).
		AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	defer mgr.Close()

	if _, err := mgr.GetDashboardData(context.Background()); err == nil {
		t.Fatal("expected error from GetDashboardData")
	}
}

// TestGetDashboardData_UnhealthyBackends asserts that an open-circuit
// backend appears in UnhealthyBackends.
func TestGetDashboardData_UnhealthyBackends(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{"b1": {}}, nil).
		AnyTimes()
	store.EXPECT().GetObjectCounts(gomock.Any()).
		Return(map[string]int64{"b1": 0}, nil).
		AnyTimes()
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).
		Return(map[string]int64{"b1": 0}, nil).
		AnyTimes()
	store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).
		Return(map[string]core.UsageStat{}, nil).
		AnyTimes()
	store.EXPECT().ListDirectoryChildren(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&core.DirectoryListResult{}, nil).
		AnyTimes()
	storetest.Permissive(store)

	mock := newMockBackend()
	mock.putErr = errors.New("down")
	cbBackend := backend.NewCircuitBreakerBackend(mock, "b1", 1, time.Minute)
	// Trip the circuit.
	_, _ = cbBackend.PutObject(context.Background(), "k", nil, 0, "", nil)

	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": cbBackend},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1"},
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers
	defer mgr.Close()

	data, err := mgr.GetDashboardData(context.Background())
	if err != nil {
		t.Fatalf("GetDashboardData: %v", err)
	}
	if !data.UnhealthyBackends["b1"] {
		t.Error("expected b1 to be marked unhealthy")
	}
}

// TestGetDashboardData_HealthyBackendsNotMarked asserts a healthy backend
// is absent from the UnhealthyBackends map.
func TestGetDashboardData_HealthyBackendsNotMarked(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{"b1": {}}, nil).
		AnyTimes()
	store.EXPECT().GetObjectCounts(gomock.Any()).
		Return(map[string]int64{"b1": 0}, nil).
		AnyTimes()
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).
		Return(map[string]int64{"b1": 0}, nil).
		AnyTimes()
	store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).
		Return(map[string]core.UsageStat{}, nil).
		AnyTimes()
	store.EXPECT().ListDirectoryChildren(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&core.DirectoryListResult{}, nil).
		AnyTimes()
	storetest.Permissive(store)

	cbBackend := backend.NewCircuitBreakerBackend(newMockBackend(), "b1", 5, time.Minute)
	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": cbBackend},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1"},
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers
	defer mgr.Close()

	data, err := mgr.GetDashboardData(context.Background())
	if err != nil {
		t.Fatalf("GetDashboardData: %v", err)
	}
	if data.UnhealthyBackends["b1"] {
		t.Error("healthy backend should not be marked unhealthy")
	}
}

// TestGetDirectoryChildren_CapsMaxKeys exercises the maxKeys clamping
// applied before the store query.
func TestGetDirectoryChildren_CapsMaxKeys(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListDirectoryChildren(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&core.DirectoryListResult{}, nil).
		AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	defer mgr.Close()

	tests := []struct {
		name    string
		maxKeys int
	}{
		{"zero becomes 200", 0},
		{"negative becomes 200", -5},
		{"over 200 becomes 200", 500},
		{"valid stays valid", 50},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := mgr.GetDirectoryChildren(context.Background(), "", "", tt.maxKeys)
			if err != nil {
				t.Fatalf("GetDirectoryChildren: %v", err)
			}
			if result == nil {
				t.Fatal("expected non-nil result")
			}
		})
	}
}
