// -------------------------------------------------------------------------------
// Fleet Gauges - Fleet-Wide Figures Read From the Store
//
// Author: Alex Freidah
//
// The quota, object, multipart, plaintext, verification-coverage and queue
// depth gauges describe state every instance shares. Each is a read of a few
// counter rows or a count of a queue's rows, so every instance reads them
// itself on its flush tick and serves the same values without coordinating.
// Capacity warnings are the exception: one instance at a time raises them, and
// a shared marker keeps the fleet from raising one again within the notifier's
// dampen window.
// -------------------------------------------------------------------------------

package metrics

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/sharedstate"
	"github.com/afreidah/s3-orchestrator/internal/store/core"

	"github.com/prometheus/client_golang/prometheus"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// capacityWarningUtilization is the fraction of a quota at which a backend
// is reported as approaching capacity.
const capacityWarningUtilization = 0.8

// capacityWarningWindow is how long a backend's capacity warning stands before
// the fleet raises it again, the notifier's dampen window.
const capacityWarningWindow = time.Hour

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// FleetSnapshot is one read of the fleet-wide figures. Ledger carries every
// per-backend ledger figure, so the gauges and the dashboard read the same
// numbers, and Coverage is scoped to the backends the scrub queue draws from.
type FleetSnapshot struct {
	Quota     map[string]core.QuotaStat
	Ledger    core.LedgerStats
	Multipart map[string]int64
	Coverage  core.CoverageStat
}

// -------------------------------------------------------------------------
// READ
// -------------------------------------------------------------------------

// FleetSnapshot reads the fleet-wide figures now. Each is a read of a few
// rows, so the dashboard reads them fresh rather than from a cached copy.
func (mc *Collector) FleetSnapshot(ctx context.Context) (*FleetSnapshot, error) {
	stats, err := mc.d.Store.GetQuotaStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("read quota stats: %w", err)
	}
	return mc.readFleet(ctx, stats)
}

// readFleet reads the ledger, multipart and coverage figures to go with a
// quota read the caller made.
func (mc *Collector) readFleet(ctx context.Context, stats map[string]core.QuotaStat) (*FleetSnapshot, error) {
	ledger, err := mc.d.Store.LedgerStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("read ledger stats: %w", err)
	}
	multipart, err := mc.d.Store.GetActiveMultipartCounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("read multipart upload counts: %w", err)
	}
	coverage, err := mc.coverage(ctx, ledger)
	if err != nil {
		return nil, err
	}
	return &FleetSnapshot{Quota: stats, Ledger: ledger, Multipart: multipart, Coverage: coverage}, nil
}

// coverage reports how far behind verification is on the backends the scrub
// queue draws from. The oldest touch among them is the head of the scrub
// queue, the copy the sweep would read first.
func (mc *Collector) coverage(ctx context.Context, ledger core.LedgerStats) (core.CoverageStat, error) {
	reachable := mc.d.Scrub.ReachableBackends()
	now := time.Now()
	oldest, err := core.OldestTouched(ctx, mc.d.Store, reachable, now)
	if err != nil {
		return core.CoverageStat{}, fmt.Errorf("read the scrub queue head: %w", err)
	}
	return ledger.Coverage(reachable, oldest, now), nil
}

// -------------------------------------------------------------------------
// APPLY
// -------------------------------------------------------------------------

// refreshFleet reads the fleet-wide figures beside the quota read, publishes
// them to the gauges, and raises the capacity warnings due.
func (mc *Collector) refreshFleet(ctx context.Context, stats map[string]core.QuotaStat) error {
	snap, err := mc.readFleet(ctx, stats)
	if err != nil {
		return err
	}
	mc.applyFleet(snap)
	mc.warnNearCapacity(ctx, stats)
	return nil
}

// refreshQueues publishes how many rows wait in each queue a worker drains. A
// failed count leaves its gauges as they were.
func (mc *Collector) refreshQueues(ctx context.Context) error {
	s := mc.d.Store
	backlog, err := s.CountReplicationQueue(ctx)
	if err == nil {
		telemetry.ReplicationPending.Set(float64(backlog.Under))
		telemetry.OverReplicationPending.Set(float64(backlog.Over))
	}
	errs := []error{err}
	for gauge, count := range map[prometheus.Gauge]func(context.Context) (int64, error){
		telemetry.PendingIntentsDepth:    s.PendingDepth,
		telemetry.CleanupQueueDepth:      s.CleanupQueueDepth,
		telemetry.CleanupDLQDepth:        s.CleanupDLQDepth,
		telemetry.NotificationQueueDepth: s.NotificationQueueDepth,
	} {
		n, err := count(ctx)
		if err == nil {
			gauge.Set(float64(n))
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// applyFleet publishes a snapshot's figures to the gauges. The coverage gauges
// are left alone unless the scrub sweep runs, so a fleet nothing verifies
// reports no age that nothing would lower.
func (mc *Collector) applyFleet(snap *FleetSnapshot) {
	applyQuotaGauges(snap.Quota)
	applyPerBackend(telemetry.ObjectCount, snap.Quota, snap.Ledger, func(st core.LedgerStat) int64 { return st.Objects })
	applyPerBackend(telemetry.ActiveMultipartUploads, snap.Quota, snap.Multipart, func(n int64) int64 { return n })
	// Encryption applies to new writes only, so without this nothing reports
	// that a fleet configured for encryption is still partly plaintext.
	telemetry.EncryptionPlaintextCopies.Set(float64(snap.Ledger.PlaintextCopies()))
	telemetry.UnreadableCopies.Set(float64(snap.Ledger.UnreadableCopies()))
	if mc.d.Scrub.Sweeping() {
		telemetry.IntegrityOldestUnverifiedSeconds.Set(snap.Coverage.OldestUnverifiedAge.Seconds())
		telemetry.IntegrityNeverVerifiedCopies.Set(float64(snap.Coverage.NeverVerified))
		telemetry.IntegrityDeferredCopies.Set(float64(snap.Coverage.Deferred))
	}
}

// applyQuotaGauges publishes each backend's quota bytes.
func applyQuotaGauges(stats map[string]core.QuotaStat) {
	for name, stat := range stats {
		telemetry.QuotaBytesUsed.WithLabelValues(name).Set(float64(stat.BytesUsed))
		telemetry.QuotaOrphanBytes.WithLabelValues(name).Set(float64(stat.OrphanBytes))
		if stat.Unlimited() {
			telemetry.QuotaBytesLimit.WithLabelValues(name).Set(0)
			telemetry.QuotaBytesAvailable.WithLabelValues(name).Set(0)
			continue
		}
		telemetry.QuotaBytesLimit.WithLabelValues(name).Set(float64(stat.BytesLimit))
		telemetry.QuotaBytesAvailable.WithLabelValues(name).Set(float64(stat.BytesLimit - stat.Occupied()))
	}
}

// applyPerBackend sets a per-backend gauge to each backend's figure, zeroing
// every known backend first so one that dropped out of values reports zero
// rather than its old figure.
func applyPerBackend[V any](gauge *prometheus.GaugeVec, known map[string]core.QuotaStat, values map[string]V, figure func(V) int64) {
	for name := range known {
		gauge.WithLabelValues(name).Set(0)
	}
	for name, v := range values {
		gauge.WithLabelValues(name).Set(float64(figure(v)))
	}
}

// -------------------------------------------------------------------------
// CAPACITY WARNINGS
// -------------------------------------------------------------------------

// warnNearCapacity emits a slog warning and a capacity event for each backend
// past the capacity threshold, once per fleet per window: under the capacity
// lock, a backend whose shared marker still stands is skipped, and one warned
// for now sets it. Without shared state no marker stands, and the notifier's
// own dampener spaces the events. Operators rely on this signal to expand
// capacity before writes start failing with 507.
func (mc *Collector) warnNearCapacity(ctx context.Context, stats map[string]core.QuotaStat) {
	_, err := mc.d.Store.WithAdvisoryLock(ctx, core.LockCapacityWarning, func(ctx context.Context) error {
		for name, stat := range stats {
			if !stat.Unlimited() && utilization(stat) >= capacityWarningUtilization {
				mc.warnOnce(ctx, name, stat)
			}
		}
		return nil
	})
	if err != nil {
		mc.log.WarnContext(ctx, "capacity warning check failed", "error", err)
	}
}

// warnOnce raises a backend's capacity warning unless its shared marker still
// stands from an earlier one, and sets the marker for the window.
func (mc *Collector) warnOnce(ctx context.Context, name string, stat core.QuotaStat) {
	marker := sharedstate.NewValue[time.Time](mc.d.Shared, "capacity_warning:"+name, capacityWarningWindow)
	if warned, err := marker.Get(ctx); err == nil && warned != nil {
		return
	}
	mc.raiseCapacityWarning(ctx, name, stat)
	if err := marker.Put(ctx, new(time.Now())); err != nil {
		mc.log.WarnContext(ctx, "failed to mark capacity warning", "backend", name, "error", err)
	}
}

// utilization is the share of a backend's quota its bytes occupy.
func utilization(stat core.QuotaStat) float64 {
	return float64(stat.Occupied()) / float64(stat.BytesLimit)
}

// raiseCapacityWarning logs and publishes one backend's capacity warning.
func (mc *Collector) raiseCapacityWarning(ctx context.Context, name string, stat core.QuotaStat) {
	pct := int(utilization(stat) * 100)
	available := stat.BytesLimit - stat.Occupied()
	mc.log.WarnContext(ctx, "backend approaching capacity",
		"backend", name,
		"utilization_pct", pct,
		"bytes_available", available,
		"bytes_limit", stat.BytesLimit)
	event.Publish(event.BackendCapacityWarning, name, map[string]any{
		"backend":         name,
		"utilization_pct": pct,
		"bytes_available": available,
		"bytes_limit":     stat.BytesLimit,
	})
}
