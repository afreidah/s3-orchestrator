// -------------------------------------------------------------------------------
// MetricsCollector - Prometheus Gauge and Counter Updates
//
// Author: Alex Freidah
//
// Owns Prometheus metric recording for manager operations and periodic gauge
// refreshes from the metadata store. The monthly usage gauges and the usage
// baselines limit checks compare against live here; the fleet-wide gauges live
// in fleet.go.
// -------------------------------------------------------------------------------

package metrics

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/sharedstate"
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
	GetLeastRecentlyScrubbedObjects(ctx context.Context, limit int, backends []string, scrubbedBefore time.Time) ([]core.ObjectLocation, error)
	PendingDepth(ctx context.Context) (int64, error)
	CleanupQueueDepth(ctx context.Context) (int64, error)
	CleanupDLQDepth(ctx context.Context) (int64, error)
	CountReplicationQueue(ctx context.Context) (core.ReplicationBacklog, error)
	NotificationQueueDepth(ctx context.Context) (int64, error)
	core.UsageReader
	core.AdvisoryLocker
}

// ScrubSweep names the backends the scrub queue draws from right now, and says
// whether the sweep runs at all. *worker.Scrubber satisfies it.
type ScrubSweep interface {
	ReachableBackends() []string
	Sweeping() bool
}

// Collector records Prometheus metrics for manager-level operations and
// periodically refreshes gauge values from the metadata store.
type Collector struct {
	d   CollectorDeps
	log *slog.Logger
}

// CollectorDeps groups the metrics collector's constructor parameters. Scrub
// scopes the coverage figures. Shared is where worker gauges and capacity
// warning markers are published and read; nil, or one without a store, is a
// single instance.
type CollectorDeps struct {
	Store  Deps
	Usage  *counter.UsageTracker
	Scrub  ScrubSweep
	Shared *sharedstate.Shared
}

// New creates a Collector with references to the store and usage tracker
// needed for gauge refreshes.
func New(d *CollectorDeps) *Collector {
	return &Collector{d: *d, log: logfmt.Logger("metrics_collector")}
}

// -------------------------------------------------------------------------
// PERIODIC REFRESH
// -------------------------------------------------------------------------

// RefreshGauges republishes the gauges the store backs: the fleet-wide figures,
// the queue depths, the capacity warnings due, and this instance's usage with
// the baselines its limit checks compare against. stats is a quota read the
// caller already made, or nil to read it here.
func (mc *Collector) RefreshGauges(ctx context.Context, stats map[string]core.QuotaStat) error {
	if stats == nil {
		read, err := mc.d.Store.GetQuotaStats(ctx)
		if err != nil {
			return err
		}
		stats = read
	}
	fleetErr := mc.refreshFleet(ctx, stats)
	queueErr := mc.refreshQueues(ctx)
	mc.updateUsageGauges(ctx, stats)
	return errors.Join(fleetErr, queueErr)
}

// updateUsageGauges refreshes the tracker's cycle baselines, which the
// in-process limit checks use, and republishes the usage gauges from them.
func (mc *Collector) updateUsageGauges(ctx context.Context, stats map[string]core.QuotaStat) {
	byWorkload, err := mc.d.Usage.RefreshUsage(ctx, mc.d.Store)
	if err != nil {
		mc.log.ErrorContext(ctx, "failed to get usage stats", "error", err)
		return
	}
	usage := core.Totals(byWorkload)
	applyPerBackend(telemetry.UsageAPIRequests, stats, usage, func(u core.Usage) int64 { return u.APIRequests })
	applyPerBackend(telemetry.UsageEgressBytes, stats, usage, func(u core.Usage) int64 { return u.EgressBytes })
	applyPerBackend(telemetry.UsageIngressBytes, stats, usage, func(u core.Usage) int64 { return u.IngressBytes })
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
	limits := mc.d.Usage.GetLimits()
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
