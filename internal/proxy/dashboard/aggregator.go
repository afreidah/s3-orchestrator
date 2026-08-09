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
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// maxDirectoryChildren bounds one page of the lazy-loaded file browser, both
// for the top-level listing and for a caller-supplied page size.
const maxDirectoryChildren = 200

// Data holds a snapshot of all operational data for the dashboard.
type Data struct {
	BackendOrder []string
	QuotaStats   map[string]core.QuotaStat
	ObjectCounts map[string]int64
	// UnverifiedObjectCounts is per-backend count of objects with a
	// NULL content_hash (objects that predate integrity verification or
	// otherwise have not been checksummed). Drives the dashboard's
	// "needs backfill" column when integrity is enabled.
	UnverifiedObjectCounts map[string]int64
	NeverVerifiedCopies    int64
	OldestUnverifiedAge    time.Duration
	ActiveMultipartCounts  map[string]int64
	UsageStats             map[string]core.UsageStat
	UsageLimits            map[string]core.UsageLimits
	UsagePeriod            string
	TopLevelEntries        *core.DirectoryListResult
	DrainingBackends       map[string]drain.Progress
	UnhealthyBackends      map[string]bool
}

// Aggregator queries the metadata store and usage tracker to build
// snapshots for the web UI.
//go:generate mockgen -destination=mock_test.go -package=dashboard github.com/afreidah/s3-orchestrator/internal/proxy/dashboard FleetView,DrainProgressReader,UsageReader

// FleetView is the backend-fleet surface the aggregator reads to decorate
// stored stats with live state. *infra.BackendRuntime satisfies it.
type FleetView interface {
	BackendOrder() []string
	IsDraining(name string) bool
	Backends() map[string]backend.ObjectBackend
}

// UsageReader is the usage surface the aggregator reads: the configured
// per-backend limits it renders alongside consumption.
// *counter.UsageTracker satisfies it.
type UsageReader interface {
	GetLimits() map[string]core.UsageLimits
}

// DrainProgressReader reports per-backend drain progress.
// *drain.Manager satisfies it. Nil when the deployment runs no drain manager.
type DrainProgressReader interface {
	GetDrainProgress(ctx context.Context, name string) (*drain.Progress, error)
}

type Aggregator struct {
	store core.DashboardStore
	usage UsageReader
	order []string
	fleet FleetView
	drain DrainProgressReader
}

// New creates an Aggregator.
func New(store core.DashboardStore, usage UsageReader, order []string, fleet FleetView, drainReader DrainProgressReader) *Aggregator {
	return &Aggregator{
		store: store,
		usage: usage,
		order: order,
		fleet: fleet,
		drain: drainReader,
	}
}

// decorateLiveState fills in the fields that come from the running fleet
// rather than the store: which backends are draining and how far along, and
// which are failing their circuit breaker. Both are best-effort - a backend
// whose drain progress cannot be read is simply omitted rather than failing
// the whole dashboard.
func (da *Aggregator) decorateLiveState(ctx context.Context, data *Data) {
	data.DrainingBackends = make(map[string]drain.Progress)
	data.UnhealthyBackends = make(map[string]bool)
	if da.fleet == nil {
		return
	}

	if da.drain != nil {
		for _, name := range da.fleet.BackendOrder() {
			if !da.fleet.IsDraining(name) {
				continue
			}
			if progress, err := da.drain.GetDrainProgress(ctx, name); err == nil {
				data.DrainingBackends[name] = *progress
			}
		}
	}

	for name, be := range da.fleet.Backends() {
		if cb, ok := be.(*backend.CircuitBreakerBackend); ok && !cb.IsHealthy() {
			data.UnhealthyBackends[name] = true
		}
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

	data.UnverifiedObjectCounts, err = da.store.GetUnverifiedObjectCounts(ctx)
	if err != nil {
		return nil, err
	}

	data.OldestUnverifiedAge, data.NeverVerifiedCopies, err = da.store.OldestUnverifiedAge(ctx)
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
	data.TopLevelEntries, err = da.store.ListDirectoryChildren(ctx, "", "", maxDirectoryChildren)
	if err != nil {
		return nil, err
	}

	da.decorateLiveState(ctx, data)
	return data, nil
}

// GetDirectoryChildren returns the immediate children of a directory path
// for the lazy-loaded file browser.
func (da *Aggregator) GetDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*core.DirectoryListResult, error) {
	if maxKeys <= 0 || maxKeys > maxDirectoryChildren {
		maxKeys = maxDirectoryChildren
	}
	return da.store.ListDirectoryChildren(ctx, prefix, startAfter, maxKeys)
}
