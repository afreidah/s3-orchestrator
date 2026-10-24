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

// EligibleForWrite returns the write targets, in configured order, that are
// within usage limits and max object size for one call of op.
func (c *BackendRuntime) EligibleForWrite(op s3op.Operation, egress, ingress int64) []string {
	return slices.DeleteFunc(c.WriteTargets(), func(name string) bool {
		if !c.usage.WithinLimits(name, op, egress, ingress) {
			return true
		}
		maxSize := c.maxObjectSizes[name]
		return maxSize > 0 && ingress > maxSize
	})
}
