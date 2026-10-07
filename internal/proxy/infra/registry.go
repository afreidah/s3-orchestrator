// -------------------------------------------------------------------------------
// Backend Runtime - Backend Registry
//
// Author: Alex Freidah
//
// The backend map, its configured iteration order, and the filters that
// drop draining and circuit-broken backends from a candidate list.
// -------------------------------------------------------------------------------

package infra

import (
	"fmt"
	"slices"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/breaker"
)

// SetDrainChecker points the eligibility filter at the drain manager so
// IsDraining reflects live drain state. Called once the drain manager
// exists, since it is built after the runtime.
func (c *BackendRuntime) SetDrainChecker(d DrainChecker) {
	c.drainMgr = d
}

// GetBackend returns the named backend, or an error if it doesn't exist.
func (c *BackendRuntime) GetBackend(name string) (backend.ObjectBackend, error) {
	b, ok := c.backends[name]
	if !ok {
		return nil, fmt.Errorf("backend %s not found", name)
	}
	return b, nil
}

// Backends returns the backend map.
func (c *BackendRuntime) Backends() map[string]backend.ObjectBackend {
	return c.backends
}

// BackendOrder returns the configured backend ordering.
func (c *BackendRuntime) BackendOrder() []string {
	return c.order
}

// IsDraining returns true if the named backend is currently being drained.
// Returns false when no drain manager is wired.
func (c *BackendRuntime) IsDraining(name string) bool {
	if c.drainMgr == nil {
		return false
	}
	return c.drainMgr.IsDraining(name)
}

// ExcludeDraining filters out backends that are currently draining.
func (c *BackendRuntime) ExcludeDraining(eligible []string) []string {
	return slices.DeleteFunc(slices.Clone(eligible), c.IsDraining)
}

// ExcludeUnhealthy filters out backends whose circuit breaker is open.
// Backends that are not breaker-wrapped pass through unconditionally.
func (c *BackendRuntime) ExcludeUnhealthy(eligible []string) []string {
	return slices.DeleteFunc(slices.Clone(eligible), func(name string) bool {
		b, ok := c.backends[name]
		if !ok {
			return true
		}
		cb, ok := b.(*backend.CircuitBreakerBackend)
		return ok && cb.State() == breaker.StateOpen
	})
}
