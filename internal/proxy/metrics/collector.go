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
	"context"
	"log/slog"
	"sync"
	"time"

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
	GetObjectCounts(ctx context.Context) (map[string]int64, error)
	GetActiveMultipartCounts(ctx context.Context) (map[string]int64, error)
	GetUsageForPeriod(ctx context.Context, period string) (map[string]core.UsageStat, error)
	GetPoolUsageForPeriod(ctx context.Context, period string) (map[string]core.PoolUsage, error)
	GetUnderReplicatedObjects(ctx context.Context, factor, limit int) ([]core.ObjectLocation, error)
	CountOverReplicatedObjects(ctx context.Context, factor int) (int64, error)
	CountUnencryptedLocations(ctx context.Context) (int64, error)
}

// Collector records Prometheus metrics for manager-level operations and
// periodically refreshes gauge values from the metadata store.
type Collector struct {
	store             Deps
	usage             *counter.UsageTracker
	backendNames      []string
	replicationFactor func() int  // returns 0 when replication is disabled
	shared            SharedState // nil on a single instance
	log               *slog.Logger

	repMu   sync.RWMutex        // guards repSnap
	repSnap ReplicationSnapshot // last-computed replication state, served to admin
}

// CollectorDeps groups the metrics collector's constructor parameters.
// ReplicationFactor returns 0 when replication is disabled. Shared is where
// the fleet snapshot is published and read, and is nil on a single instance.
type CollectorDeps struct {
	Store             Deps
	Usage             *counter.UsageTracker
	BackendNames      []string
	ReplicationFactor func() int
	Shared            SharedState
}

// New creates a Collector with references to the store and usage tracker
// needed for gauge refreshes.
func New(deps CollectorDeps) *Collector {
	return &Collector{
		store:             deps.Store,
		usage:             deps.Usage,
		backendNames:      deps.BackendNames,
		replicationFactor: deps.ReplicationFactor,
		shared:            deps.Shared,
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

// updateUsageGauges refreshes the monthly usage gauges and seeds the
// usage tracker baselines used by the in-process limit checks.
func (mc *Collector) updateUsageGauges(ctx context.Context, stats map[string]core.QuotaStat) {
	period := counter.CurrentPeriod()
	usage, err := mc.store.GetUsageForPeriod(ctx, period)
	if err != nil {
		mc.log.ErrorContext(ctx, "failed to get usage stats", "error", err)
		return
	}
	// Fetched alongside the totals rather than on its own tick: the two
	// baselines are compared against the same counters, and seeding one
	// without the other admits work against a budget it has already spent.
	pools, err := mc.store.GetPoolUsageForPeriod(ctx, period)
	if err != nil {
		mc.log.ErrorContext(ctx, "failed to get request pool usage", "error", err)
		return
	}
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
	// Reset all baselines first so period rollover (new month with no
	// rows) zeroes out before the new period's values get cached.
	mc.usage.ResetBaselines(mc.backendNames)
	for name, u := range usage {
		mc.usage.SetBaseline(name, u, pools[name])
	}
	mc.updatePoolGauges(pools)
}

// updatePoolGauges publishes the per-pool request counts and the ceilings they
// are judged against, so an operator can see which budget is close to
// refusing work rather than only that the backend stopped accepting it.
func (mc *Collector) updatePoolGauges(pools map[string]core.PoolUsage) {
	limits := mc.usage.GetLimits()
	for name, lim := range limits {
		for _, pool := range lim.Pools() {
			telemetry.UsagePoolRequests.WithLabelValues(name, pool.Name).Set(float64(pools[name][pool.Name]))
			telemetry.UsagePoolLimit.WithLabelValues(name, pool.Name).Set(float64(pool.Limit))
		}
	}
}
