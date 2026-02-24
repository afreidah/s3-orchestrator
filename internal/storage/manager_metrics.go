// -------------------------------------------------------------------------------
// Metrics Recording - Quota and Usage Gauge Updates
//
// Author: Alex Freidah
//
// Periodic refresh of Prometheus gauge metrics from PostgreSQL. Reads quota stats,
// object counts, multipart counts, and monthly usage from the store and updates
// the corresponding Prometheus gauges. Called every 30 seconds by the main loop.
// -------------------------------------------------------------------------------

package storage

import (
	"context"
	"log/slog"

	"github.com/afreidah/s3-orchestrator/internal/telemetry"
)

// -------------------------------------------------------------------------
// QUOTA METRICS
// -------------------------------------------------------------------------

// UpdateQuotaMetrics fetches quota stats, object counts, and active multipart
// upload counts, then updates the corresponding Prometheus gauges.
func (m *BackendManager) UpdateQuotaMetrics(ctx context.Context) error {
	stats, err := m.store.GetQuotaStats(ctx)
	if err != nil {
		return err
	}

	for name, stat := range stats {
		telemetry.QuotaBytesUsed.WithLabelValues(name).Set(float64(stat.BytesUsed))
		if stat.BytesLimit == 0 {
			// Unlimited — no meaningful limit or available metric
			telemetry.QuotaBytesLimit.WithLabelValues(name).Set(0)
			telemetry.QuotaBytesAvailable.WithLabelValues(name).Set(0)
		} else {
			telemetry.QuotaBytesLimit.WithLabelValues(name).Set(float64(stat.BytesLimit))
			telemetry.QuotaBytesAvailable.WithLabelValues(name).Set(float64(stat.BytesLimit - stat.BytesUsed))
		}
	}

	// --- Object counts per backend ---
	objCounts, err := m.store.GetObjectCounts(ctx)
	if err != nil {
		slog.Error("Failed to get object counts", "error", err)
	} else {
		// Reset to zero for backends with no objects, then set actual counts
		for name := range stats {
			telemetry.ObjectCount.WithLabelValues(name).Set(0)
		}
		for name, count := range objCounts {
			telemetry.ObjectCount.WithLabelValues(name).Set(float64(count))
		}
	}

	// --- Active multipart uploads per backend ---
	mpCounts, err := m.store.GetActiveMultipartCounts(ctx)
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
	usage, err := m.store.GetUsageForPeriod(ctx, currentPeriod())
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
		m.usageBaselineMu.Lock()
		for name := range m.backends {
			m.usageBaseline[name] = UsageStat{}
		}
		for name, u := range usage {
			m.usageBaseline[name] = u
		}
		m.usageBaselineMu.Unlock()
	}

	return nil
}
