// -------------------------------------------------------------------------------
// Worker Admission Helper
//
// Author: Alex Freidah
//
// Free helper that wraps the AcquireAdmission / ReleaseAdmission /
// telemetry-rejection triplet every worker repeats around its inner
// per-item loop body. Kept as a free function rather than an interface
// method so the existing mocks need no regeneration.
// -------------------------------------------------------------------------------

package worker

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// Worker name labels used by admission-rejection metric and other
// worker-scoped telemetry. Constants prevent the silent label drift
// that would otherwise split a metric's time series in Grafana when
// a typo lands.
const (
	WorkerNameCleanup         = "cleanup"
	WorkerNameOverReplication = "over_replication"
	WorkerNamePendingReaper   = "pending_reaper"
	WorkerNameRebalancer      = "rebalancer"
	WorkerNameReplicator      = "replicator"
)

// WithAdmission acquires an admission slot, runs fn if granted, and
// releases the slot afterward. When admission is rejected, increments
// the per-worker rejection counter and returns without invoking fn.
// name must be one of the WorkerName* constants above.
func WithAdmission(ctx context.Context, ac AdmissionControl, name string, fn func()) {
	if !ac.AcquireAdmission(ctx) {
		telemetry.WorkerAdmissionRejectionsTotal.WithLabelValues(name).Inc()
		return
	}
	defer ac.ReleaseAdmission()
	fn()
}
