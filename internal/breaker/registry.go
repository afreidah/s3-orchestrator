// -------------------------------------------------------------------------------
// Circuit Breaker Registry
//
// Author: Alex Freidah
//
// Holds the set of circuit breakers the watchdog probes on each tick.
// Populated at DI construction time so the watchdog never has to type-assert
// or reach into the backend manager to discover breakers.
// -------------------------------------------------------------------------------

package breaker

import (
	"context"
	"sync"
)

// Prober is implemented by anything the watchdog drives toward recovery on
// each tick. *CircuitBreaker resets a stale half-open probe;
// backend.CircuitBreakerBackend health-checks its backend while open and
// recovers it once the check passes.
type Prober interface {
	Probe(ctx context.Context)
}

// Registry is a thread-safe collection of breakers probed by the watchdog.
type Registry struct {
	mu       sync.RWMutex
	breakers []Prober
}

// NewRegistry constructs a Registry preloaded with the given breakers.
// Nil entries are silently dropped.
func NewRegistry(initial ...Prober) *Registry {
	r := &Registry{}
	for _, b := range initial {
		if b != nil {
			r.breakers = append(r.breakers, b)
		}
	}
	return r
}

// Register appends a breaker to the registry. Nil is a no-op.
func (r *Registry) Register(b Prober) {
	if b == nil {
		return
	}
	r.mu.Lock()
	r.breakers = append(r.breakers, b)
	r.mu.Unlock()
}

// ProbeAll invokes Probe on every registered breaker concurrently, so one slow
// health check cannot delay the others, and returns when all have finished.
// Safe for concurrent use.
func (r *Registry) ProbeAll(ctx context.Context) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var wg sync.WaitGroup
	for _, b := range r.breakers {
		wg.Go(func() { b.Probe(ctx) })
	}
	wg.Wait()
}

// Len returns the number of registered breakers.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.breakers)
}
