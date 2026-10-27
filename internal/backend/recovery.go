// -------------------------------------------------------------------------------
// Backend Recovery - Usage-Charged Health Checks for Open Breakers
//
// Author: Alex Freidah
//
// Turns one of a backend's breaker checks (see CircuitBreakerBackend.Probes)
// into the breaker.Recovery a breaker.RecoveryProber runs. Each check is
// admitted and charged against the backend's usage budget as the operation it
// makes, so an open breaker costs a bounded, accounted trickle of requests and
// never spends a client request as a probe.
// -------------------------------------------------------------------------------

package backend

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
)

// maxRecoveryBackoff caps the interval between health checks of a backend
// that stays down. An open timeout set longer than this is used as the cap
// instead.
const maxRecoveryBackoff = 5 * time.Minute

// UsageGate admits and charges the checks against a backend's usage budget.
// Satisfied by *counter.UsageTracker.
type UsageGate interface {
	WithinLimits(backendName string, op s3op.Operation, egress, ingress int64) bool
	Record(ctx context.Context, backendName string, op s3op.Operation, egress, ingress int64)
}

// Recovery builds the recovery for this probe on backendName: its check,
// backing off to maxRecoveryBackoff, admitted only while the usage budget has
// room for one more call of the probe's operation and charged as that call.
func (p Probe) Recovery(backendName string, usage UsageGate) breaker.Recovery {
	return breaker.Recovery{
		Check:      p.Check,
		MaxBackoff: maxRecoveryBackoff,
		Admit: func(ctx context.Context) bool {
			if !usage.WithinLimits(backendName, p.Op, 0, 0) {
				return false
			}
			usage.Record(ctx, backendName, p.Op, 0, 0)
			return true
		},
	}
}
