// -------------------------------------------------------------------------------
// Scrub Budget - Which Backends the Sweep May Read From
//
// Author: Alex Freidah
//
// The sweep reads a backend only while its usage limits leave room for the read
// and the scrub reserve on top, so client reads keep that share of the egress
// limit. The decision holds no state of its own, so the scrubber and the
// coverage gauges each build a budget over the same runtime and agree on which
// copies the sweep can stamp.
// -------------------------------------------------------------------------------

package worker

import (
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// ScrubBudget decides which backends the scrub sweep may read from right now.
// The embedded Section reads the integrity config from the running
// configuration.
type ScrubBudget struct {
	syncutil.Section[config.IntegrityConfig]
	ops Ops
}

// NewScrubBudget builds the budget the sweep reads the fleet under.
func NewScrubBudget(ops Ops, cfg syncutil.Section[config.IntegrityConfig]) *ScrubBudget {
	return &ScrubBudget{Section: cfg, ops: ops}
}

// ReachableBackends returns the backends the scrub queue draws from right now,
// so coverage is scoped to exactly the copies the sweep can stamp.
func (b *ScrubBudget) ReachableBackends() []string {
	affordable, _ := b.split()
	return affordable
}

// Sweeping reports whether the scrub sweep runs, which it does while its
// interval is set, so coverage gauges are only published for a fleet it
// verifies.
func (b *ScrubBudget) Sweeping() bool {
	return b.Config().ScrubberInterval > 0
}

// split divides the fleet into the backends the sweep can still read from and
// the ones it has to leave alone. It checks only for headroom, since no object
// is known yet; verifyOne re-checks against the real size.
func (b *ScrubBudget) split() (affordable, declined []string) {
	for _, name := range b.ops.BackendOrder() {
		if b.admits(name, 0) {
			affordable = append(affordable, name)
		} else {
			declined = append(declined, name)
		}
	}
	return affordable, declined
}

// admits reports whether the sweep may read egress bytes from a backend. The
// read is checked as if it also spent the scrub reserve, so the sweep stops
// while that share of the monthly egress limit is still free for client reads,
// which are checked against the whole limit.
func (b *ScrubBudget) admits(backend string, egress int64) bool {
	reserve := b.Config().ScrubReserve(b.ops.Usage().GetLimits()[backend].EgressByteLimit)
	return b.ops.Acct().Allow(backend, s3op.GetObject, egress+reserve, 0)
}
