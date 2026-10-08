// -------------------------------------------------------------------------------
// Backend Runtime - Usage Limits
//
// Author: Alex Freidah
//
// The live usage tracker plus the per-backend max-object-size map, and the
// write-eligibility decision that combines them with the registry filters.
// -------------------------------------------------------------------------------

package infra

import (
	"slices"

	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
)

// Usage returns the usage tracker.
func (c *BackendRuntime) Usage() *counter.UsageTracker {
	return c.usage
}

// MaxObjectSize returns the per-backend max object size; 0 means
// unlimited.
func (c *BackendRuntime) MaxObjectSize(name string) int64 {
	return c.maxObjectSizes[name]
}

// EligibleForWrite returns backends, in configured order, that are not
// draining, not circuit-broken, and within usage limits and max object size
// for the given operation.
func (c *BackendRuntime) EligibleForWrite(ops []s3op.Operation, egress, ingress int64) []string {
	eligible := c.ExcludeUnhealthy(c.ExcludeDraining(c.order))
	return slices.DeleteFunc(eligible, func(name string) bool {
		return !c.withinLimits(name, ops, egress, ingress)
	})
}

// withinLimits combines the live usage check and the static max-object-size
// check for one backend.
func (c *BackendRuntime) withinLimits(name string, ops []s3op.Operation, egress, ingress int64) bool {
	if !c.usage.WithinLimits(name, ops, egress, ingress) {
		return false
	}
	maxSize := c.maxObjectSizes[name]
	return maxSize <= 0 || ingress <= maxSize
}
