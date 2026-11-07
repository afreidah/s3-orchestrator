// -------------------------------------------------------------------------------
// Backend Runtime - Admission
//
// Author: Alex Freidah
//
// The bounded-concurrency admission semaphore. A nil semaphore means
// admission is unbounded. The raw channel is exposed for the split
// read/write admission controllers the HTTP server builds.
//
// Clients only try for a slot, so any worker blocked on a full pool is handed
// the next one to free, ahead of them. One worker at a time may block, and it
// keeps that turn until its item finishes, which leaves background work one
// slot however busy the clients are and every other slot to them. Workers take
// a free slot only below half full, or one would retake its own released slot.
// -------------------------------------------------------------------------------

package infra

import (
	"context"
	"time"
)

// admissionRetry is how often a worker checks for a free slot while another
// holds the turn to block on the pool.
const admissionRetry = 100 * time.Millisecond

// AcquireAdmission takes a slot for background work and returns the func that
// gives it back, or false if ctx is cancelled first. Below half full a free slot
// is taken at once. Otherwise the worker takes the turn to block on the pool, or
// checks again shortly while another worker holds it. Always admits when no
// semaphore is wired.
func (c *BackendRuntime) AcquireAdmission(ctx context.Context) (func(), bool) {
	sem := c.cfg.AdmissionSem
	if sem == nil {
		return func() {
			// No semaphore means no slot was taken, so nothing goes back.
		}, true
	}
	for {
		if len(sem) < cap(sem)/2 {
			select {
			case sem <- struct{}{}:
				return func() { <-sem }, true
			default:
			}
		}
		select {
		case c.workerTurn <- struct{}{}:
			return c.acquireOnTurn(ctx)
		case <-time.After(admissionRetry):
		case <-ctx.Done():
			return nil, false
		}
	}
}

// acquireOnTurn blocks on the pool as the one worker allowed to, keeping the
// turn until the slot is given back.
func (c *BackendRuntime) acquireOnTurn(ctx context.Context) (func(), bool) {
	sem := c.cfg.AdmissionSem
	select {
	case sem <- struct{}{}:
		return func() {
			<-sem
			<-c.workerTurn
		}, true
	case <-ctx.Done():
		<-c.workerTurn
		return nil, false
	}
}

// AdmissionSem returns the underlying semaphore channel (nil if unwired), for
// the split admission controllers.
func (c *BackendRuntime) AdmissionSem() chan struct{} {
	return c.cfg.AdmissionSem
}
