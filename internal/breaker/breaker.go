// -------------------------------------------------------------------------------
// CircuitBreaker - Generic Two-State Circuit Breaker
//
// Author: Alex Freidah
//
// Reusable circuit breaker state machine shared by the database driver
// chokepoint (store.NewDatabaseBreaker), the backend wrapper
// (CircuitBreakerBackend) and the Redis counter backend. Callers provide a
// pluggable error filter so only domain-relevant failures trip the breaker.
//
// States: closed (healthy) -> open (down) -> closed. An open breaker refuses
// every call, so no ordinary call is spent finding out whether the dependency
// is back. Its owner checks the dependency itself, usually through a
// RecoveryProber, and calls Recover once the check passes.
// -------------------------------------------------------------------------------

package breaker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
)

// -------------------------------------------------------------------------
// STATE
// -------------------------------------------------------------------------

// State represents the current circuit breaker state.
type State int

// The two circuit states.
const (
	StateClosed State = iota // healthy  -  all calls pass through
	StateOpen                // down  -  return sentinel error
)

// String returns the human-readable name of the circuit state.
func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	default:
		return "unknown"
	}
}

// -------------------------------------------------------------------------
// SENTINEL ERRORS
// -------------------------------------------------------------------------

// ErrBackendUnavailable is returned by CircuitBreakerBackend when the circuit
// is open (backend is known to be unreachable or returning errors).
var ErrBackendUnavailable = errors.New("backend unavailable")

// -------------------------------------------------------------------------
// CIRCUIT BREAKER
// -------------------------------------------------------------------------

// StateChangeInfo captures the context the breaker shares with its
// OnStateChange callback. Failures and OpenDuration are read-only snapshots
// of the breaker's state at the moment of the transition.
type StateChangeInfo struct {
	Name         string
	From         State
	To           State
	Failures     int
	Threshold    int
	OpenDuration time.Duration // time spent open before reaching this state; zero when From was Closed
}

// CircuitBreaker implements a two-state circuit breaker with a pluggable
// error filter. It is safe for concurrent use. Observability hooks live
// behind the optional OnStateChange callback  -  the breaker package itself
// imports nothing from observe/.
type CircuitBreaker struct {
	mu            sync.RWMutex
	state         State
	failures      int
	openedAt      time.Time
	failThreshold int
	openTimeout   time.Duration
	name          string           // for logging and metrics labels
	log           *slog.Logger     // scoped to logfmt.Component("circuit_breaker") with breaker_name attr
	isError       func(error) bool // returns true if the error should trip the breaker
	sentinel      error            // error returned when circuit is open
	lastErr       error            // the failure that last counted against the breaker, for its logs
	onStateChange func(StateChangeInfo)
}

// Config configures a CircuitBreaker. Use SetOnStateChange after
// construction to install metric / event hooks. Timeout is how long an open
// breaker waits before its owner first checks whether the dependency is back.
type Config struct {
	Name      string           // identifier for logging and labels (e.g. "database", "oci-backend")
	Threshold int              // consecutive failures before opening
	Timeout   time.Duration    // delay before the first recovery check
	IsError   func(error) bool // returns true for errors that should count as failures
	Sentinel  error            // error returned when the circuit is open (e.g. ErrDBUnavailable)
}

// NewCircuitBreaker creates a new circuit breaker from cfg.
func NewCircuitBreaker(cfg Config) *CircuitBreaker {
	return &CircuitBreaker{
		state:         StateClosed,
		failThreshold: cfg.Threshold,
		openTimeout:   cfg.Timeout,
		name:          cfg.Name,
		log: slog.Default().With(
			logfmt.Component("circuit_breaker"),
			"breaker_name", cfg.Name,
		),
		isError:  cfg.IsError,
		sentinel: cfg.Sentinel,
	}
}

// SetOnStateChange installs a callback invoked on every closed/open
// transition. The callback runs synchronously while the breaker holds its
// lock, so implementations must be cheap and non-blocking. Passing nil clears
// any previously installed callback.
func (cb *CircuitBreaker) SetOnStateChange(fn func(StateChangeInfo)) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.onStateChange = fn
}

// AddOnStateChange appends a callback to the existing hook chain. Used
// to attach an additional listener (e.g. cache invalidation on recovery)
// without disturbing the telemetry hook already installed.
func (cb *CircuitBreaker) AddOnStateChange(fn func(StateChangeInfo)) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	prev := cb.onStateChange
	if prev == nil {
		cb.onStateChange = fn
		return
	}
	cb.onStateChange = func(info StateChangeInfo) {
		prev(info)
		fn(info)
	}
}

// Name returns the breaker's identifier  -  useful for callers wiring
// observability hooks that need the label.
func (cb *CircuitBreaker) Name() string { return cb.name }

// IsHealthy returns true when the circuit is closed.
func (cb *CircuitBreaker) IsHealthy() bool {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state == StateClosed
}

// State returns the current circuit state.
func (cb *CircuitBreaker) State() State {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}

// OpenDuration returns how long the circuit has been open.
// Returns 0 when the circuit is closed (healthy).
func (cb *CircuitBreaker) OpenDuration() time.Duration {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	if cb.state == StateClosed {
		return 0
	}
	return time.Since(cb.openedAt)
}

// RecoveryDelay returns the open timeout: how long an open circuit waits
// before recovery is first attempted.
func (cb *CircuitBreaker) RecoveryDelay() time.Duration {
	return cb.openTimeout
}

// -------------------------------------------------------------------------
// STATE MACHINE
// -------------------------------------------------------------------------

// PreCheck returns the sentinel error while the circuit is open, and nil while
// it is closed.
func (cb *CircuitBreaker) PreCheck() error {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	if cb.state == StateOpen {
		return cb.sentinel
	}
	return nil
}

// PostCheck records the result of a real call. While the circuit is open after
// a failure, the error returned wraps the sentinel around the original, so
// callers match the canonical error with errors.Is and logs still show what
// actually failed.
func (cb *CircuitBreaker) PostCheck(err error) error {
	if !cb.isError(err) {
		cb.onSuccess()
		return err
	}
	cb.onFailure(err)
	if !cb.IsHealthy() {
		return fmt.Errorf("%w: %w", cb.sentinel, err)
	}
	return err
}

// Recover closes the breaker and zeroes the failure count, for an
// out-of-band check that has confirmed the dependency is reachable.
func (cb *CircuitBreaker) Recover() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures = 0
	if cb.state != StateClosed {
		cb.transition(StateClosed)
	}
}

// onSuccess resets the consecutive failure count. It does not close an open
// breaker: a call that succeeds while the circuit is open did not go through
// PreCheck, and only Recover closes the circuit.
func (cb *CircuitBreaker) onSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures = 0
}

// onFailure increments the failure counter and opens the circuit once the
// threshold is reached.
func (cb *CircuitBreaker) onFailure(err error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failures++
	cb.lastErr = err
	if cb.state == StateClosed && cb.failures >= cb.failThreshold {
		cb.transition(StateOpen)
	}
}

// transition changes the circuit state, emits structured logs, and notifies
// the OnStateChange callback. Caller must hold cb.mu.
func (cb *CircuitBreaker) transition(to State) {
	from := cb.state
	cb.state = to

	var openFor time.Duration
	if from != StateClosed {
		openFor = time.Since(cb.openedAt)
	}

	switch to {
	case StateOpen:
		cb.openedAt = time.Now()
		cb.log.WarnContext(context.Background(), "opened: failure threshold reached",
			"name", cb.name,
			"from", from.String(),
			"to", to.String(),
			"failures", cb.failures,
			"threshold", cb.failThreshold,
			logfmt.Err(cb.lastErr))

	case StateClosed:
		cb.log.InfoContext(context.Background(), "closed: recovered",
			"name", cb.name,
			"from", from.String(),
			"to", to.String(),
			"degraded_duration", openFor.Round(time.Millisecond).String())
	}

	if cb.onStateChange != nil {
		cb.onStateChange(StateChangeInfo{
			Name:         cb.name,
			From:         from,
			To:           to,
			Failures:     cb.failures,
			Threshold:    cb.failThreshold,
			OpenDuration: openFor,
		})
	}
}

// -------------------------------------------------------------------------
// GENERIC CALL HELPERS
// -------------------------------------------------------------------------

// Call wraps a call that returns (T, error) with circuit breaker logic.
func (cb *CircuitBreaker) Call[T any](fn func() (T, error)) (T, error) {
	var zero T
	if err := cb.PreCheck(); err != nil {
		return zero, err
	}
	result, err := fn()
	return result, cb.PostCheck(err)
}
