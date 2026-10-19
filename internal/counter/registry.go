// -------------------------------------------------------------------------------
// Registry - Keyed Accumulators with Whole-Map Swap
//
// Author: Alex Freidah
//
// Registry holds one accumulator per key behind an RWMutex and hands the whole
// map out in a single swap. Every in-memory counter in this package keeps its
// state this way: the charge path takes the read lock and touches an atomic,
// and the flush replaces the map wholesale rather than resetting keys one at a
// time, so a charge landing mid-flush is either counted in the batch that was
// taken or in the one that follows it, never dropped between two per-key
// resets.
// -------------------------------------------------------------------------------

package counter

import "sync"

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Registry is a set of per-key accumulators of type T, safe for concurrent use
// and ready at its zero value. T must be usable at its zero value, such as an
// atomic.Int64 or a struct of them.
type Registry[K comparable, T any] struct {
	mu      sync.RWMutex
	entries map[K]*T
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Peek returns the accumulator for key, or nil when the registry does not hold
// one. The read path uses this so an unrecognised key is a no-op rather than an
// allocation.
func (r *Registry[K, T]) Peek(key K) *T {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.entries[key]
}

// Get returns the accumulator for key, creating it on first use. Double-checked
// so the common case - an entry that already exists - costs a read lock.
func (r *Registry[K, T]) Get(key K) *T {
	if existing := r.Peek(key); existing != nil {
		return existing
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if existing := r.entries[key]; existing != nil {
		return existing
	}
	created := new(T)
	if r.entries == nil {
		r.entries = make(map[K]*T)
	}
	r.entries[key] = created
	return created
}

// Range calls fn for every accumulator without resetting or copying them, so
// a reader on the request path does not allocate. fn runs under the read lock
// and must not add keys to this registry.
func (r *Registry[K, T]) Range(fn func(key K, entry *T)) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for key, entry := range r.entries {
		fn(key, entry)
	}
}

// SwapAll empties the registry and returns the accumulators it held. A charge
// racing the swap lands in whichever map it resolved to, and both are
// accounted for: the old one by the caller, the new one by the next swap.
func (r *Registry[K, T]) SwapAll() map[K]*T {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.entries
	r.entries = nil
	return old
}
