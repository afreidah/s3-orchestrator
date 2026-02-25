// -------------------------------------------------------------------------------
// DashboardAggregator - Aggregated Stats for the Web UI
//
// Author: Alex Freidah
//
// Queries the MetadataStore and UsageTracker to build dashboard snapshots.
// Exposes GetDashboardData for the main dashboard page and GetDirectoryChildren
// for lazy-loaded directory expansion. Delegates to the underlying MetadataStore,
// benefiting from the circuit breaker when wired through CircuitBreakerStore.
// -------------------------------------------------------------------------------

package storage

import "context"

// DashboardAggregator queries the metadata store and usage tracker to build
// snapshots for the web UI.
type DashboardAggregator struct {
	store MetadataStore
	usage *UsageTracker
	order []string
}

// NewDashboardAggregator creates a DashboardAggregator.
func NewDashboardAggregator(store MetadataStore, usage *UsageTracker, order []string) *DashboardAggregator {
	return &DashboardAggregator{
		store: store,
		usage: usage,
		order: order,
	}
}

// GetDashboardData fetches all stats needed for the web UI in one call.
func (da *DashboardAggregator) GetDashboardData(ctx context.Context) (*DashboardData, error) {
	limits := da.usage.GetLimits()

	data := &DashboardData{
		BackendOrder: da.order,
		UsageLimits:  limits,
		UsagePeriod:  currentPeriod(),
	}

	var err error

	data.QuotaStats, err = da.store.GetQuotaStats(ctx)
	if err != nil {
		return nil, err
	}

	data.ObjectCounts, err = da.store.GetObjectCounts(ctx)
	if err != nil {
		return nil, err
	}

	data.ActiveMultipartCounts, err = da.store.GetActiveMultipartCounts(ctx)
	if err != nil {
		return nil, err
	}

	data.UsageStats, err = da.store.GetUsageForPeriod(ctx, data.UsagePeriod)
	if err != nil {
		return nil, err
	}

	// Fetch top-level directory entries for the lazy-loaded file browser.
	data.TopLevelEntries, err = da.store.ListDirectoryChildren(ctx, "", "", 200)
	if err != nil {
		return nil, err
	}

	return data, nil
}

// GetDirectoryChildren returns the immediate children of a directory path
// for the lazy-loaded file browser.
func (da *DashboardAggregator) GetDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*DirectoryListResult, error) {
	if maxKeys <= 0 || maxKeys > 200 {
		maxKeys = 200
	}
	return da.store.ListDirectoryChildren(ctx, prefix, startAfter, maxKeys)
}
