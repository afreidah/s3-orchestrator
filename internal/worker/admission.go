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

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// WithAdmission acquires an admission slot, runs fn if granted, releases the
// slot afterward, and returns what fn returned. A rejected admission counts on
// the rejection counter under ctx's workload and returns the zero value
// without running fn, which for a batch.ItemResult is a skipped item.
func WithAdmission[T any](ctx context.Context, ac Ops, fn func() T) T {
	if !ac.AcquireAdmission(ctx) {
		telemetry.WorkerAdmissionRejectionsTotal.WithLabelValues(string(audit.WorkloadOf(ctx))).Inc()
		var zero T
		return zero
	}
	defer ac.ReleaseAdmission()
	return fn()
}
