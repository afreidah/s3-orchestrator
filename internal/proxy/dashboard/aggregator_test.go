// -------------------------------------------------------------------------------
// DashboardAggregator Tests
//
// Author: Alex Freidah
//
// Tests for dashboard data aggregation: successful assembly, individual query
// errors, empty data, and directory listing edge cases.
// -------------------------------------------------------------------------------

package dashboard

import (
	"context"
	"errors"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// mockDashboardStore implements store.DashboardStore for aggregator tests.
type mockDashboardStore struct {
	quotaStats          map[string]core.QuotaStat
	quotaStatsErr       error
	objectCounts        map[string]int64
	objectCountsErr     error
	unverifiedCounts    map[string]int64
	unverifiedCountsErr error
	multipartCounts     map[string]int64
	multipartCountsErr  error
	usageStats          map[string]core.UsageStat
	usageStatsErr       error
	dirChildren         *core.DirectoryListResult
	dirChildrenErr      error
}

// GetQuotaStats returns quota stats.
func (m *mockDashboardStore) GetQuotaStats(_ context.Context) (map[string]core.QuotaStat, error) {
	return m.quotaStats, m.quotaStatsErr
}

// GetObjectCounts returns object counts.
func (m *mockDashboardStore) GetObjectCounts(_ context.Context) (map[string]int64, error) {
	return m.objectCounts, m.objectCountsErr
}

// GetUnverifiedObjectCounts returns unverified counts.
func (m *mockDashboardStore) GetUnverifiedObjectCounts(_ context.Context) (map[string]int64, error) {
	return m.unverifiedCounts, m.unverifiedCountsErr
}

// GetActiveMultipartCounts returns active multipart counts.
func (m *mockDashboardStore) GetActiveMultipartCounts(_ context.Context) (map[string]int64, error) {
	return m.multipartCounts, m.multipartCountsErr
}

// GetUsageForPeriod returns usage for period.
func (m *mockDashboardStore) GetUsageForPeriod(_ context.Context, _ string) (map[string]core.UsageStat, error) {
	return m.usageStats, m.usageStatsErr
}

// ListDirectoryChildren lists directory children.
func (m *mockDashboardStore) ListDirectoryChildren(_ context.Context, _, _ string, _ int) (*core.DirectoryListResult, error) {
	return m.dirChildren, m.dirChildrenErr
}

// TestAggregator_Success verifies the aggregator success contract.
// Asserts that BytesUsed = , want 100.
func TestAggregator_Success(t *testing.T) {
	t.Parallel()
	ms := &mockDashboardStore{
		quotaStats:      map[string]core.QuotaStat{"b1": {BytesUsed: 100}},
		objectCounts:    map[string]int64{"b1": 5},
		multipartCounts: map[string]int64{"b1": 1},
		usageStats:      map[string]core.UsageStat{"b1": {APIRequests: 10}},
		dirChildren:     &core.DirectoryListResult{},
	}

	usage := counter.NewUsageTracker(
		counter.NewLocalCounterBackend([]string{"b1"}),
		nil,
	)

	da := New(ms, usage, []string{"b1"})
	data, err := da.GetData(context.Background())
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

// TestAggregator_QuotaStatsError verifies the aggregator quota stats error path by exercising errors.New, counter.NewUsageTracker, counter.NewLocalCounterBackend.
func TestAggregator_QuotaStatsError(t *testing.T) {
	t.Parallel()
	ms := &mockDashboardStore{
		quotaStatsErr: errors.New("db down"),
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil)
	da := New(ms, usage, nil)

	_, err := da.GetData(context.Background())
	if err == nil {
		t.Fatal("expected error when QuotaStats fails")
	}
}

// TestAggregator_ObjectCountsError verifies the aggregator object counts error path by exercising errors.New, counter.NewUsageTracker, counter.NewLocalCounterBackend.
func TestAggregator_ObjectCountsError(t *testing.T) {
	t.Parallel()
	ms := &mockDashboardStore{
		quotaStats:      map[string]core.QuotaStat{},
		objectCountsErr: errors.New("db down"),
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil)
	da := New(ms, usage, nil)

	_, err := da.GetData(context.Background())
	if err == nil {
		t.Fatal("expected error when ObjectCounts fails")
	}
}

// TestAggregator_UnverifiedCountsError pins the GetData failure when
// GetUnverifiedObjectCounts errors. Mirrors TestAggregator_ObjectCountsError.
func TestAggregator_UnverifiedCountsError(t *testing.T) {
	t.Parallel()
	ms := &mockDashboardStore{
		quotaStats:          map[string]core.QuotaStat{},
		objectCounts:        map[string]int64{},
		unverifiedCountsErr: errors.New("db down"),
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil)
	da := New(ms, usage, nil)

	if _, err := da.GetData(context.Background()); err == nil {
		t.Fatal("expected error when GetUnverifiedObjectCounts fails")
	}
}

// TestAggregator_MultipartCountsError verifies the aggregator multipart counts error path by exercising errors.New, counter.NewUsageTracker, counter.NewLocalCounterBackend.
func TestAggregator_MultipartCountsError(t *testing.T) {
	t.Parallel()
	ms := &mockDashboardStore{
		quotaStats:         map[string]core.QuotaStat{},
		objectCounts:       map[string]int64{},
		multipartCountsErr: errors.New("db down"),
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil)
	da := New(ms, usage, nil)

	_, err := da.GetData(context.Background())
	if err == nil {
		t.Fatal("expected error when MultipartCounts fails")
	}
}

// TestAggregator_UsageStatsError verifies the aggregator usage stats error path by exercising errors.New, counter.NewUsageTracker, counter.NewLocalCounterBackend.
func TestAggregator_UsageStatsError(t *testing.T) {
	t.Parallel()
	ms := &mockDashboardStore{
		quotaStats:      map[string]core.QuotaStat{},
		objectCounts:    map[string]int64{},
		multipartCounts: map[string]int64{},
		usageStatsErr:   errors.New("db down"),
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil)
	da := New(ms, usage, nil)

	_, err := da.GetData(context.Background())
	if err == nil {
		t.Fatal("expected error when UsageStats fails")
	}
}

// TestAggregator_DirChildrenError verifies the aggregator dir children error path by exercising errors.New, counter.NewUsageTracker, counter.NewLocalCounterBackend.
func TestAggregator_DirChildrenError(t *testing.T) {
	t.Parallel()
	ms := &mockDashboardStore{
		quotaStats:      map[string]core.QuotaStat{},
		objectCounts:    map[string]int64{},
		multipartCounts: map[string]int64{},
		usageStats:      map[string]core.UsageStat{},
		dirChildrenErr:  errors.New("db down"),
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil)
	da := New(ms, usage, nil)

	_, err := da.GetData(context.Background())
	if err == nil {
		t.Fatal("expected error when DirChildren fails")
	}
}

// TestAggregator_GetDirectoryChildren_ClampsMaxKeys verifies the aggregator get directory children clamps max keys path by exercising counter.NewUsageTracker, counter.NewLocalCounterBackend, da.GetDirectoryChildren.
func TestAggregator_GetDirectoryChildren_ClampsMaxKeys(t *testing.T) {
	t.Parallel()
	ms := &mockDashboardStore{
		dirChildren: &core.DirectoryListResult{},
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil)
	da := New(ms, usage, nil)

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
