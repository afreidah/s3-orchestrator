// -------------------------------------------------------------------------------
// MetricsCollector - Prometheus Gauge and Counter Updates
//
// Author: Alex Freidah
//
// Owns Prometheus metric recording for manager operations and periodic gauge
// refreshes from PostgreSQL. Reads quota stats, object counts, multipart counts,
// and monthly usage from the store and updates the corresponding gauges.
// -------------------------------------------------------------------------------

package storage

import (
	"context"
	"log/slog"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/telemetry"
)

// MetricsCollector records Prometheus metrics for manager-level operations
// and periodically refreshes gauge values from the metadata store.
type MetricsCollector struct {
	store        MetadataStore
	usage        *UsageTracker
	backendNames []string
}

// NewMetricsCollector creates a MetricsCollector with references to the store
// and usage tracker needed for gauge refreshes.
func NewMetricsCollector(store MetadataStore, usage *UsageTracker, backendNames []string) *MetricsCollector {
	return &MetricsCollector{
		store:        store,
		usage:        usage,
		backendNames: backendNames,
	}
}

// RecordOperation updates Prometheus request count and duration metrics
// for a single manager operation.
func (mc *MetricsCollector) RecordOperation(operation, backend string, start time.Time, err error) {
	status := "success"
	if err != nil {
		status = "error"
	}

	telemetry.ManagerRequestsTotal.WithLabelValues(operation, backend, status).Inc()
	telemetry.ManagerDuration.WithLabelValues(operation, backend).Observe(time.Since(start).Seconds())
}

// UpdateQuotaMetrics fetches quota stats, object counts, active multipart
// upload counts, and monthly usage, then updates the corresponding Prometheus
// gauges and caches usage baselines for limit enforcement.
func (mc *MetricsCollector) UpdateQuotaMetrics(ctx context.Context) error {
	stats, err := mc.store.GetQuotaStats(ctx)
	if err != nil {
		return err
	}

	for name, stat := range stats {
		telemetry.QuotaBytesUsed.WithLabelValues(name).Set(float64(stat.BytesUsed))
		if stat.BytesLimit == 0 {
			telemetry.QuotaBytesLimit.WithLabelValues(name).Set(0)
			telemetry.QuotaBytesAvailable.WithLabelValues(name).Set(0)
		} else {
			telemetry.QuotaBytesLimit.WithLabelValues(name).Set(float64(stat.BytesLimit))
			telemetry.QuotaBytesAvailable.WithLabelValues(name).Set(float64(stat.BytesLimit - stat.BytesUsed))
		}
	}

	// --- Object counts per backend ---
	objCounts, err := mc.store.GetObjectCounts(ctx)
	if err != nil {
		slog.Error("Failed to get object counts", "error", err)
	} else {
		for name := range stats {
			telemetry.ObjectCount.WithLabelValues(name).Set(0)
		}
		for name, count := range objCounts {
			telemetry.ObjectCount.WithLabelValues(name).Set(float64(count))
		}
	}

	// --- Active multipart uploads per backend ---
	mpCounts, err := mc.store.GetActiveMultipartCounts(ctx)
	if err != nil {
		slog.Error("Failed to get multipart upload counts", "error", err)
	} else {
		for name := range stats {
			telemetry.ActiveMultipartUploads.WithLabelValues(name).Set(0)
		}
		for name, count := range mpCounts {
			telemetry.ActiveMultipartUploads.WithLabelValues(name).Set(float64(count))
		}
	}

	// --- Monthly usage per backend ---
	usage, err := mc.store.GetUsageForPeriod(ctx, currentPeriod())
	if err != nil {
		slog.Error("Failed to get usage stats", "error", err)
	} else {
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

		// Cache baseline for usage limit enforcement. Reset all backends
		// first so period rollover (new month with no rows) zeroes out.
		mc.usage.ResetBaselines(mc.backendNames)
		for name, u := range usage {
			mc.usage.SetBaseline(name, u)
		}
	}

	return nil
}
