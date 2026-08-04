// -------------------------------------------------------------------------------
// Backend Registry - Lookup and Health Filtering
//
// Author: Alex Freidah
//
// Owns the static backend map + the configured iteration order, plus the
// dynamic drain checker that decides which backends are currently in
// service. Hides the circuit-breaker probe logic so write-eligibility
// callers do not have to know about the breaker implementation. Used
// internally by *BackendRuntime; consumers reach the same methods through BackendRuntime's
// public surface (Backends, BackendOrder, GetBackend, etc.).
// -------------------------------------------------------------------------------

package infra

import (
	"fmt"
	"slices"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/breaker"
)

// backendRegistry owns the per-process backend map, the configured
// iteration order, and the drain checker. drainMgr is set via
// SetDrainChecker once the drain manager exists, since it is built after
// the runtime.
type backendRegistry struct {
	backends map[string]backend.ObjectBackend
	order    []string
	drainMgr DrainChecker
}

// newBackendRegistry constructs a registry from the backend map and
// iteration order. The drain checker is wired post-construction via
// SetDrainChecker.
func newBackendRegistry(backends map[string]backend.ObjectBackend, order []string) *backendRegistry {
	return &backendRegistry{backends: backends, order: order}
}

// SetDrainChecker installs the drain manager after BackendRuntime has been
// constructed.
func (r *backendRegistry) SetDrainChecker(d DrainChecker) {
	r.drainMgr = d
}

// All returns the backend map (read-only contract).
func (r *backendRegistry) All() map[string]backend.ObjectBackend {
	return r.backends
}

// Order returns the configured backend iteration order.
func (r *backendRegistry) Order() []string {
	return r.order
}

// Get returns the named backend, or an error if it doesn't exist.
func (r *backendRegistry) Get(name string) (backend.ObjectBackend, error) {
	b, ok := r.backends[name]
	if !ok {
		return nil, fmt.Errorf("backend %s not found", name)
	}
	return b, nil
}

// IsDraining returns true if the named backend is currently being
// drained. Returns false when no drain manager is set (e.g. early
// startup before SetDrainChecker).
func (r *backendRegistry) IsDraining(name string) bool {
	if r.drainMgr == nil {
		return false
	}
	return r.drainMgr.IsDraining(name)
}

// ExcludeDraining filters out backends that are currently draining.
func (r *backendRegistry) ExcludeDraining(eligible []string) []string {
	return slices.DeleteFunc(slices.Clone(eligible), r.IsDraining)
}

// ExcludeUnhealthy filters out backends whose circuit breaker is open
// and not probe-eligible. Backends that are not breaker-wrapped pass
// through unconditionally.
func (r *backendRegistry) ExcludeUnhealthy(eligible []string) []string {
	return slices.DeleteFunc(slices.Clone(eligible), func(name string) bool {
		b, ok := r.backends[name]
		if !ok {
			return true
		}
		cb, ok := b.(*backend.CircuitBreakerBackend)
		return ok && cb.State() == breaker.StateOpen && !cb.ProbeEligible()
	})
}
