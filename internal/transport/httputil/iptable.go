// -------------------------------------------------------------------------------
// Per-Client Table - Keyed State With a Background Sweep
//
// Author: Alex Freidah
//
// The state a per-client guard keeps for each address it has seen, behind one
// lock, with a background sweep that drops the entries that no longer matter.
// The rate limiter and the login throttle both keep their state here, so the
// locking, the sweep and its shutdown are written once.
// -------------------------------------------------------------------------------

package httputil

import (
	"context"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// ipTable maps a client address to its entry. stale reports whether an entry
// the sweep finds is done with, as of now.
type ipTable[T any] struct {
	mu      sync.Mutex
	entries map[string]*T
	stale   func(e *T, now time.Time) bool
	stop    context.CancelFunc
}

// newIPTable builds an empty table and starts its sweep every interval until
// close. stale decides which entries a sweep removes.
func newIPTable[T any](interval time.Duration, stale func(e *T, now time.Time) bool) *ipTable[T] {
	ctx, stop := context.WithCancel(context.Background()) //nolint:gosec // G118: close calls stop; gosec does not follow it into a generic type
	t := &ipTable[T]{entries: make(map[string]*T), stale: stale, stop: stop}
	go syncutil.Every(ctx, interval, func(context.Context) { t.sweep() })
	return t
}

// visit runs fn on ip's entry under the table's lock. fn receives nil when ip
// has no entry and returns the entry to keep, or nil to remove it.
func (t *ipTable[T]) visit(ip string, fn func(e *T) *T) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e := fn(t.entries[ip]); e != nil {
		t.entries[ip] = e
	} else {
		delete(t.entries, ip)
	}
}

// reset drops every entry. apply runs under the same lock, so a setting it
// changes reaches only the entries created after it.
func (t *ipTable[T]) reset(apply func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	apply()
	clear(t.entries)
}

// sweep removes the entries stale reports done with.
func (t *ipTable[T]) sweep() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for ip, e := range t.entries {
		if t.stale(e, now) {
			delete(t.entries, ip)
		}
	}
}

// close stops the sweep. Safe to call more than once.
func (t *ipTable[T]) close() {
	t.stop()
}
