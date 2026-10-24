// -------------------------------------------------------------------------------
// Worker Admission Helper
//
// Author: Alex Freidah
//
// Free helper that wraps the AcquireAdmission / ReleaseAdmission /
// telemetry-rejection triplet every worker repeats around its inner
// per-item loop body.
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
	WorkerNameDrainer         = "drainer"
	WorkerNameOverReplication = "over_replication"
	WorkerNamePendingReaper   = "pending_reaper"
	WorkerNameRebalancer      = "rebalancer"
	WorkerNameReplicator      = "replicator"
)

// WithAdmission acquires an admission slot, runs fn if granted, releases the
// slot afterward, and returns what fn returned. A rejected admission counts on
// the per-worker rejection counter and returns the zero value without running
// fn, which for a batch.ItemResult is a skipped item. name must be one of the
// WorkerName* constants above.
func WithAdmission[T any](ctx context.Context, ac Ops, name string, fn func() T) T {
	if !ac.AcquireAdmission(ctx) {
		telemetry.WorkerAdmissionRejectionsTotal.WithLabelValues(name).Inc()
		var zero T
		return zero
	}
	defer ac.ReleaseAdmission()
	return fn()
}
