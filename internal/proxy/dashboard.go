// -------------------------------------------------------------------------------
// Dashboard Data - Aggregated Stats for the Web UI
//
// Author: Alex Freidah
//
// DashboardData type and thin wrappers on BackendManager that delegate to the
// DashboardAggregator.
// -------------------------------------------------------------------------------

package proxy

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/store"
)

// DashboardData holds a snapshot of all operational data for the dashboard.
type DashboardData struct {
	BackendOrder          []string
	QuotaStats            map[string]store.QuotaStat
	ObjectCounts          map[string]int64
	ActiveMultipartCounts map[string]int64
	UsageStats            map[string]store.UsageStat
	UsageLimits           map[string]store.UsageLimits
	UsagePeriod           string
	TopLevelEntries       *store.DirectoryListResult
	DrainingBackends      map[string]DrainProgress
	UnhealthyBackends     map[string]bool
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
		progress, err := m.DrainManager.GetDrainProgress(ctx, name)
		if err == nil {
			data.DrainingBackends[name] = *progress
		}
	}

	data.UnhealthyBackends = make(map[string]bool)
	for name, be := range m.backends {
		if cb, ok := be.(*backend.CircuitBreakerBackend); ok && !cb.IsHealthy() {
			data.UnhealthyBackends[name] = true
		}
	}

	return data, nil
}

// GetDirectoryChildren delegates to the DashboardAggregator.
func (m *BackendManager) GetDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*store.DirectoryListResult, error) {
	return m.dashboard.GetDirectoryChildren(ctx, prefix, startAfter, maxKeys)
}
