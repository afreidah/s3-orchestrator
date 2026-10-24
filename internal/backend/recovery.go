// -------------------------------------------------------------------------------
// RecoveryProber - Out-of-Band Recovery for Backend Circuit Breakers
//
// Author: Alex Freidah
//
// Drives one of a backend's open breakers back to closed by running that
// breaker's own check (see CircuitBreakerBackend.Probes). The breaker watchdog
// calls Probe on every tick; while the circuit is closed that does nothing.
// Once it opens, the check runs after the open timeout, then after doubling
// intervals up to maxRecoveryBackoff, until one passes and the breaker
// recovers. Each check is admitted and charged against the backend's usage
// budget as the operation it makes, so an open breaker costs a bounded,
// accounted trickle of requests and never spends a client request as a probe.
// -------------------------------------------------------------------------------

package backend

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
)

// maxRecoveryBackoff caps the interval between health checks of a backend
// that stays down. An open timeout set longer than this is used as the cap
// instead.
const maxRecoveryBackoff = 5 * time.Minute

// healthCheckTimeout bounds one check so a hung backend cannot stall the
// watchdog tick.
const healthCheckTimeout = 10 * time.Second

// UsageGate admits and charges the checks against a backend's usage budget.
// Satisfied by *counter.UsageTracker.
type UsageGate interface {
	WithinLimits(backendName string, op s3op.Operation, egress, ingress int64) bool
	Record(ctx context.Context, backendName string, op s3op.Operation, egress, ingress int64)
}

// RecoveryProber runs one breaker's check on a backoff schedule while it is
// open and recovers it once the check passes.
type RecoveryProber struct {
	backend string
	probe   Probe
	usage   UsageGate
	log     *slog.Logger
	now     func() time.Time

	mu      sync.Mutex
	nextDue time.Time
	backoff time.Duration
}

// Compile-time check.
var _ breaker.Prober = (*RecoveryProber)(nil)

// NewRecoveryProber builds the prober for one of backendName's breakers,
// admitting and charging each check through usage.
func NewRecoveryProber(backendName string, probe Probe, usage UsageGate) *RecoveryProber {
	return &RecoveryProber{
		backend: backendName,
		probe:   probe,
		usage:   usage,
		log:     slog.Default().With(logfmt.Component("circuit_breaker"), "breaker_name", probe.Breaker.Name()),
		now:     time.Now,
	}
}

// Probe implements breaker.Prober. While the circuit is open and a check is
// due, it runs the check once: success recovers the breaker, failure doubles
// the wait before the next one. A check the usage budget would refuse is
// skipped without being rescheduled, so it runs as soon as there is room.
func (p *RecoveryProber) Probe(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()

	cb := p.probe.Breaker
	if cb.State() != breaker.StateOpen {
		p.nextDue, p.backoff = time.Time{}, 0
		return
	}
	now := p.now()
	if p.nextDue.IsZero() {
		p.backoff = max(cb.RecoveryDelay(), breaker.DefaultWatchdogInterval)
		p.nextDue = now.Add(p.backoff)
		return
	}
	if now.Before(p.nextDue) {
		return
	}

	if !p.usage.WithinLimits(p.backend, p.probe.Op, 0, 0) {
		return
	}
	p.usage.Record(ctx, p.backend, p.probe.Op, 0, 0)

	checkCtx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	err := p.probe.Check(checkCtx)
	cancel()
	if err == nil {
		cb.Recover()
		p.nextDue, p.backoff = time.Time{}, 0
		return
	}

	p.backoff = min(p.backoff*2, max(maxRecoveryBackoff, cb.RecoveryDelay()))
	p.nextDue = now.Add(p.backoff)
	p.log.WarnContext(ctx, "recovery check failed; circuit stays open",
		logfmt.Err(err),
		"operation", p.probe.Op.String(),
		"next_check_in", p.backoff)
}
