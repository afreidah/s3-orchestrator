// -------------------------------------------------------------------------------
// Dashboard Data - Aggregated Stats for the Web UI
//
// Author: Alex Freidah
//
// Exposes a single GetDashboardData method that fetches all operational stats
// needed for the web dashboard in one call. Delegates to the underlying
// MetadataStore, benefiting from the circuit breaker when wired through
// CircuitBreakerStore.
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
}

// GetDashboardData fetches all stats needed for the web UI in one call.
func (m *BackendManager) GetDashboardData(ctx context.Context) (*DashboardData, error) {
	data := &DashboardData{
		BackendOrder: m.order,
		UsageLimits:  m.usageLimits,
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

	return data, nil
}
