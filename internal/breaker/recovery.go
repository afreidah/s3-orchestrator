// -------------------------------------------------------------------------------
// RecoveryProber - Out-of-Band Recovery for Circuit Breakers
//
// Author: Alex Freidah
//
// Drives an open breaker back to closed by running a check of its dependency
// that no client request pays for. The watchdog calls Probe on every tick;
// while the circuit is closed that does nothing. Once it opens, the check runs
// after the open timeout, then after doubling intervals up to a cap, until one
// passes and the breaker recovers. The database pings itself on that schedule;
// a backend's checks are also admitted and charged against its usage budget.
// -------------------------------------------------------------------------------

package breaker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
)

// healthCheckTimeout bounds one check so a hung dependency cannot stall the
// watchdog tick.
const healthCheckTimeout = 10 * time.Second

// Recovery describes how a breaker's dependency is checked. Check reports
// whether the dependency answers. MaxBackoff caps the interval between checks
// of a dependency that stays down; an open timeout longer than it is used as
// the cap instead, so zero checks every open timeout. Admit, when set, decides
// whether a due check may run now; a check it refuses is skipped without being
// rescheduled, so it runs as soon as it is admitted.
type Recovery struct {
	Check      func(context.Context) error
	MaxBackoff time.Duration
	Admit      func(context.Context) bool
}

// RecoveryProber runs one breaker's check on a backoff schedule while it is
// open and recovers it once the check passes.
type RecoveryProber struct {
	breaker  *CircuitBreaker
	recovery Recovery
	log      *slog.Logger
	now      func() time.Time

	mu      sync.Mutex
	nextDue time.Time
	backoff time.Duration
}

// Compile-time check.
var _ Prober = (*RecoveryProber)(nil)

// NewRecoveryProber builds the prober that recovers cb through r.
func NewRecoveryProber(cb *CircuitBreaker, r Recovery) *RecoveryProber {
	return &RecoveryProber{
		breaker:  cb,
		recovery: r,
		log:      slog.Default().With(logfmt.Component("circuit_breaker"), "breaker_name", cb.Name()),
		now:      time.Now,
	}
}

// Probe implements Prober. While the circuit is open and a check is due, it
// runs the check once: success recovers the breaker, failure doubles the wait
// before the next one.
func (p *RecoveryProber) Probe(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()

	cb := p.breaker
	if cb.State() != StateOpen {
		p.nextDue, p.backoff = time.Time{}, 0
		return
	}
	now := p.now()
	if p.nextDue.IsZero() {
		p.backoff = max(cb.RecoveryDelay(), DefaultWatchdogInterval)
		p.nextDue = now.Add(p.backoff)
		return
	}
	if now.Before(p.nextDue) {
		return
	}
	if p.recovery.Admit != nil && !p.recovery.Admit(ctx) {
		return
	}

	checkCtx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	err := p.recovery.Check(checkCtx)
	cancel()
	if err == nil {
		cb.Recover()
		p.nextDue, p.backoff = time.Time{}, 0
		return
	}

	p.backoff = min(p.backoff*2, max(p.recovery.MaxBackoff, cb.RecoveryDelay(), DefaultWatchdogInterval))
	p.nextDue = now.Add(p.backoff)
	p.log.WarnContext(ctx, "recovery check failed; circuit stays open",
		logfmt.Err(err),
		"next_check_in", p.backoff)
}
