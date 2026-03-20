// -------------------------------------------------------------------------------
// CircuitBreaker - Generic Three-State Circuit Breaker
//
// Author: Alex Freidah
//
// Reusable circuit breaker state machine shared by both the database wrapper
// (CircuitBreakerStore) and the backend wrapper (CircuitBreakerBackend). Callers
// provide a pluggable error filter so only domain-relevant failures trip the
// breaker.
//
// States: closed (healthy) → open (down) → half-open (probing) → closed.
// -------------------------------------------------------------------------------

// Package breaker implements a generic three-state circuit breaker (closed,
// open, half-open) with pluggable error filters, probe jitter, and Prometheus
// metrics. Shared by the backend and store circuit breaker wrappers.
package breaker

import (
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/telemetry"
)

// -------------------------------------------------------------------------
// STATE
// -------------------------------------------------------------------------

// State represents the current circuit breaker state.
type State int

const (
	StateClosed   State = iota // healthy — all calls pass through
	StateOpen                         // down — return sentinel error
	StateHalfOpen                     // probing — one call allowed through
)

// String returns the human-readable name of the circuit state.
func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
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

// CircuitBreaker implements a three-state circuit breaker with a pluggable
// error filter. It is safe for concurrent use.
type CircuitBreaker struct {
	mu            sync.RWMutex
	state         State
	failures      int
	lastFailure   time.Time
	openedAt      time.Time
	failThreshold int
	openTimeout   time.Duration
	probeJitter   time.Duration     // randomized delay added to openTimeout; recomputed each time the circuit opens
	probeInFlight atomic.Bool
	name          string            // for logging and metrics labels
	isError       func(error) bool  // returns true if the error should trip the breaker
	sentinel      error             // error returned when circuit is open
}

// NewCircuitBreaker creates a new circuit breaker.
//
//   - name: identifier for logging and metrics (e.g. "database", "oci-backend")
//   - threshold: consecutive failures before opening
//   - timeout: delay before probing recovery
//   - isError: filter that returns true for errors that should count as failures
//   - sentinel: the error returned when the circuit is open (e.g. ErrDBUnavailable)
func NewCircuitBreaker(name string, threshold int, timeout time.Duration, isError func(error) bool, sentinel error) *CircuitBreaker {
	// Initialize the state gauge so Prometheus reports "closed" immediately
	// rather than showing "No Data" until the first transition.
	telemetry.CircuitBreakerState.WithLabelValues(name).Set(float64(StateClosed))

	return &CircuitBreaker{
		state:         StateClosed,
		failThreshold: threshold,
		openTimeout:   timeout,
		name:          name,
		isError:       isError,
		sentinel:      sentinel,
	}
}

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

// OpenDuration returns how long the circuit has been open or half-open.
// Returns 0 when the circuit is closed (healthy).
func (cb *CircuitBreaker) OpenDuration() time.Duration {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	if cb.state == StateClosed {
		return 0
	}
	return time.Since(cb.openedAt)
}

// ProbeEligible returns true when the circuit is open and the open timeout
// has elapsed, meaning the next request should be allowed through as a probe.
// This is a read-only check with no side effects — the actual state transition
// happens in PreCheck when the request is dispatched.
func (cb *CircuitBreaker) ProbeEligible() bool {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state == StateOpen && time.Since(cb.lastFailure) >= cb.openTimeout+cb.probeJitter
}

// -------------------------------------------------------------------------
// STATE MACHINE
// -------------------------------------------------------------------------

// PreCheck returns the sentinel error when the circuit is open. Transitions
// open → half-open when the timeout has elapsed, allowing one probe request.
func (cb *CircuitBreaker) PreCheck() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case StateClosed:
		return nil
	case StateOpen:
		if time.Since(cb.lastFailure) >= cb.openTimeout+cb.probeJitter {
			if !cb.probeInFlight.CompareAndSwap(false, true) {
				return cb.sentinel // another probe already in flight
			}
			cb.transition(StateHalfOpen)
			return nil // allow this request as the probe
		}
		return cb.sentinel
	case StateHalfOpen:
		return cb.sentinel
	}
	return nil
}

// PostCheck records the result of a real call and transitions state.
// When an error causes the circuit to open (or reopen), the original error
// is replaced with the sentinel so callers always see the canonical error.
func (cb *CircuitBreaker) PostCheck(err error) error {
	if !cb.isError(err) {
		cb.onSuccess()
		return err
	}
	cb.onFailure()
	if !cb.IsHealthy() {
		return cb.sentinel
	}
	return err
}

// onSuccess resets failures and transitions half-open → closed.
func (cb *CircuitBreaker) onSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.state == StateHalfOpen {
		cb.probeInFlight.Store(false)
		cb.transition(StateClosed)
	}
	cb.failures = 0
}

// onFailure increments the failure counter and transitions to open if the
// threshold is reached.
func (cb *CircuitBreaker) onFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failures++
	cb.lastFailure = time.Now()

	switch cb.state {
	case StateHalfOpen:
		cb.probeInFlight.Store(false)
		cb.transition(StateOpen)
	case StateClosed:
		if cb.failures >= cb.failThreshold {
			cb.transition(StateOpen)
		}
	}
}

// transition changes the circuit state and emits metrics + structured logs.
// Logs include from/to state, failure counts, and degraded_duration when the
// circuit recovers. Caller must hold cb.mu.
//
//nolint:sloglint // State machine has no request context; transitions are system-level events.
func (cb *CircuitBreaker) transition(to State) {
	from := cb.state
	cb.state = to
	telemetry.CircuitBreakerState.WithLabelValues(cb.name).Set(float64(to))
	telemetry.CircuitBreakerTransitionsTotal.WithLabelValues(cb.name, from.String(), to.String()).Inc()

	switch {
	case to == StateOpen && from == StateClosed:
		cb.openedAt = time.Now()
		cb.probeJitter = rand.N(cb.openTimeout / 4)
		slog.Warn("Circuit breaker opened: failure threshold reached",
			"name", cb.name,
			"from", from.String(),
			"to", to.String(),
			"failures", cb.failures,
			"threshold", cb.failThreshold)

	case to == StateOpen && from == StateHalfOpen:
		cb.probeJitter = rand.N(cb.openTimeout / 4)
		slog.Warn("Circuit breaker reopened: probe failed",
			"name", cb.name,
			"from", from.String(),
			"to", to.String(),
			"failures", cb.failures)

	case to == StateHalfOpen:
		slog.Info("Circuit breaker half-open: probing",
			"name", cb.name,
			"from", from.String(),
			"to", to.String(),
			"open_duration", time.Since(cb.openedAt).Round(time.Millisecond).String())

	case to == StateClosed:
		slog.Info("Circuit breaker closed: recovered",
			"name", cb.name,
			"from", from.String(),
			"to", to.String(),
			"degraded_duration", time.Since(cb.openedAt).Round(time.Millisecond).String())
	}
}

// -------------------------------------------------------------------------
// GENERIC CALL HELPERS
// -------------------------------------------------------------------------

// CBCall wraps a call that returns (T, error) with circuit breaker logic.
func CBCall[T any](cb *CircuitBreaker, fn func() (T, error)) (T, error) {
	var zero T
	if err := cb.PreCheck(); err != nil {
		return zero, err
	}
	result, err := fn()
	return result, cb.PostCheck(err)
}

// CBCallNoResult wraps a call that returns only error with circuit breaker logic.
func CBCallNoResult(cb *CircuitBreaker, fn func() error) error {
	if err := cb.PreCheck(); err != nil {
		return err
	}
	return cb.PostCheck(fn())
}
