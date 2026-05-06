// -------------------------------------------------------------------------------
// MetricsCollector - Prometheus Gauge and Counter Updates
//
// Author: Alex Freidah
//
// Owns Prometheus metric recording for manager operations and periodic gauge
// refreshes from PostgreSQL. Reads quota stats, object counts, multipart counts,
// and monthly usage from the store and updates the corresponding gauges.
// -------------------------------------------------------------------------------

// Package metrics owns the Prometheus gauge/counter recording for the proxy
// package. It exposes a Collector that the orchestrator embeds to refresh
// gauge values from the metadata store and record per-operation metrics.
package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
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
type Deps interface {
	GetQuotaStats(ctx context.Context) (map[string]core.QuotaStat, error)
	GetObjectCounts(ctx context.Context) (map[string]int64, error)
	GetActiveMultipartCounts(ctx context.Context) (map[string]int64, error)
	GetUsageForPeriod(ctx context.Context, period string) (map[string]core.UsageStat, error)
	GetUnderReplicatedObjects(ctx context.Context, factor, limit int) ([]core.ObjectLocation, error)
}

// Collector records Prometheus metrics for manager-level operations and
// periodically refreshes gauge values from the metadata store.
type Collector struct {
	store             Deps
	usage             *counter.UsageTracker
	backendNames      []string
	replicationFactor func() int // returns 0 when replication is disabled
}

// New creates a Collector with references to the store and usage tracker
// needed for gauge refreshes.
func New(store Deps, usage *counter.UsageTracker, backendNames []string, replicationFactor func() int) *Collector {
	return &Collector{
		store:             store,
		usage:             usage,
		backendNames:      backendNames,
		replicationFactor: replicationFactor,
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

// UpdateQuotaMetrics fetches quota stats, object counts, active multipart
// upload counts, and monthly usage, then updates the corresponding
// Prometheus gauges and caches usage baselines for limit enforcement.
func (mc *Collector) UpdateQuotaMetrics(ctx context.Context) error {
	stats, err := mc.store.GetQuotaStats(ctx)
	if err != nil {
		return err
	}
	mc.updateQuotaGauges(ctx, stats)
	mc.updateObjectCountGauges(ctx, stats)
	mc.updateMultipartCountGauges(ctx, stats)
	mc.updateUsageGauges(ctx, stats)
	mc.updateReplicationPending(ctx)
	return nil
}

// updateQuotaGauges sets per-backend quota bytes gauges and emits a
// capacity-warning event when utilization crosses 80%.
func (mc *Collector) updateQuotaGauges(ctx context.Context, stats map[string]core.QuotaStat) {
	for name, stat := range stats {
		telemetry.QuotaBytesUsed.WithLabelValues(name).Set(float64(stat.BytesUsed))
		telemetry.QuotaOrphanBytes.WithLabelValues(name).Set(float64(stat.OrphanBytes))
		if stat.BytesLimit == 0 {
			telemetry.QuotaBytesLimit.WithLabelValues(name).Set(0)
			telemetry.QuotaBytesAvailable.WithLabelValues(name).Set(0)
			continue
		}
		telemetry.QuotaBytesLimit.WithLabelValues(name).Set(float64(stat.BytesLimit))
		available := stat.BytesLimit - stat.BytesUsed - stat.OrphanBytes
		telemetry.QuotaBytesAvailable.WithLabelValues(name).Set(float64(available))
		mc.maybeEmitCapacityWarning(ctx, name, &stat, available)
	}
}

// maybeEmitCapacityWarning emits a slog warning and a capacity event when
// the backend has crossed 80% utilization. Operators rely on this signal
// to expand capacity before writes start failing with 507.
func (mc *Collector) maybeEmitCapacityWarning(ctx context.Context, name string, stat *core.QuotaStat, available int64) {
	utilization := float64(stat.BytesUsed+stat.OrphanBytes) / float64(stat.BytesLimit)
	if utilization < 0.8 {
		return
	}
	slog.WarnContext(ctx, "backend approaching capacity",
		"backend", name,
		"utilization_pct", int(utilization*100),
		"bytes_available", available,
		"bytes_limit", stat.BytesLimit)
	if event.Emit == nil {
		return
	}
	event.Emit(event.Event{
		Type:    event.BackendCapacityWarning,
		Subject: name,
		Data: map[string]any{
			"backend":         name,
			"utilization_pct": int(utilization * 100),
			"bytes_available": available,
			"bytes_limit":     stat.BytesLimit,
		},
	})
}

// updateObjectCountGauges resets every known backend's object count to
// zero before applying the live counts so a backend that just lost its
// last object reports as zero rather than retaining the stale value.
func (mc *Collector) updateObjectCountGauges(ctx context.Context, stats map[string]core.QuotaStat) {
	objCounts, err := mc.store.GetObjectCounts(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to get object counts", "error", err)
		return
	}
	for name := range stats {
		telemetry.ObjectCount.WithLabelValues(name).Set(0)
	}
	for name, count := range objCounts {
		telemetry.ObjectCount.WithLabelValues(name).Set(float64(count))
	}
}

// updateMultipartCountGauges follows the same reset-then-set pattern as
// updateObjectCountGauges for active multipart uploads.
func (mc *Collector) updateMultipartCountGauges(ctx context.Context, stats map[string]core.QuotaStat) {
	mpCounts, err := mc.store.GetActiveMultipartCounts(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to get multipart upload counts", "error", err)
		return
	}
	for name := range stats {
		telemetry.ActiveMultipartUploads.WithLabelValues(name).Set(0)
	}
	for name, count := range mpCounts {
		telemetry.ActiveMultipartUploads.WithLabelValues(name).Set(float64(count))
	}
}

// updateUsageGauges refreshes the monthly usage gauges and seeds the
// usage tracker baselines used by the in-process limit checks.
func (mc *Collector) updateUsageGauges(ctx context.Context, stats map[string]core.QuotaStat) {
	usage, err := mc.store.GetUsageForPeriod(ctx, counter.CurrentPeriod())
	if err != nil {
		slog.ErrorContext(ctx, "failed to get usage stats", "error", err)
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
		mc.usage.SetBaseline(name, u)
	}
}

// updateReplicationPending updates the under-replicated-objects gauge.
// No-op when replication is disabled (factor <= 1) or when no factor
// source has been wired (the closure is nil in test fixtures that build
// metrics without a BackendManager).
func (mc *Collector) updateReplicationPending(ctx context.Context) {
	if mc.replicationFactor == nil {
		return
	}
	factor := mc.replicationFactor()
	if factor <= 1 {
		return
	}
	locations, err := mc.store.GetUnderReplicatedObjects(ctx, factor, 10000)
	if err != nil {
		slog.ErrorContext(ctx, "failed to get under-replicated objects", "error", err)
		return
	}
	telemetry.ReplicationPending.Set(float64(len(core.GroupByKey(locations))))
}
