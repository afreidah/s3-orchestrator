// -------------------------------------------------------------------------------
// Async Operation Tracker - Background Task State
//
// Author: Alex Freidah
//
// Thread-safe tracker for the passes the dashboard runs in background
// goroutines. Prevents duplicate runs and keeps each pass's last report for
// status polling.
// -------------------------------------------------------------------------------

package ui

import (
	"sync"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// asyncOpTracker manages named async operations. Each operation can be running
// or idle, with the last report available for polling after completion.
type asyncOpTracker struct {
	mu      sync.Mutex
	running map[string]bool
	results map[string]*admin.Report
}

// -------------------------------------------------------------------------
// OPERATIONS
// -------------------------------------------------------------------------

// TryStart marks an operation as running. Returns false if it is already
// running (caller should return 409 Conflict).
func (t *asyncOpTracker) TryStart(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.running == nil {
		t.running = make(map[string]bool)
		t.results = make(map[string]*admin.Report)
	}
	if t.running[name] {
		return false
	}
	t.running[name] = true
	t.results[name] = nil
	return true
}

// Complete records the report and marks the operation as no longer running.
func (t *asyncOpTracker) Complete(name string, rep *admin.Report) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.running[name] = false
	t.results[name] = rep
}

// Status returns the last report and whether the operation is currently
// running.
func (t *asyncOpTracker) Status(name string) (rep *admin.Report, running bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.running == nil {
		return nil, false
	}
	return t.results[name], t.running[name]
}
