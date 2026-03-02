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
	DrainingBackends      map[string]DrainProgress
}

// GetDashboardData delegates to the DashboardAggregator and enriches the
// result with drain status from the BackendManager's in-memory state.
func (m *BackendManager) GetDashboardData(ctx context.Context) (*DashboardData, error) {
	data, err := m.dashboard.GetDashboardData(ctx)
	if err != nil {
		return nil, err
	}

	data.DrainingBackends = make(map[string]DrainProgress)
	for _, name := range m.order {
		if !m.IsDraining(name) {
			continue
		}
		progress, err := m.GetDrainProgress(ctx, name)
		if err == nil {
			data.DrainingBackends[name] = *progress
		}
	}

	return data, nil
}

// GetDirectoryChildren delegates to the DashboardAggregator.
func (m *BackendManager) GetDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*DirectoryListResult, error) {
	return m.dashboard.GetDirectoryChildren(ctx, prefix, startAfter, maxKeys)
}
