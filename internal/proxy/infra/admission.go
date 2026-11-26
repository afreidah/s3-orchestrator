// -------------------------------------------------------------------------------
// Backend Runtime - Admission
//
// Author: Alex Freidah
//
// Background work is admitted from the pool HTTP writes share: the write pool
// when reads and writes are split, the one pool otherwise. It is a request flow
// of its own there, so it takes its turn at freed slots beside each virtual
// bucket and never holds more than half the pool. A nil pool admits without
// limit.
// -------------------------------------------------------------------------------

package infra

import "context"

// AcquireAdmission takes a slot for background work and returns the func that
// gives it back, or false if ctx ends first. Always admits when no pool is
// wired.
func (c *BackendRuntime) AcquireAdmission(ctx context.Context) (func(), bool) {
	slot, err := c.cfg.Admission.AcquireBackground(ctx)
	if err != nil {
		return nil, false
	}
	return slot.Release, true
}
