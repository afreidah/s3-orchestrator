// -------------------------------------------------------------------------------
// Async Operation Tracker - Background Task State
//
// Author: Alex Freidah
//
// Thread-safe tracker for long-running UI operations (rebalance, cleanup) that
// run in background goroutines. Prevents duplicate runs and provides status
// polling for the frontend.
// -------------------------------------------------------------------------------

package ui

import "sync"

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// asyncResult holds the outcome of a completed async operation. Extra
// carries op-specific fields for the status payload (e.g. failed/total
// counts for encrypt-existing). Skipped, when set, surfaces a reason
// instead of a hard success.
type asyncResult struct {
	OK      bool
	Count   int
	Error   string
	Skipped string
	Extra   map[string]any
}

// asyncOpTracker manages named async operations. Each operation can be running
// or idle, with the last result available for polling after completion.
type asyncOpTracker struct {
	mu      sync.Mutex
	running map[string]bool
	results map[string]*asyncResult
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
		t.results = make(map[string]*asyncResult)
	}
	if t.running[name] {
		return false
	}
	t.running[name] = true
	t.results[name] = nil
	return true
}

// Complete records the result and marks the operation as no longer running.
func (t *asyncOpTracker) Complete(name string, result *asyncResult) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.running[name] = false
	t.results[name] = result
}

// Status returns the last result and whether the operation is currently running.
func (t *asyncOpTracker) Status(name string) (result *asyncResult, running bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.running == nil {
		return nil, false
	}
	return t.results[name], t.running[name]
}
