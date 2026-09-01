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
// States: closed (healthy) -> open (down) -> half-open (probing) -> closed.
// -------------------------------------------------------------------------------

// Package breaker implements a generic three-state circuit breaker (closed,
// open, half-open) with pluggable error filters and probe jitter. The
// breaker emits no metrics or events on its own  -  callers wire those via
// the optional OnStateChange callback so this package stays free of
// observability dependencies.
package breaker

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
)

// -------------------------------------------------------------------------
// STATE
// -------------------------------------------------------------------------

// State represents the current circuit breaker state.
type State int

// StateClosed and related constants used by this package.
const (
	StateClosed   State = iota // healthy  -  all calls pass through
	StateOpen                  // down  -  return sentinel error
	StateHalfOpen              // probing  -  one call allowed through
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

// StateChangeInfo captures the context the breaker shares with its
// OnStateChange callback. Failures and OpenDuration are read-only snapshots
// of the breaker's state at the moment of the transition.
type StateChangeInfo struct {
	Name         string
	From         State
	To           State
	Failures     int
	Threshold    int
	OpenDuration time.Duration // time spent open or half-open before reaching this state; zero when From was Closed
}

// CircuitBreaker implements a three-state circuit breaker with a pluggable
// error filter. It is safe for concurrent use. Observability hooks live
// behind the optional OnStateChange callback  -  the breaker package itself
// imports nothing from observe/.
type CircuitBreaker struct {
	mu            sync.RWMutex
	state         State
	failures      int
	lastFailure   time.Time
	openedAt      time.Time
	failThreshold int
	openTimeout   time.Duration
	probeJitter   time.Duration // randomized delay added to openTimeout; recomputed each time the circuit opens
	probeInFlight atomic.Bool
	probeStarted  atomic.Int64     // UnixNano timestamp of the probe dispatch; zero when no probe is active
	name          string           // for logging and metrics labels
	log           *slog.Logger     // scoped to logfmt.Component("circuit_breaker") with breaker_name attr
	isError       func(error) bool // returns true if the error should trip the breaker
	sentinel      error            // error returned when circuit is open
	onStateChange func(StateChangeInfo)
}

// Config configures a CircuitBreaker. Use SetOnStateChange after
// construction to install metric / event hooks.
type Config struct {
	Name      string           // identifier for logging and labels (e.g. "database", "oci-backend")
	Threshold int              // consecutive failures before opening
	Timeout   time.Duration    // delay before probing recovery
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

// SetOnStateChange installs a callback invoked on every closed/open/
// half-open transition. The callback runs synchronously while the breaker
// holds its lock, so implementations must be cheap and non-blocking.
// Passing nil clears any previously installed callback.
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
// This is a read-only check with no side effects  -  the actual state transition
// happens in PreCheck when the request is dispatched.
func (cb *CircuitBreaker) ProbeEligible() bool {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state == StateOpen && time.Since(cb.lastFailure) >= cb.openTimeout+cb.probeJitter
}

// -------------------------------------------------------------------------
// STATE MACHINE
// -------------------------------------------------------------------------

// probeTimeout is the maximum duration a probe request can be in flight
// before it is considered stale. If PostCheck is never called (e.g. due
// to a panic in the call chain), the probe flag auto-resets after this
// duration so the circuit is not permanently stuck in half-open.
const probeTimeout = 2 * time.Minute

// PreCheck returns the sentinel error when the circuit is open. Transitions
// open -> half-open when the timeout has elapsed, allowing one probe request.
// If a previous probe has been in flight longer than probeTimeout, it is
// considered abandoned and a new probe is allowed.
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
			cb.probeStarted.Store(time.Now().UnixNano())
			cb.transition(StateHalfOpen)
			return nil // allow this request as the probe
		}
		return cb.sentinel
	case StateHalfOpen:
		// Recover from a stale probe whose PostCheck was never called.
		if started := cb.probeStarted.Load(); started > 0 {
			elapsed := time.Since(time.Unix(0, started))
			if elapsed >= probeTimeout {
				cb.log.WarnContext(context.Background(), "stale probe detected, resetting to open",
					"name", cb.name, "probe_age", elapsed.Round(time.Second))
				cb.probeInFlight.Store(false)
				cb.probeStarted.Store(0)
				cb.transition(StateOpen)
				return cb.sentinel
			}
		}
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

// Recover transitions the breaker back to Closed cleanly, regardless of
// the current state. The intended caller is an out-of-band liveness
// probe that has independently confirmed the dependency is reachable
// (e.g., the Redis counter recovery probe in internal/counter/redis.go).
// PostCheck(nil) is the wrong tool for this case: it models a single
// in-flight call's success and only handles HalfOpen->Closed, leaving
// an Open breaker stuck. Recover clears probe state and zeroes the
// failure counter so the breaker tolerates the configured threshold of
// new failures before re-opening.
func (cb *CircuitBreaker) Recover() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.probeInFlight.Store(false)
	cb.probeStarted.Store(0)
	cb.failures = 0
	if cb.state != StateClosed {
		cb.transition(StateClosed)
	}
}

// onSuccess resets failures and transitions half-open -> closed.
func (cb *CircuitBreaker) onSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.state == StateHalfOpen {
		cb.probeInFlight.Store(false)
		cb.probeStarted.Store(0)
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
		cb.probeStarted.Store(0)
		cb.transition(StateOpen)
	case StateClosed:
		if cb.failures >= cb.failThreshold {
			cb.transition(StateOpen)
		}
	}
}

// ResetStaleProbe checks whether a half-open probe has exceeded probeTimeout
// and resets the circuit to open if so. Called by the background watchdog
// service to ensure stale probes are detected even when no new requests
// arrive. Returns true if a stale probe was reset.
func (cb *CircuitBreaker) ResetStaleProbe() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.state != StateHalfOpen {
		return false
	}
	started := cb.probeStarted.Load()
	if started == 0 {
		return false
	}
	elapsed := time.Since(time.Unix(0, started))
	if elapsed < probeTimeout {
		return false
	}

	cb.log.WarnContext(context.Background(), "stale probe reset by watchdog",
		"name", cb.name, "probe_age", elapsed.Round(time.Second))
	cb.probeInFlight.Store(false)
	cb.probeStarted.Store(0)
	cb.transition(StateOpen)
	return true
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

	switch {
	case to == StateOpen && from == StateClosed:
		cb.openedAt = time.Now()
		cb.probeJitter = rand.N(cb.openTimeout / 4) //nolint:gosec // G404: jitter does not require crypto-strength randomness
		cb.log.WarnContext(context.Background(), "opened: failure threshold reached",
			"name", cb.name,
			"from", from.String(),
			"to", to.String(),
			"failures", cb.failures,
			"threshold", cb.failThreshold)

	case to == StateOpen && from == StateHalfOpen:
		cb.probeJitter = rand.N(cb.openTimeout / 4) //nolint:gosec // G404: jitter does not require crypto-strength randomness
		cb.log.WarnContext(context.Background(), "reopened: probe failed",
			"name", cb.name,
			"from", from.String(),
			"to", to.String(),
			"failures", cb.failures)

	case to == StateHalfOpen:
		cb.log.InfoContext(context.Background(), "half-open: probing",
			"name", cb.name,
			"from", from.String(),
			"to", to.String(),
			"open_duration", openFor.Round(time.Millisecond).String())

	case to == StateClosed:
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

// CallNoResult wraps a call that returns only error with circuit breaker logic.
func (cb *CircuitBreaker) CallNoResult(fn func() error) error {
	if err := cb.PreCheck(); err != nil {
		return err
	}
	return cb.PostCheck(fn())
}
