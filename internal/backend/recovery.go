// -------------------------------------------------------------------------------
// RecoveryProber - Out-of-Band Recovery for Backend Circuit Breakers
//
// Author: Alex Freidah
//
// Drives an open backend breaker back to closed. The breaker watchdog calls
// Probe on every tick; while the circuit is closed that does nothing. Once it
// opens, a HeadBucket health check runs after the open timeout, then after
// doubling intervals up to maxRecoveryBackoff, until one passes and the
// breaker recovers. Each check is admitted and charged against the backend's
// usage budget like any other call, so an open breaker costs a bounded,
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

// healthCheckTimeout bounds one health check so a hung backend cannot stall
// the watchdog tick.
const healthCheckTimeout = 10 * time.Second

// healthCheckOps is the operation a health check is admitted and charged as.
var healthCheckOps = []s3op.Operation{s3op.HeadBucket}

// UsageGate admits and charges the health checks against a backend's usage
// budget. Satisfied by *counter.UsageTracker.
type UsageGate interface {
	WithinLimits(backendName string, ops []s3op.Operation, egress, ingress int64) bool
	Record(backendName string, op s3op.Operation, egress, ingress int64)
}

// RecoveryProber health-checks one open backend breaker on a backoff
// schedule and recovers it once the backend answers.
type RecoveryProber struct {
	cb    *CircuitBreakerBackend
	usage UsageGate
	log   *slog.Logger
	now   func() time.Time

	mu      sync.Mutex
	nextDue time.Time
	backoff time.Duration
}

// Compile-time check.
var _ breaker.Prober = (*RecoveryProber)(nil)

// NewRecoveryProber builds the prober for cb, admitting and charging each
// health check through usage.
func NewRecoveryProber(cb *CircuitBreakerBackend, usage UsageGate) *RecoveryProber {
	return &RecoveryProber{
		cb:    cb,
		usage: usage,
		log:   slog.Default().With(logfmt.Component("circuit_breaker"), "breaker_name", cb.Name()),
		now:   time.Now,
	}
}

// Probe implements breaker.Prober. While the circuit is open and a check is
// due, it runs one health check: success recovers the breaker, failure
// doubles the wait before the next one. A check the usage budget would refuse
// is skipped without being rescheduled, so it runs as soon as there is room.
func (p *RecoveryProber) Probe(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cb.State() != breaker.StateOpen {
		p.nextDue, p.backoff = time.Time{}, 0
		return
	}
	now := p.now()
	if p.nextDue.IsZero() {
		p.backoff = max(p.cb.RecoveryDelay(), breaker.DefaultWatchdogInterval)
		p.nextDue = now.Add(p.backoff)
		return
	}
	if now.Before(p.nextDue) {
		return
	}

	name := p.cb.Name()
	if !p.usage.WithinLimits(name, healthCheckOps, 0, 0) {
		return
	}
	p.usage.Record(name, s3op.HeadBucket, 0, 0)

	checkCtx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	err := p.cb.CheckHealth(checkCtx)
	cancel()
	if err == nil {
		p.cb.Recover()
		p.nextDue, p.backoff = time.Time{}, 0
		return
	}

	p.backoff = min(p.backoff*2, max(maxRecoveryBackoff, p.cb.RecoveryDelay()))
	p.nextDue = now.Add(p.backoff)
	p.log.WarnContext(ctx, "health check failed; circuit stays open",
		logfmt.Err(err),
		"next_check_in", p.backoff)
}
