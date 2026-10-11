// -------------------------------------------------------------------------------
// Backend Runtime - Backend Registry
//
// Author: Alex Freidah
//
// The backend map, its configured iteration order, and the filters that
// drop backends refusing writes, draining or circuit-broken from a candidate
// list.
// -------------------------------------------------------------------------------

package infra

import (
	"fmt"
	"slices"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/breaker"
)

// SetStateChecker points the eligibility filters at the backend state manager
// so AcceptsWrites and IsDraining reflect live state. Called once the manager
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

// AcceptsWrites reports whether the named backend takes new writes. Returns
// true when no state manager is wired.
func (c *BackendRuntime) AcceptsWrites(name string) bool {
	return c.states == nil || c.states.AcceptsWrites(name)
}

// IsDraining reports whether the named backend's state belongs to a drain.
// Returns false when no state manager is wired.
func (c *BackendRuntime) IsDraining(name string) bool {
	return c.states != nil && c.states.IsDraining(name)
}

// AcceptingWrites keeps only the backends that take new writes.
func (c *BackendRuntime) AcceptingWrites(eligible []string) []string {
	return slices.DeleteFunc(slices.Clone(eligible), func(name string) bool { return !c.AcceptsWrites(name) })
}

// ExcludeDraining filters out backends whose state belongs to a drain.
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
