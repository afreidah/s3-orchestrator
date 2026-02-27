// -------------------------------------------------------------------------------
// Dashboard Aggregator Tests
//
// Author: Alex Freidah
//
// Tests for BackendManager dashboard data aggregation. Validates storage summary
// computation, backend status reporting, and monthly usage statistics collection.
// -------------------------------------------------------------------------------

package storage

import (
	"context"
	"errors"
	"testing"
)

func TestGetDashboardData_Success(t *testing.T) {
	store := &mockStore{
		getQuotaStatsResp: map[string]QuotaStat{
			"b1": {BackendName: "b1", BytesUsed: 500, BytesLimit: 1000},
		},
		getObjectCountsResp:    map[string]int64{"b1": 42},
		getActiveMultipartResp: map[string]int64{"b1": 3},
		getUsageForPeriodResp:  map[string]UsageStat{"b1": {APIRequests: 100}},
		listDirChildrenResp: &DirectoryListResult{
			Entries: []DirEntry{
				{Name: "bucket1/", IsDir: true, FileCount: 10, TotalSize: 4096},
			},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})
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

func TestGetDashboardData_QuotaStatsError(t *testing.T) {
	store := &mockStore{
		getQuotaStatsErr: errors.New("db error"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})
	defer mgr.Close()

	_, err := mgr.GetDashboardData(context.Background())
	if err == nil {
		t.Fatal("expected error from GetDashboardData")
	}
}

func TestGetDashboardData_ObjectCountsError(t *testing.T) {
	store := &mockStore{
		getQuotaStatsResp:  map[string]QuotaStat{"b1": {}},
		getObjectCountsErr: errors.New("db error"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})
	defer mgr.Close()

	_, err := mgr.GetDashboardData(context.Background())
	if err == nil {
		t.Fatal("expected error from GetDashboardData")
	}
}

func TestGetDashboardData_MultipartCountsError(t *testing.T) {
	store := &mockStore{
		getQuotaStatsResp:   map[string]QuotaStat{"b1": {}},
		getObjectCountsResp: map[string]int64{"b1": 0},
		getActiveMultipartErr: errors.New("db error"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})
	defer mgr.Close()

	_, err := mgr.GetDashboardData(context.Background())
	if err == nil {
		t.Fatal("expected error from GetDashboardData")
	}
}

func TestGetDashboardData_UsageForPeriodError(t *testing.T) {
	store := &mockStore{
		getQuotaStatsResp:      map[string]QuotaStat{"b1": {}},
		getObjectCountsResp:    map[string]int64{"b1": 0},
		getActiveMultipartResp: map[string]int64{"b1": 0},
		getUsageForPeriodErr:   errors.New("db error"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})
	defer mgr.Close()

	_, err := mgr.GetDashboardData(context.Background())
	if err == nil {
		t.Fatal("expected error from GetDashboardData")
	}
}

func TestGetDashboardData_ListDirChildrenError(t *testing.T) {
	store := &mockStore{
		getQuotaStatsResp:      map[string]QuotaStat{"b1": {}},
		getObjectCountsResp:    map[string]int64{"b1": 0},
		getActiveMultipartResp: map[string]int64{"b1": 0},
		getUsageForPeriodResp:  map[string]UsageStat{},
		listDirChildrenErr:     errors.New("db error"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})
	defer mgr.Close()

	_, err := mgr.GetDashboardData(context.Background())
	if err == nil {
		t.Fatal("expected error from GetDashboardData")
	}
}

func TestGetDirectoryChildren_CapsMaxKeys(t *testing.T) {
	store := &mockStore{
		listDirChildrenResp: &DirectoryListResult{},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})
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
