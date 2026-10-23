// -------------------------------------------------------------------------------
// MetricsCollector - Prometheus Gauge and Counter Updates
//
// Author: Alex Freidah
//
// Owns Prometheus metric recording for manager operations and periodic gauge
// refreshes from the metadata store. The monthly usage gauges and the usage
// baselines limit checks compare against live here; the fleet-wide gauges and
// the snapshot shared between instances live in fleet.go.
// -------------------------------------------------------------------------------

package metrics

import (
	"cmp"
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Deps is the narrow store surface Collector needs to refresh Prometheus
// gauges. Defined here  -  at the consumer  -  rather than in the store
// package: adding a new metric is a Collector concern, not a store-package
// concern.
//go:generate mockgen -destination=mock_test.go -package=metrics github.com/afreidah/s3-orchestrator/internal/proxy/metrics Deps

type Deps interface {
	GetQuotaStats(ctx context.Context) (map[string]core.QuotaStat, error)
	LedgerStats(ctx context.Context) (core.LedgerStats, error)
	GetActiveMultipartCounts(ctx context.Context) (map[string]int64, error)
	core.UsageReader
	CountReplicationBacklog(ctx context.Context, factor int) (core.ReplicationBacklog, error)
}

// Collector records Prometheus metrics for manager-level operations and
// periodically refreshes gauge values from the metadata store.
type Collector struct {
	store             Deps
	usage             *counter.UsageTracker
	replicationFactor func() int  // returns 0 when replication is disabled
	shared            SharedState // nil on a single instance
	fleetInterval     time.Duration
	log               *slog.Logger

	snapMu   sync.RWMutex        // guards repSnap and lastSnap
	repSnap  ReplicationSnapshot // last-computed replication state, served to admin
	lastSnap *FleetSnapshot      // last snapshot applied here, computed or loaded
}

// CollectorDeps groups the metrics collector's constructor parameters.
// ReplicationFactor returns 0 when replication is disabled. Shared is where
// the fleet snapshot is published and read, and is nil on a single instance.
// FleetInterval is the snapshot recompute cadence; zero means
// config.DefaultFleetInterval.
type CollectorDeps struct {
	Store             Deps
	Usage             *counter.UsageTracker
	ReplicationFactor func() int
	Shared            SharedState
	FleetInterval     time.Duration
}

// New creates a Collector with references to the store and usage tracker
// needed for gauge refreshes.
func New(deps *CollectorDeps) *Collector {
	return &Collector{
		store:             deps.Store,
		usage:             deps.Usage,
		replicationFactor: deps.ReplicationFactor,
		shared:            deps.Shared,
		fleetInterval:     cmp.Or(deps.FleetInterval, config.DefaultFleetInterval),
		log:               slog.Default().With(logfmt.Component("metrics_collector")),
	}
}

// -------------------------------------------------------------------------
// PER-OPERATION RECORDING
// -------------------------------------------------------------------------

// RecordOperation updates Prometheus request count and duration metrics
// for a single manager operation.
func (mc *Collector) RecordOperation(operation, backend string, start time.Time, err error) {
	status := "success"
	if err != nil {
		status = "error"
	}

	telemetry.ManagerRequestsTotal.WithLabelValues(operation, backend, status).Inc()
	telemetry.ManagerDuration.WithLabelValues(operation, backend).Observe(time.Since(start).Seconds())
}

// -------------------------------------------------------------------------
// PERIODIC REFRESH
// -------------------------------------------------------------------------

// UpdateQuotaMetrics runs a full refresh: the fleet gauges and this
// instance's usage baselines.
func (mc *Collector) UpdateQuotaMetrics(ctx context.Context) error {
	stats, err := mc.store.GetQuotaStats(ctx)
	if err != nil {
		return err
	}
	mc.refreshFleet(ctx, stats)
	mc.updateUsageGauges(ctx, stats)
	return nil
}

// UpdateFleetMetrics computes the fleet snapshot - quota bytes, object and
// multipart counts, replication state and plaintext copies - applies it, and
// publishes it for the other instances, which load it with LoadFleetMetrics.
// Every instance would read the same values from the store, so with several
// instances only one runs it.
func (mc *Collector) UpdateFleetMetrics(ctx context.Context) error {
	stats, err := mc.store.GetQuotaStats(ctx)
	if err != nil {
		return err
	}
	mc.refreshFleet(ctx, stats)
	return nil
}

// RefreshUsageBaselines reloads this period's usage from the store into the
// tracker's baselines and republishes the usage gauges. Limit checks compare
// against these baselines, so every instance must run it, whether or not it
// flushed the counters itself.
func (mc *Collector) RefreshUsageBaselines(ctx context.Context) error {
	stats, err := mc.store.GetQuotaStats(ctx)
	if err != nil {
		return err
	}
	mc.updateUsageGauges(ctx, stats)
	return nil
}

// updateUsageGauges refreshes the tracker's cycle baselines, which the
// in-process limit checks use, and republishes the usage gauges from them.
func (mc *Collector) updateUsageGauges(ctx context.Context, stats map[string]core.QuotaStat) {
	byWorkload, err := mc.usage.RefreshUsage(ctx, mc.store)
	if err != nil {
		mc.log.ErrorContext(ctx, "failed to get usage stats", "error", err)
		return
	}
	usage := core.Totals(byWorkload)
	for name := range stats {
		telemetry.UsageAPIRequests.WithLabelValues(name).Set(0)
		telemetry.UsageEgressBytes.WithLabelValues(name).Set(0)
		telemetry.UsageIngressBytes.WithLabelValues(name).Set(0)
	}
	for name, u := range usage {
		telemetry.UsageAPIRequests.WithLabelValues(name).Set(float64(u.APIRequests))
		telemetry.UsageEgressBytes.WithLabelValues(name).Set(float64(u.EgressBytes))
		telemetry.UsageIngressBytes.WithLabelValues(name).Set(float64(u.IngressBytes))
	}
	mc.updateLimitGauges(usage)
	updateWorkloadGauges(byWorkload)
}

// updateWorkloadGauges publishes each backend's usage split by workload. The
// vectors are reset first so a workload with no rows this month drops out.
func updateWorkloadGauges(usage map[string]core.WorkloadUsage) {
	telemetry.UsageWorkloadAPIRequests.Reset()
	telemetry.UsageWorkloadBytes.Reset()
	for name, workloads := range usage {
		for workload, u := range workloads {
			telemetry.UsageWorkloadAPIRequests.WithLabelValues(name, workload).Set(float64(u.APIRequests))
			telemetry.UsageWorkloadBytes.WithLabelValues(name, workload, "egress").Set(float64(u.EgressBytes))
			telemetry.UsageWorkloadBytes.WithLabelValues(name, workload, "ingress").Set(float64(u.IngressBytes))
		}
	}
}

// updateLimitGauges publishes each backend's limits, its per-pool request
// counts with their ceilings, and when its cycle next resets, so an operator
// sees which limit is close to refusing work and how long it has left.
func (mc *Collector) updateLimitGauges(usage map[string]core.Usage) {
	limits := mc.usage.GetLimits()
	now := time.Now()
	for name, lim := range limits {
		telemetry.UsageCycleResetTimestamp.WithLabelValues(name).Set(float64(lim.Period.Cycle(now).Next.Unix()))
		telemetry.UsageEgressLimitBytes.WithLabelValues(name).Set(float64(lim.EgressByteLimit))
		telemetry.UsageIngressLimitBytes.WithLabelValues(name).Set(float64(lim.IngressByteLimit))
		for _, pool := range lim.Pools() {
			telemetry.UsagePoolRequests.WithLabelValues(name, pool.Name).Set(float64(usage[name].Pools[pool.Name]))
			telemetry.UsagePoolLimit.WithLabelValues(name, pool.Name).Set(float64(pool.Limit))
		}
	}
}
