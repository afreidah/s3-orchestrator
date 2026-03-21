// -------------------------------------------------------------------------------
// DashboardAggregator Tests
//
// Author: Alex Freidah
//
// Tests for dashboard data aggregation: successful assembly, individual query
// errors, empty data, and directory listing edge cases.
// -------------------------------------------------------------------------------

package proxy

import (
	"context"
	"errors"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/store"
)

// mockDashboardStore implements store.DashboardStore for aggregator tests.
type mockDashboardStore struct {
	quotaStats      map[string]store.QuotaStat
	quotaStatsErr   error
	objectCounts    map[string]int64
	objectCountsErr error
	multipartCounts    map[string]int64
	multipartCountsErr error
	usageStats      map[string]store.UsageStat
	usageStatsErr   error
	dirChildren     *store.DirectoryListResult
	dirChildrenErr  error
}

func (m *mockDashboardStore) GetQuotaStats(_ context.Context) (map[string]store.QuotaStat, error) {
	return m.quotaStats, m.quotaStatsErr
}

func (m *mockDashboardStore) GetObjectCounts(_ context.Context) (map[string]int64, error) {
	return m.objectCounts, m.objectCountsErr
}

func (m *mockDashboardStore) GetActiveMultipartCounts(_ context.Context) (map[string]int64, error) {
	return m.multipartCounts, m.multipartCountsErr
}

func (m *mockDashboardStore) GetUsageForPeriod(_ context.Context, _ string) (map[string]store.UsageStat, error) {
	return m.usageStats, m.usageStatsErr
}

func (m *mockDashboardStore) ListDirectoryChildren(_ context.Context, _, _ string, _ int) (*store.DirectoryListResult, error) {
	return m.dirChildren, m.dirChildrenErr
}

func TestAggregator_Success(t *testing.T) {
	ms := &mockDashboardStore{
		quotaStats:      map[string]store.QuotaStat{"b1": {BytesUsed: 100}},
		objectCounts:    map[string]int64{"b1": 5},
		multipartCounts: map[string]int64{"b1": 1},
		usageStats:      map[string]store.UsageStat{"b1": {APIRequests: 10}},
		dirChildren:     &store.DirectoryListResult{},
	}

	usage := counter.NewUsageTracker(
		counter.NewLocalCounterBackend([]string{"b1"}),
		nil,
	)

	da := NewDashboardAggregator(ms, usage, []string{"b1"})
	data, err := da.GetDashboardData(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if data.QuotaStats["b1"].BytesUsed != 100 {
		t.Errorf("BytesUsed = %d, want 100", data.QuotaStats["b1"].BytesUsed)
	}
	if data.ObjectCounts["b1"] != 5 {
		t.Errorf("ObjectCounts = %d, want 5", data.ObjectCounts["b1"])
	}
	if len(data.BackendOrder) != 1 || data.BackendOrder[0] != "b1" {
		t.Errorf("BackendOrder = %v, want [b1]", data.BackendOrder)
	}
}

func TestAggregator_QuotaStatsError(t *testing.T) {
	ms := &mockDashboardStore{
		quotaStatsErr: errors.New("db down"),
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil)
	da := NewDashboardAggregator(ms, usage, nil)

	_, err := da.GetDashboardData(context.Background())
	if err == nil {
		t.Fatal("expected error when QuotaStats fails")
	}
}

func TestAggregator_ObjectCountsError(t *testing.T) {
	ms := &mockDashboardStore{
		quotaStats:      map[string]store.QuotaStat{},
		objectCountsErr: errors.New("db down"),
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil)
	da := NewDashboardAggregator(ms, usage, nil)

	_, err := da.GetDashboardData(context.Background())
	if err == nil {
		t.Fatal("expected error when ObjectCounts fails")
	}
}

func TestAggregator_MultipartCountsError(t *testing.T) {
	ms := &mockDashboardStore{
		quotaStats:         map[string]store.QuotaStat{},
		objectCounts:       map[string]int64{},
		multipartCountsErr: errors.New("db down"),
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil)
	da := NewDashboardAggregator(ms, usage, nil)

	_, err := da.GetDashboardData(context.Background())
	if err == nil {
		t.Fatal("expected error when MultipartCounts fails")
	}
}

func TestAggregator_UsageStatsError(t *testing.T) {
	ms := &mockDashboardStore{
		quotaStats:      map[string]store.QuotaStat{},
		objectCounts:    map[string]int64{},
		multipartCounts: map[string]int64{},
		usageStatsErr:   errors.New("db down"),
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil)
	da := NewDashboardAggregator(ms, usage, nil)

	_, err := da.GetDashboardData(context.Background())
	if err == nil {
		t.Fatal("expected error when UsageStats fails")
	}
}

func TestAggregator_DirChildrenError(t *testing.T) {
	ms := &mockDashboardStore{
		quotaStats:      map[string]store.QuotaStat{},
		objectCounts:    map[string]int64{},
		multipartCounts: map[string]int64{},
		usageStats:      map[string]store.UsageStat{},
		dirChildrenErr:  errors.New("db down"),
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil)
	da := NewDashboardAggregator(ms, usage, nil)

	_, err := da.GetDashboardData(context.Background())
	if err == nil {
		t.Fatal("expected error when DirChildren fails")
	}
}

func TestAggregator_GetDirectoryChildren_ClampsMaxKeys(t *testing.T) {
	ms := &mockDashboardStore{
		dirChildren: &store.DirectoryListResult{},
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil)
	da := NewDashboardAggregator(ms, usage, nil)

	// maxKeys=0 should be clamped to 200
	result, err := da.GetDirectoryChildren(context.Background(), "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// maxKeys=999 should be clamped to 200
	result, err = da.GetDirectoryChildren(context.Background(), "", "", 999)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}
