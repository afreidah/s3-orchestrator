// -------------------------------------------------------------------------------
// Dashboard Aggregator - Aggregated Stats for the Web UI
//
// Author: Alex Freidah
//
// Builds the dashboard and admin status views from the fleet snapshot the
// metrics collector maintains, the current period's usage and the live fleet
// state. Exposes GetStatus for the admin status endpoint, GetData for the main
// dashboard page and GetDirectoryChildren for lazy-loaded directory expansion.
// -------------------------------------------------------------------------------

package dashboard

import (
	"context"
	"errors"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/backendstate"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// maxDirectoryChildren bounds one page of the lazy-loaded file browser, both
// for the top-level listing and for a caller-supplied page size.
const maxDirectoryChildren = 200

// errIncompleteSnapshot reports a fleet snapshot missing the ledger or
// multipart figures because the store read behind them failed.
var errIncompleteSnapshot = errors.New("fleet snapshot is missing figures; the store read behind them failed")

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
// RefusedCalls holds only backends refusing at least one call, so a missing
// entry means the backend serves every call.
type Data struct {
	BackendOrder           []string
	QuotaStats             map[string]core.QuotaStat
	ObjectCounts           map[string]int64
	UnverifiedObjectCounts map[string]int64
	NeverVerifiedCopies    int64
	OldestUnverifiedAge    time.Duration
	DeferredCopies         int64
	PlaintextCopies        int64
	CompressionStats       map[string]core.CompressionStat
	ActiveMultipartCounts  map[string]int64
	UsageStats             map[string]core.Usage
	UsageLimits            map[string]core.UsageLimits
	UsagePeriod            string
	TopLevelEntries        *core.DirectoryListResult
	DrainingBackends       map[string]backendstate.DrainProgress
	BackendFlags           map[string]core.BackendFlags
	RefusedCalls           map[string]core.BackendCalls
}

//go:generate mockgen -destination=mock_test.go -package=dashboard github.com/afreidah/s3-orchestrator/internal/proxy/dashboard FleetView,DrainProgressReader,UsageReader,ScrubReachReader

// FleetView is the backend-fleet surface the aggregator reads: the fleet
// snapshot the ledger figures come from, and the live state it decorates them
// with. *infra.BackendRuntime satisfies it.
type FleetView interface {
	FleetSnapshot(ctx context.Context) (*metrics.FleetSnapshot, error)
	BackendOrder() []string
	BackendFlags(name string) core.BackendFlags
	ServedCalls(name string) core.BackendCalls
}

// UsageReader is the usage surface the aggregator reads: each backend's usage
// so far this cycle and the limits it renders it against.
// *counter.UsageTracker satisfies it.
type UsageReader interface {
	GetLimits() map[string]core.UsageLimits
	CycleUsage() map[string]core.Usage
}

// ScrubReachReader names the backends the scrub queue draws from right now,
// which scopes the integrity figures so the dashboard and the sweep agree on
// which copies are reachable. *worker.Scrubber satisfies it.
type ScrubReachReader interface {
	ReachableBackends() []string
}

// DrainProgressReader reports per-backend drain progress.
// *backendstate.Manager satisfies it. Nil when the deployment runs no backend
// state manager.
type DrainProgressReader interface {
	GetDrainProgress(ctx context.Context, name string) (*backendstate.DrainProgress, error)
}

// Aggregator queries the metadata store and usage tracker to build
// snapshots for the web UI.
type Aggregator struct {
	store core.DashboardStore
	usage UsageReader
	scrub ScrubReachReader
	order []string
	fleet FleetView
	drain DrainProgressReader
}

// New creates an Aggregator over the given backend order. scrub may be nil,
// which scopes the integrity figures to every backend; drainReader may be nil
// when the deployment runs no backend state manager.
func New(store core.DashboardStore, usage UsageReader, scrub ScrubReachReader, order []string, fleet FleetView, drainReader DrainProgressReader) *Aggregator {
	return &Aggregator{
		store: store,
		usage: usage,
		scrub: scrub,
		order: order,
		fleet: fleet,
		drain: drainReader,
	}
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// scrubbableBackends is the set the scrub queue would draw from right now, or
// every backend when no scrubber is wired.
func (da *Aggregator) scrubbableBackends() []string {
	if da.scrub == nil {
		return da.order
	}
	return da.scrub.ReachableBackends()
}

// decorateLiveState fills in the fields that come from the running fleet
// rather than the store: each backend's state flags, which backends are
// draining and how far along, and which calls each is refusing. Drain progress
// is best-effort - a backend whose progress cannot be read is simply omitted
// rather than failing the whole dashboard.
func (da *Aggregator) decorateLiveState(ctx context.Context, data *Data) {
	data.DrainingBackends = make(map[string]backendstate.DrainProgress)
	data.BackendFlags = make(map[string]core.BackendFlags)
	data.RefusedCalls = make(map[string]core.BackendCalls)

	for _, name := range da.fleet.BackendOrder() {
		if refused := core.AllCalls &^ da.fleet.ServedCalls(name); refused != 0 {
			data.RefusedCalls[name] = refused
		}
		flags := da.fleet.BackendFlags(name)
		if flags == 0 {
			continue
		}
		data.BackendFlags[name] = flags
		if da.drain == nil || !flags.Any(core.DrainFlags) {
			continue
		}
		if progress, err := da.drain.GetDrainProgress(ctx, name); err == nil {
			data.DrainingBackends[name] = *progress
		}
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// GetStatus builds the status figures from the fleet snapshot, plus this
// period's usage and the live fleet state. It fails rather than report zeroes
// when the snapshot is missing a figure because its store read failed.
func (da *Aggregator) GetStatus(ctx context.Context) (*Data, error) {
	snap, err := da.fleet.FleetSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	if snap.Ledger == nil || snap.Multipart == nil {
		return nil, errIncompleteSnapshot
	}

	coverage := snap.Ledger.Coverage(da.scrubbableBackends(), time.Now())
	data := &Data{
		BackendOrder:           da.order,
		UsageLimits:            da.usage.GetLimits(),
		UsagePeriod:            counter.CurrentPeriod(),
		QuotaStats:             snap.Quota,
		ObjectCounts:           snap.Ledger.ObjectCounts(),
		UnverifiedObjectCounts: snap.Ledger.UnhashedCounts(),
		OldestUnverifiedAge:    coverage.OldestUnverifiedAge,
		NeverVerifiedCopies:    coverage.NeverVerified,
		DeferredCopies:         coverage.Deferred,
		PlaintextCopies:        snap.Ledger.PlaintextCopies(),
		CompressionStats:       snap.Ledger.Compression(),
		ActiveMultipartCounts:  snap.Multipart,
		UsageStats:             da.usage.CycleUsage(),
	}
	da.decorateLiveState(ctx, data)
	return data, nil
}

// GetData is GetStatus plus the top-level entries of the web UI's file
// browser.
func (da *Aggregator) GetData(ctx context.Context) (*Data, error) {
	data, err := da.GetStatus(ctx)
	if err != nil {
		return nil, err
	}
	data.TopLevelEntries, err = da.store.ListDirectoryChildren(ctx, "", "", maxDirectoryChildren)
	if err != nil {
		return nil, err
	}
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

// HasCompressedData reports whether any copy is stored encoded. The views gate
// on this, not the config flag, which only describes new writes.
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
