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
// each tick. *RecoveryProber health-checks its breaker's dependency while the
// breaker is open and recovers it once the check passes.
type Prober interface {
	Probe(ctx context.Context)
}

// Registry is the fixed set of breakers the watchdog probes, built once at
// startup and only read after.
type Registry struct {
	probers []Prober
}

// NewRegistry constructs a Registry over probers.
func NewRegistry(probers ...Prober) *Registry {
	return &Registry{probers: probers}
}

// Len returns the number of breakers the registry probes.
func (r *Registry) Len() int {
	return len(r.probers)
}

// ProbeAll invokes Probe on every breaker concurrently, so one slow health
// check cannot delay the others, and returns when all have finished.
func (r *Registry) ProbeAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, p := range r.probers {
		wg.Go(func() { p.Probe(ctx) })
	}
	wg.Wait()
}
