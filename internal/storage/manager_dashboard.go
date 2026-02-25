// -------------------------------------------------------------------------------
// Dashboard Data - Aggregated Stats for the Web UI
//
// Author: Alex Freidah
//
// DashboardData type and thin wrappers on BackendManager that delegate to the
// DashboardAggregator.
// -------------------------------------------------------------------------------

package storage

import "context"

// DashboardData holds a snapshot of all operational data for the dashboard.
type DashboardData struct {
	BackendOrder          []string
	QuotaStats            map[string]QuotaStat
	ObjectCounts          map[string]int64
	ActiveMultipartCounts map[string]int64
	UsageStats            map[string]UsageStat
	UsageLimits           map[string]UsageLimits
	UsagePeriod           string
	TopLevelEntries       *DirectoryListResult
}

// GetDashboardData delegates to the DashboardAggregator.
func (m *BackendManager) GetDashboardData(ctx context.Context) (*DashboardData, error) {
	return m.dashboard.GetDashboardData(ctx)
}

// GetDirectoryChildren delegates to the DashboardAggregator.
func (m *BackendManager) GetDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*DirectoryListResult, error) {
	return m.dashboard.GetDirectoryChildren(ctx, prefix, startAfter, maxKeys)
}
