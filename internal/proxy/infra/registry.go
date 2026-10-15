// -------------------------------------------------------------------------------
// Backend Runtime - Backend Registry
//
// Author: Alex Freidah
//
// The backend map, its configured iteration order, and the filters that
// drop backends whose state flags a purpose does not accept, or that are not
// serving the calls a purpose makes, from a candidate list.
// -------------------------------------------------------------------------------

package infra

import (
	"fmt"
	"slices"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
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

// ServedCalls returns the calls the named backend is serving. A backend with
// no circuit breaker serves every call, and an unknown one serves none.
func (c *BackendRuntime) ServedCalls(name string) core.BackendCalls {
	b, ok := c.backends[name]
	if !ok {
		return 0
	}
	return backend.ServedCalls(b)
}

// RefusedFor returns how long the named backend has refused any call in
// required, or zero while it serves them all or has no circuit breaker.
func (c *BackendRuntime) RefusedFor(name string, required core.BackendCalls) time.Duration {
	b, ok := c.backends[name]
	if !ok {
		return 0
	}
	return backend.RefusedFor(b, required)
}

// Serving keeps only the backends serving every call in required.
func (c *BackendRuntime) Serving(eligible []string, required core.BackendCalls) []string {
	return slices.DeleteFunc(slices.Clone(eligible), func(name string) bool {
		return !c.ServedCalls(name).Has(required)
	})
}

// WriteTargets returns the backends, in configured order, that may be given
// new bytes: their state flags accept writes and they serve every call a write
// needs. Client writes, replicas, drains and rebalance moves all start here.
func (c *BackendRuntime) WriteTargets() []string {
	return c.Serving(c.Accepted(c.order, core.AcceptForWrite), core.RequiredForWrite)
}
