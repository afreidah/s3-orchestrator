// -------------------------------------------------------------------------------
// Backend Runtime - Backend Registry
//
// Author: Alex Freidah
//
// The backend map, its configured iteration order, and the filters that
// drop backends whose state flags a purpose does not accept, or whose circuit
// breaker is open, from a candidate list.
// -------------------------------------------------------------------------------

package infra

import (
	"fmt"
	"slices"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// SetStateChecker points the eligibility filters at the backend state manager
// so BackendFlags reflects live state. Called once the manager
// exists, since it is built after the runtime.
func (c *BackendRuntime) SetStateChecker(s StateChecker) {
	c.states = s
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

// BackendFlags returns the named backend's state flags. Returns none when no
// state manager is wired.
func (c *BackendRuntime) BackendFlags(name string) core.BackendFlags {
	if c.states == nil {
		return 0
	}
	return c.states.Flags(name)
}

// Accepted keeps only the backends whose flags are all ones accept allows.
func (c *BackendRuntime) Accepted(eligible []string, accept core.BackendFlags) []string {
	return slices.DeleteFunc(slices.Clone(eligible), func(name string) bool {
		return !c.BackendFlags(name).AcceptedBy(accept)
	})
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
