// -------------------------------------------------------------------------------
// Usage Policy - Per-Backend Quota Admission
//
// Author: Alex Freidah
//
// Owns the live usage tracker plus the per-backend max-object-size map,
// and answers "does this proposed operation fit within the backend's
// configured limits?" In isolation: no knowledge of which backends
// exist, who is draining, or which circuit breakers are open. The
// composition of "registered AND not draining AND not unhealthy AND
// within limits" lives in *BackendRuntime; this type only owns the limits half.
// -------------------------------------------------------------------------------

package infra

import (
	"slices"

	"github.com/afreidah/s3-orchestrator/internal/counter"
)

// usagePolicy owns the runtime usage tracker and the static per-backend
// max-object-size map (sourced once at BackendRuntime construction from the
// backend config).
type usagePolicy struct {
	usage          *counter.UsageTracker
	maxObjectSizes map[string]int64
}

// newUsagePolicy constructs the policy from the shared usage tracker
// and the per-backend object-size limits.
func newUsagePolicy(usage *counter.UsageTracker, maxObjectSizes map[string]int64) *usagePolicy {
	return &usagePolicy{usage: usage, maxObjectSizes: maxObjectSizes}
}

// Tracker exposes the underlying usage tracker for callers that need
// the legacy WithinLimits / Record entry points (pre-flight checks,
// admin handlers). New code should call usagePolicy.WithinLimits
// instead so the max-object-size check stays bundled with the quota
// check.
func (p *usagePolicy) Tracker() *counter.UsageTracker {
	return p.usage
}

// MaxObjectSize returns the per-backend max object size; 0 means
// unlimited.
func (p *usagePolicy) MaxObjectSize(name string) int64 {
	return p.maxObjectSizes[name]
}

// WithinLimits combines the live quota check and the static max-object-
// size check into one decision. Used by FilterEligible so the per-name
// filter loop does not have to repeat both checks at every call site.
func (p *usagePolicy) WithinLimits(name string, apiCalls, egress, ingress int64) bool {
	if !p.usage.WithinLimits(name, apiCalls, egress, ingress) {
		return false
	}
	if max := p.maxObjectSizes[name]; max > 0 && ingress > max {
		return false
	}
	return true
}

// FilterEligible returns the subset of names that pass WithinLimits
// for the proposed operation dimensions. Order is preserved so the
// caller's routing-strategy iteration order is respected.
func (p *usagePolicy) FilterEligible(names []string, apiCalls, egress, ingress int64) []string {
	return slices.DeleteFunc(slices.Clone(names), func(name string) bool {
		return !p.WithinLimits(name, apiCalls, egress, ingress)
	})
}
