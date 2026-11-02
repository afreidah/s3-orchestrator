// -------------------------------------------------------------------------------
// Admission - Background Work Competing for Backend Capacity
//
// Author: Alex Freidah
//
// Background items take the same admission slots client requests do, so a
// worker cannot crowd out the traffic it serves. An item refused a slot is
// skipped, not failed: the work is still there for the next cycle.
// -------------------------------------------------------------------------------

package batch

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// Admission hands out the slots background items run under.
type Admission interface {
	AcquireAdmission(ctx context.Context) bool
	ReleaseAdmission()
}

// Admit runs fn under a slot of a and reports whether it ran. A refused slot
// is counted under ctx's workload, and fn is not run.
func Admit(ctx context.Context, a Admission, fn func()) bool {
	if !a.AcquireAdmission(ctx) {
		telemetry.WorkerAdmissionRejectionsTotal.WithLabelValues(string(audit.WorkloadOf(ctx))).Inc()
		return false
	}
	defer a.ReleaseAdmission()
	fn()
	return true
}
