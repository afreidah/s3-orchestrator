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

// Admitter hands out the slots background items run under, each with the func
// that gives it back.
type Admitter interface {
	AcquireAdmission(ctx context.Context) (release func(), admitted bool)
}

// Admit runs fn under a slot of a and reports whether it ran. A refused slot
// is counted under ctx's workload, and fn is not run.
func Admit(ctx context.Context, a Admitter, fn func()) bool {
	release, admitted := a.AcquireAdmission(ctx)
	if !admitted {
		telemetry.WorkerAdmissionRejectionsTotal.WithLabelValues(string(audit.WorkloadOf(ctx))).Inc()
		return false
	}
	defer release()
	fn()
	return true
}
