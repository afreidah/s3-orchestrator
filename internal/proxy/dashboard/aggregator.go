// -------------------------------------------------------------------------------
// Dashboard Aggregator - Aggregated Stats for the Web UI
//
// Author: Alex Freidah
//
// Queries the metadata store and counter.UsageTracker to build dashboard
// snapshots. Exposes GetData for the main dashboard page and
// GetDirectoryChildren for lazy-loaded directory expansion. Delegates to
// the underlying core.DashboardStore, benefiting from the circuit breaker
// when wired through CircuitBreakerStore.
// -------------------------------------------------------------------------------

// Package dashboard owns the read-only stats aggregation that the web UI
// renders. The proxy package wraps Aggregator results with cluster-state
// enrichment (drain progress, breaker health) before returning them to
// HTTP handlers.
package dashboard

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// Data holds a snapshot of all operational data for the dashboard.
type Data struct {
	BackendOrder          []string
	QuotaStats            map[string]core.QuotaStat
	ObjectCounts          map[string]int64
	ActiveMultipartCounts map[string]int64
	UsageStats            map[string]core.UsageStat
	UsageLimits           map[string]core.UsageLimits
	UsagePeriod           string
	TopLevelEntries       *core.DirectoryListResult
	DrainingBackends      map[string]drain.Progress
	UnhealthyBackends     map[string]bool
}

// Aggregator queries the metadata store and usage tracker to build
// snapshots for the web UI.
type Aggregator struct {
	store core.DashboardStore
	usage *counter.UsageTracker
	order []string
}

// New creates an Aggregator.
func New(store core.DashboardStore, usage *counter.UsageTracker, order []string) *Aggregator {
	return &Aggregator{
		store: store,
		usage: usage,
		order: order,
	}
}

// GetData fetches all stats needed for the web UI in one call.
func (da *Aggregator) GetData(ctx context.Context) (*Data, error) {
	limits := da.usage.GetLimits()

	data := &Data{
		BackendOrder: da.order,
		UsageLimits:  limits,
		UsagePeriod:  counter.CurrentPeriod(),
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
func (da *Aggregator) GetDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*core.DirectoryListResult, error) {
	if maxKeys <= 0 || maxKeys > 200 {
		maxKeys = 200
	}
	return da.store.ListDirectoryChildren(ctx, prefix, startAfter, maxKeys)
}
