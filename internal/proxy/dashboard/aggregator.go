// -------------------------------------------------------------------------------
// Dashboard Aggregator - Aggregated Stats for the Web UI
//
// Author: Alex Freidah
//
// Builds the dashboard and admin status views from the fleet snapshot the
// metrics collector reads, the current period's usage and the live fleet
// state. GetStatus serves both the admin status endpoint and the dashboard page.
// -------------------------------------------------------------------------------

package dashboard

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/backendstate"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Data holds a snapshot of all operational data for the dashboard.
//
// Ledger holds each backend's object, checksum, encryption and compression
// figures, and a backend that holds no rows is absent from it, so a lookup
// reads as zero. RefusedCalls holds only backends refusing at least one call,
// so a missing entry means the backend serves every call. Unsupported likewise
// holds only backends that have refused an optional feature. Pressure and
// Profiles hold every backend.
type Data struct {
	BackendOrder          []string
	QuotaStats            map[string]core.QuotaStat
	Ledger                core.LedgerStats
	NeverVerifiedCopies   int64
	OldestUnverifiedAge   time.Duration
	DeferredCopies        int64
	ActiveMultipartCounts map[string]int64
	UsageStats            map[string]core.Usage
	UsageCycles           map[string]core.UsageCycle
	UsageLimits           map[string]core.UsageLimits
	UsagePeriod           string
	DrainingBackends      map[string]backendstate.DrainProgress
	BackendFlags          map[string]core.BackendFlags
	RefusedCalls          map[string]core.BackendCalls
	Unsupported           map[string]core.BackendFeatures
	Pressure              map[string]counter.PressureState
	Profiles              map[string]core.BackendProfile
}

//go:generate mockgen -destination=mock_test.go -package=dashboard github.com/afreidah/s3-orchestrator/internal/proxy/dashboard FleetView,DrainProgressReader,UsageReader

// FleetView is the live state and configured profiles the aggregator decorates
// the snapshot's figures with. *infra.BackendRuntime satisfies it.
type FleetView interface {
	BackendOrder() []string
	BackendFlags(name string) core.BackendFlags
	ServedCalls(name string) core.BackendCalls
	Unsupported(name string) core.BackendFeatures
	PressureState(name string) counter.PressureState
	Profile(name string) core.BackendProfile
}

// FleetSnapshotter serves the fleet snapshot the ledger figures come from.
// *metrics.Collector satisfies it.
type FleetSnapshotter interface {
	FleetSnapshot(ctx context.Context) (*metrics.FleetSnapshot, error)
}

// UsageReader is the usage surface the aggregator reads: each backend's usage
// so far this cycle and the limits it renders it against.
// *counter.UsageTracker satisfies it.
type UsageReader interface {
	GetLimits() map[string]core.UsageLimits
	CycleUsage() map[string]core.CycleUsage
}

// DrainProgressReader reports per-backend drain progress.
// *backendstate.Manager satisfies it. Nil when the deployment runs no backend
// state manager.
type DrainProgressReader interface {
	GetDrainProgress(ctx context.Context, name string) (*backendstate.DrainProgress, error)
}

// Deps groups what the aggregator reads. Drain may be nil when the deployment
// runs no backend state manager.
type Deps struct {
	Usage     UsageReader
	Fleet     FleetView
	Snapshots FleetSnapshotter
	Drain     DrainProgressReader
}

// Aggregator builds the dashboard and status snapshots from the fleet
// snapshot, the usage tracker and the live fleet state.
type Aggregator struct {
	d Deps
}

// New creates an Aggregator over the fleet's backends, in the fleet's order.
func New(d *Deps) *Aggregator {
	return &Aggregator{d: *d}
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// splitCycleUsage separates each backend's cycle usage into its figures and
// its cycle. Both come from one read, so a figure and its label agree.
func splitCycleUsage(in map[string]core.CycleUsage) (map[string]core.Usage, map[string]core.UsageCycle) {
	usage := make(map[string]core.Usage, len(in))
	cycles := make(map[string]core.UsageCycle, len(in))
	for name, cu := range in {
		usage[name] = cu.Usage
		cycles[name] = cu.Cycle
	}
	return usage, cycles
}

// decorateLiveState fills in the fields that come from the running fleet
// rather than the store: each backend's profile and state flags, which
// backends are draining and how far along, which calls each is refusing, and
// which optional features each has refused. Drain progress is best-effort - a
// backend whose progress cannot be read is simply omitted rather than failing
// the whole dashboard.
func (da *Aggregator) decorateLiveState(ctx context.Context, data *Data) {
	data.DrainingBackends = make(map[string]backendstate.DrainProgress)
	data.BackendFlags = make(map[string]core.BackendFlags)
	data.RefusedCalls = make(map[string]core.BackendCalls)
	data.Unsupported = make(map[string]core.BackendFeatures)
	data.Pressure = make(map[string]counter.PressureState)
	data.Profiles = make(map[string]core.BackendProfile)

	for _, name := range da.d.Fleet.BackendOrder() {
		data.Profiles[name] = da.d.Fleet.Profile(name)
		data.Pressure[name] = da.d.Fleet.PressureState(name)
		if refused := core.AllCalls &^ da.d.Fleet.ServedCalls(name); refused != 0 {
			data.RefusedCalls[name] = refused
		}
		if unsupported := da.d.Fleet.Unsupported(name); unsupported != 0 {
			data.Unsupported[name] = unsupported
		}
		flags := da.d.Fleet.BackendFlags(name)
		if flags == 0 {
			continue
		}
		data.BackendFlags[name] = flags
		if da.d.Drain == nil || !flags.Any(core.DrainFlags) {
			continue
		}
		if progress, err := da.d.Drain.GetDrainProgress(ctx, name); err == nil {
			data.DrainingBackends[name] = *progress
		}
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// GetStatus builds the status figures from the fleet snapshot, plus this
// period's usage and the live fleet state. It fails rather than report zeroes
// when a store read behind the snapshot failed.
func (da *Aggregator) GetStatus(ctx context.Context) (*Data, error) {
	snap, err := da.d.Snapshots.FleetSnapshot(ctx)
	if err != nil {
		return nil, err
	}

	data := &Data{
		BackendOrder:          da.d.Fleet.BackendOrder(),
		UsageLimits:           da.d.Usage.GetLimits(),
		UsagePeriod:           counter.CurrentPeriod(),
		QuotaStats:            snap.Quota,
		Ledger:                snap.Ledger,
		OldestUnverifiedAge:   snap.Coverage.OldestUnverifiedAge,
		NeverVerifiedCopies:   snap.Coverage.NeverVerified,
		DeferredCopies:        snap.Coverage.Deferred,
		ActiveMultipartCounts: snap.Multipart,
	}
	data.UsageStats, data.UsageCycles = splitCycleUsage(da.d.Usage.CycleUsage())
	da.decorateLiveState(ctx, data)
	return data, nil
}

// CompressionTotals sums the per-backend stats into one fleet figure, which is
// what the dashboard reports. Summing in the view would mean the template doing
// arithmetic, and this is the number an operator reads first.
func (d *Data) CompressionTotals() core.CompressionStat {
	var total core.CompressionStat
	for _, st := range d.Ledger {
		total.Objects += st.Compressed.Objects
		total.LogicalBytes += st.Compressed.LogicalBytes
		total.StoredBytes += st.Compressed.StoredBytes
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
	s := d.Ledger[backend].Compressed
	return s.LogicalBytes - s.StoredBytes
}
