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

package dashboard

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// maxDirectoryChildren bounds one page of the lazy-loaded file browser, both
// for the top-level listing and for a caller-supplied page size.
const maxDirectoryChildren = 200

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Data holds a snapshot of all operational data for the dashboard.
//
// UnverifiedObjectCounts is the per-backend count of objects with a NULL
// content_hash, which drives the "needs backfill" column. PlaintextCopies is
// how many copies are still stored unencrypted: encryption covers new writes
// only, so a non-zero value means existing objects were never rewritten.
//
// CompressionStats holds only backends that hold an encoded copy, so a missing
// entry means nothing there is compressed rather than compressed to nothing.
type Data struct {
	BackendOrder           []string
	QuotaStats             map[string]core.QuotaStat
	ObjectCounts           map[string]int64
	UnverifiedObjectCounts map[string]int64
	NeverVerifiedCopies    int64
	OldestUnverifiedAge    time.Duration
	PlaintextCopies        int64
	CompressionStats       map[string]core.CompressionStat
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

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

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

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

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

	data.PlaintextCopies, err = da.store.CountUnencryptedLocations(ctx)
	if err != nil {
		return nil, err
	}

	data.CompressionStats, err = da.store.CompressionStats(ctx)
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

// CompressionTotals sums the per-backend stats into one fleet figure, which is
// what the dashboard reports. Summing in the view would mean the template doing
// arithmetic, and this is the number an operator reads first.
func (d *Data) CompressionTotals() core.CompressionStat {
	var total core.CompressionStat
	for _, s := range d.CompressionStats {
		total.Objects += s.Objects
		total.LogicalBytes += s.LogicalBytes
		total.StoredBytes += s.StoredBytes
	}
	return total
}

// HasCompressedData reports whether any copy is stored encoded, which is what
// the views gate on rather than on the config flag.
//
// The two answer different questions. The flag says whether new writes will be
// encoded; this says whether anything already is. They disagree in both
// directions that matter: a fleet with the feature freshly enabled has nothing
// to show yet, and one with it freshly disabled still holds everything it
// compressed - including the objects an operator now wants to unwind.
func (d *Data) HasCompressedData() bool {
	return d.CompressionTotals().Objects > 0
}

// CompressionSaved reports the bytes compression saved on one backend, or zero
// when nothing there is stored encoded.
func (d *Data) CompressionSaved(backend string) int64 {
	s, ok := d.CompressionStats[backend]
	if !ok {
		return 0
	}
	return s.LogicalBytes - s.StoredBytes
}

// GetDirectoryChildren returns the immediate children of a directory path
// for the lazy-loaded file browser.
func (da *Aggregator) GetDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*core.DirectoryListResult, error) {
	if maxKeys <= 0 || maxKeys > maxDirectoryChildren {
		maxKeys = maxDirectoryChildren
	}
	return da.store.ListDirectoryChildren(ctx, prefix, startAfter, maxKeys)
}
