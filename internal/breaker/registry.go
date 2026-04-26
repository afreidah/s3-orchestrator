// -------------------------------------------------------------------------------
// Circuit Breaker Registry
//
// Author: Alex Freidah
//
// Holds the set of circuit breakers the watchdog should sweep on each tick.
// Populated at DI construction time so the watchdog never has to type-assert
// or reach into the backend manager to discover breakers.
// -------------------------------------------------------------------------------

package breaker

import "sync"

// StaleProbeResetter is implemented by anything whose half-open probe can
// time out and need a manual reset. Both *CircuitBreaker and
// backend.CircuitBreakerBackend (which embeds *CircuitBreaker) satisfy it.
type StaleProbeResetter interface {
	// ResetStaleProbe resets a half-open probe that has exceeded its timeout.
	// Returns true when a probe was actually reset.
	ResetStaleProbe() bool
}

// Registry is a thread-safe collection of breakers swept by the watchdog.
type Registry struct {
	mu       sync.RWMutex
	breakers []StaleProbeResetter
}

// NewRegistry constructs a Registry preloaded with the given breakers.
// Nil entries are silently dropped.
func NewRegistry(initial ...StaleProbeResetter) *Registry {
	r := &Registry{}
	for _, b := range initial {
		if b != nil {
			r.breakers = append(r.breakers, b)
		}
	}
	return r
}

// Register appends a breaker to the registry. Nil is a no-op.
func (r *Registry) Register(b StaleProbeResetter) {
	if b == nil {
		return
	}
	r.mu.Lock()
	r.breakers = append(r.breakers, b)
	r.mu.Unlock()
}

// ResetStaleProbes invokes ResetStaleProbe on every registered breaker.
// Safe for concurrent use.
func (r *Registry) ResetStaleProbes() {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, b := range r.breakers {
		b.ResetStaleProbe()
	}
}

// Len returns the number of registered breakers.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.breakers)
}
