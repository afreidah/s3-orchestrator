// -------------------------------------------------------------------------------
// Backend Runtime - Admission
//
// Author: Alex Freidah
//
// The bounded-concurrency admission semaphore. A nil semaphore means
// admission is unbounded. The raw channel is exposed for the split
// read/write admission controllers the HTTP server builds.
// -------------------------------------------------------------------------------

package infra

import "context"

// AcquireAdmission blocks until a slot is available, or returns false if ctx
// is cancelled. Returns true immediately when no semaphore is wired.
func (c *BackendRuntime) AcquireAdmission(ctx context.Context) bool {
	if c.cfg.AdmissionSem == nil {
		return true
	}
	select {
	case c.cfg.AdmissionSem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// ReleaseAdmission returns a slot to the admission semaphore.
func (c *BackendRuntime) ReleaseAdmission() {
	if c.cfg.AdmissionSem == nil {
		return
	}
	<-c.cfg.AdmissionSem
}

// AdmissionSem returns the underlying semaphore channel (nil if unwired), for
// the split admission controllers.
func (c *BackendRuntime) AdmissionSem() chan struct{} {
	return c.cfg.AdmissionSem
}
