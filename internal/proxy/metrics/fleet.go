// -------------------------------------------------------------------------------
// Fleet Snapshot - Fleet-Wide Gauges Computed Once, Served Everywhere
//
// Author: Alex Freidah
//
// The quota, object, multipart, replication and plaintext gauges, and the
// replication status the admin API serves, describe state every instance
// shares. One instance computes them from the store, applies the result, and
// publishes it to shared state; the others apply the published result rather
// than serving whatever they last computed themselves. Without shared state
// there is a single instance, and it computes its own.
// -------------------------------------------------------------------------------

package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"

	"github.com/prometheus/client_golang/prometheus"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// fleetSnapshotKey names the published snapshot in shared state.
const fleetSnapshotKey = "fleet_snapshot"

// fleetSnapshotTTLIntervals is how many fleet intervals a published snapshot
// outlives the last instance that refreshed it.
const fleetSnapshotTTLIntervals = 3

// capacityWarningUtilization is the fraction of a quota at which a backend
// is reported as approaching capacity.
const capacityWarningUtilization = 0.8

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// SharedState is where one instance publishes a fleet-wide result for the
// others to read. *counter.RedisCounterBackend satisfies it.
type SharedState interface {
	PutShared(ctx context.Context, name string, value []byte, ttl time.Duration) error
	GetShared(ctx context.Context, name string) ([]byte, error)
}

// FleetSnapshot is one computation of the fleet-wide gauges. Ledger carries
// every per-backend ledger figure from one pass, so the gauges and the admin
// status read the same numbers. A nil field marks a store read that failed,
// and applying the snapshot leaves the gauges that read feeds as they were.
type FleetSnapshot struct {
	ComputedAt  time.Time                 `json:"computed_at"`
	Quota       map[string]core.QuotaStat `json:"quota"`
	Ledger      core.LedgerStats          `json:"ledger"`
	Multipart   map[string]int64          `json:"multipart"`
	Replication *ReplicationSnapshot      `json:"replication"`
}

// ReplicationSnapshot is the last-computed replication state, retained so a
// cheap admin endpoint can serve it without a fresh ledger scan. Ready is false
// until the first computation has run.
type ReplicationSnapshot struct {
	Factor          int       `json:"factor"`
	UnderReplicated int64     `json:"under_replicated"`
	OverReplicated  int64     `json:"over_replicated"`
	ComputedAt      time.Time `json:"computed_at"`
	Ready           bool      `json:"ready"`
}

// -------------------------------------------------------------------------
// REFRESH
// -------------------------------------------------------------------------

// LoadFleetMetrics applies the snapshot the computing instance last
// published, so an instance that did not compute this round serves the same
// fleet gauges and replication status as the one that did. A no-op without
// shared state, or before any snapshot has been published.
func (mc *Collector) LoadFleetMetrics(ctx context.Context) error {
	snap, err := mc.loadPublished(ctx)
	if err != nil || snap == nil {
		return err
	}
	mc.applyFleet(snap)
	return nil
}

// RefreshFleetIfStale recomputes the fleet snapshot unless another instance
// published one within the last half interval, in which case it applies that
// one. Every instance's tick takes the fleet lock in turn, so without the
// check the ledger would be scanned once per instance per interval.
func (mc *Collector) RefreshFleetIfStale(ctx context.Context) error {
	snap, err := mc.loadPublished(ctx)
	if err != nil {
		mc.log.WarnContext(ctx, "fleet snapshot load failed, recomputing", logfmt.Err(err))
	}
	if snap != nil && time.Since(snap.ComputedAt) < mc.fleetInterval/2 {
		mc.applyFleet(snap)
		return nil
	}
	return mc.UpdateFleetMetrics(ctx)
}

// loadPublished reads the published snapshot. It returns nil without shared
// state or before any snapshot has been published.
func (mc *Collector) loadPublished(ctx context.Context) (*FleetSnapshot, error) {
	if mc.shared == nil {
		return nil, nil
	}
	data, err := mc.shared.GetShared(ctx, fleetSnapshotKey)
	if err != nil || data == nil {
		return nil, err
	}
	var snap FleetSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("decode fleet snapshot: %w", err)
	}
	return &snap, nil
}

// ReplicationSnapshot returns the newest replication state. With shared
// state it reads the published snapshot on every call, so each instance
// answers with the same result whichever of them computed it last; this
// instance's own copy lags by up to one flush interval. It falls back to that
// copy when shared state is absent, unreachable or holds nothing. Ready is
// false until a snapshot has been computed or loaded.
func (mc *Collector) ReplicationSnapshot(ctx context.Context) ReplicationSnapshot {
	if snap, err := mc.loadPublished(ctx); err == nil && snap != nil && snap.Replication != nil {
		return *snap.Replication
	}
	mc.snapMu.RLock()
	defer mc.snapMu.RUnlock()
	return mc.repSnap
}

// FleetSnapshot returns the newest fleet snapshot: the published one, else the
// last one applied here, else one computed now. Callers must not modify it.
func (mc *Collector) FleetSnapshot(ctx context.Context) (*FleetSnapshot, error) {
	if snap, err := mc.loadPublished(ctx); err == nil && snap != nil {
		return snap, nil
	}
	if snap := mc.lastSnapshot(); snap != nil {
		return snap, nil
	}
	if err := mc.UpdateFleetMetrics(ctx); err != nil {
		return nil, err
	}
	return mc.lastSnapshot(), nil
}

// lastSnapshot returns the snapshot last applied on this instance.
func (mc *Collector) lastSnapshot() *FleetSnapshot {
	mc.snapMu.RLock()
	defer mc.snapMu.RUnlock()
	return mc.lastSnap
}

// refreshFleet computes the fleet snapshot from the store, applies it here,
// and publishes it for the other instances.
func (mc *Collector) refreshFleet(ctx context.Context, stats map[string]core.QuotaStat) {
	snap := mc.computeFleet(ctx, stats)
	mc.warnNearCapacity(ctx, stats)
	mc.applyFleet(&snap)
	mc.publishFleet(ctx, &snap)
}

// publishFleet writes the snapshot to shared state. A failure is logged and
// the snapshot still stands on this instance; the others keep the last one
// that reached them.
func (mc *Collector) publishFleet(ctx context.Context, snap *FleetSnapshot) {
	if mc.shared == nil {
		return
	}
	data, err := json.Marshal(snap)
	if err != nil {
		mc.log.ErrorContext(ctx, "failed to encode fleet snapshot", logfmt.Err(err))
		return
	}
	if err := mc.shared.PutShared(ctx, fleetSnapshotKey, data, fleetSnapshotTTLIntervals*mc.fleetInterval); err != nil {
		mc.log.WarnContext(ctx, "failed to publish fleet snapshot", logfmt.Err(err))
	}
}

// -------------------------------------------------------------------------
// COMPUTE
// -------------------------------------------------------------------------

// computeFleet reads every fleet-wide figure from the store.
func (mc *Collector) computeFleet(ctx context.Context, stats map[string]core.QuotaStat) FleetSnapshot {
	snap := FleetSnapshot{Quota: stats}

	// Counted here rather than from the dashboard so the figures keep moving
	// on a deployment that scrapes Prometheus and never opens the web UI.
	if ledger, err := mc.store.LedgerStats(ctx); err != nil {
		mc.log.ErrorContext(ctx, "failed to read ledger stats", logfmt.Err(err))
	} else {
		snap.Ledger = ledger
	}

	if multipart, err := mc.store.GetActiveMultipartCounts(ctx); err != nil {
		mc.log.ErrorContext(ctx, "failed to get multipart upload counts", logfmt.Err(err))
	} else {
		snap.Multipart = multipart
	}

	snap.Replication = mc.computeReplication(ctx)
	snap.ComputedAt = time.Now()
	return snap
}

// computeReplication counts under- and over-replicated objects. Returns nil
// when no factor source is wired (test fixtures that build metrics without a
// replication worker) or when a count fails. With replication disabled it
// returns a ready, zeroed snapshot, so the admin endpoint reports "not
// replicating" rather than "not yet computed".
func (mc *Collector) computeReplication(ctx context.Context) *ReplicationSnapshot {
	if mc.replicationFactor == nil {
		return nil
	}
	factor := mc.replicationFactor()
	if factor <= 1 {
		return &ReplicationSnapshot{Factor: factor, Ready: true, ComputedAt: time.Now()}
	}

	backlog, err := mc.store.CountReplicationBacklog(ctx, factor)
	if err != nil {
		mc.log.ErrorContext(ctx, "failed to count replication backlog", logfmt.Err(err))
		return nil
	}
	return &ReplicationSnapshot{
		Factor:          factor,
		UnderReplicated: backlog.Under,
		OverReplicated:  backlog.Over,
		ComputedAt:      time.Now(),
		Ready:           true,
	}
}

// warnNearCapacity emits a slog warning and a capacity event for each backend
// past the capacity threshold. Only the computing instance does this, so the
// fleet raises one event per crossing rather than one per instance. Operators
// rely on this signal to expand capacity before writes start failing with 507.
func (mc *Collector) warnNearCapacity(ctx context.Context, stats map[string]core.QuotaStat) {
	for name, stat := range stats {
		if stat.BytesLimit == 0 {
			continue
		}
		utilization := float64(stat.BytesUsed+stat.OrphanBytes) / float64(stat.BytesLimit)
		if utilization < capacityWarningUtilization {
			continue
		}
		available := stat.BytesLimit - stat.BytesUsed - stat.OrphanBytes
		mc.log.WarnContext(ctx, "backend approaching capacity",
			"backend", name,
			"utilization_pct", int(utilization*100),
			"bytes_available", available,
			"bytes_limit", stat.BytesLimit)
		event.Publish(event.BackendCapacityWarning, name, map[string]any{
			"backend":         name,
			"utilization_pct": int(utilization * 100),
			"bytes_available": available,
			"bytes_limit":     stat.BytesLimit,
		})
	}
}

// -------------------------------------------------------------------------
// APPLY
// -------------------------------------------------------------------------

// applyFleet publishes a snapshot's figures to the gauges and the replication
// status, whether this instance computed it or loaded it, and keeps it for
// FleetSnapshot.
func (mc *Collector) applyFleet(snap *FleetSnapshot) {
	mc.snapMu.Lock()
	mc.lastSnap = snap
	mc.snapMu.Unlock()
	applyQuotaGauges(snap.Quota)
	if snap.Ledger != nil {
		applyPerBackendCounts(telemetry.ObjectCount, snap.Quota, snap.Ledger.ObjectCounts())
		// Encryption applies to new writes only, so without this nothing
		// reports that a fleet configured for encryption is still partly
		// plaintext.
		telemetry.EncryptionPlaintextCopies.Set(float64(snap.Ledger.PlaintextCopies()))
		telemetry.UnreadableCopies.Set(float64(snap.Ledger.UnreadableCopies()))
	}
	if snap.Multipart != nil {
		applyPerBackendCounts(telemetry.ActiveMultipartUploads, snap.Quota, snap.Multipart)
	}
	if snap.Replication != nil {
		mc.applyReplication(snap.Replication)
	}
}

// applyQuotaGauges publishes each backend's quota bytes.
func applyQuotaGauges(stats map[string]core.QuotaStat) {
	for name, stat := range stats {
		telemetry.QuotaBytesUsed.WithLabelValues(name).Set(float64(stat.BytesUsed))
		telemetry.QuotaOrphanBytes.WithLabelValues(name).Set(float64(stat.OrphanBytes))
		if stat.BytesLimit == 0 {
			telemetry.QuotaBytesLimit.WithLabelValues(name).Set(0)
			telemetry.QuotaBytesAvailable.WithLabelValues(name).Set(0)
			continue
		}
		telemetry.QuotaBytesLimit.WithLabelValues(name).Set(float64(stat.BytesLimit))
		telemetry.QuotaBytesAvailable.WithLabelValues(name).
			Set(float64(stat.BytesLimit - stat.BytesUsed - stat.OrphanBytes))
	}
}

// applyPerBackendCounts resets every known backend to zero before applying the
// counts, so a backend that just lost its last object reports zero rather than
// its old count.
func applyPerBackendCounts(gauge *prometheus.GaugeVec, known map[string]core.QuotaStat, counts map[string]int64) {
	for name := range known {
		gauge.WithLabelValues(name).Set(0)
	}
	for name, count := range counts {
		gauge.WithLabelValues(name).Set(float64(count))
	}
}

// applyReplication publishes the replication gauges and keeps the snapshot
// the admin endpoint falls back to.
func (mc *Collector) applyReplication(rep *ReplicationSnapshot) {
	if rep.Factor > 1 {
		telemetry.ReplicationPending.Set(float64(rep.UnderReplicated))
		telemetry.OverReplicationPending.Set(float64(rep.OverReplicated))
	}
	mc.snapMu.Lock()
	mc.repSnap = *rep
	mc.snapMu.Unlock()
}
