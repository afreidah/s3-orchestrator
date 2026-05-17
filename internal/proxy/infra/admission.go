// -------------------------------------------------------------------------------
// Admission Gate - Bounded-Concurrency Semaphore for Request Admission
//
// Author: Alex Freidah
//
// Wraps the admission semaphore behind Acquire/Release methods so
// callers do not have to know whether a semaphore is wired (zero-value
// = unbounded admission). The raw channel is still exposed via Sem()
// for split-admission controllers (separate read/write semaphores) and
// httpserver wiring; new code should prefer Acquire/Release.
// -------------------------------------------------------------------------------

package infra

import "context"

// admissionGate owns the admission semaphore. A nil sem means
// admission is unbounded; the methods short-circuit accordingly.
type admissionGate struct {
	sem chan struct{}
}

// newAdmissionGate constructs the gate with the supplied semaphore
// channel (may be nil for unbounded admission).
func newAdmissionGate(sem chan struct{}) *admissionGate {
	return &admissionGate{sem: sem}
}

// Acquire blocks until a slot is available, or returns false if ctx
// is cancelled. Returns true immediately when no semaphore is wired.
func (g *admissionGate) Acquire(ctx context.Context) bool {
	if g.sem == nil {
		return true
	}
	select {
	case g.sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// Release returns a slot to the semaphore.
func (g *admissionGate) Release() {
	if g.sem == nil {
		return
	}
	<-g.sem
}

// Sem returns the underlying semaphore channel (nil if unwired).
// Used by callers that need the raw channel (split admission
// controllers); the Acquire/Release methods are preferred for new
// code.
func (g *admissionGate) Sem() chan struct{} {
	return g.sem
}
