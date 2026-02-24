// -------------------------------------------------------------------------------
// Dashboard Data - Aggregated Stats for the Web UI
//
// Author: Alex Freidah
//
// Exposes GetDashboardData for the main dashboard page and GetDirectoryChildren
// for lazy-loaded directory expansion. Delegates to the underlying MetadataStore,
// benefiting from the circuit breaker when wired through CircuitBreakerStore.
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

// GetDashboardData fetches all stats needed for the web UI in one call.
func (m *BackendManager) GetDashboardData(ctx context.Context) (*DashboardData, error) {
	m.usageLimitsMu.RLock()
	limits := m.usageLimits
	m.usageLimitsMu.RUnlock()

	data := &DashboardData{
		BackendOrder: m.order,
		UsageLimits:  limits,
		UsagePeriod:  currentPeriod(),
	}

	var err error

	data.QuotaStats, err = m.store.GetQuotaStats(ctx)
	if err != nil {
		return nil, err
	}

	data.ObjectCounts, err = m.store.GetObjectCounts(ctx)
	if err != nil {
		return nil, err
	}

	data.ActiveMultipartCounts, err = m.store.GetActiveMultipartCounts(ctx)
	if err != nil {
		return nil, err
	}

	data.UsageStats, err = m.store.GetUsageForPeriod(ctx, data.UsagePeriod)
	if err != nil {
		return nil, err
	}

	// Fetch top-level directory entries for the lazy-loaded file browser.
	data.TopLevelEntries, err = m.store.ListDirectoryChildren(ctx, "", "", 200)
	if err != nil {
		return nil, err
	}

	return data, nil
}

// GetDirectoryChildren returns the immediate children of a directory path
// for the lazy-loaded file browser.
func (m *BackendManager) GetDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*DirectoryListResult, error) {
	if maxKeys <= 0 || maxKeys > 200 {
		maxKeys = 200
	}
	return m.store.ListDirectoryChildren(ctx, prefix, startAfter, maxKeys)
}
