// -------------------------------------------------------------------------------
// Backend Runtime - Usage Limits
//
// Author: Alex Freidah
//
// The live usage tracker plus each backend's profile, and the write-eligibility
// decision that combines the usage limits and the profile's size cap with the
// registry filters.
// -------------------------------------------------------------------------------

package infra

import (
	"slices"

	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// Usage returns the usage tracker.
func (c *BackendRuntime) Usage() *counter.UsageTracker {
	return c.usage
}

// Profile returns a backend's configured profile, or the zero profile for a
// name the runtime does not know.
func (c *BackendRuntime) Profile(name string) core.BackendProfile {
	return c.profiles[name]
}

// EligibleForWrite returns the write targets, in configured order, that are
// within usage limits and max object size for one call of op.
func (c *BackendRuntime) EligibleForWrite(op s3op.Operation, egress, ingress int64) []string {
	return slices.DeleteFunc(c.WriteTargets(), func(name string) bool {
		return !c.usage.WithinLimits(name, op, egress, ingress) || !c.profiles[name].Accepts(ingress)
	})
}
