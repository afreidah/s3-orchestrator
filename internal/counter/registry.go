// -------------------------------------------------------------------------------
// Registry - Keyed Accumulators with Whole-Map Swap
//
// Author: Alex Freidah
//
// Registry holds one accumulator per key behind an RWMutex and hands the whole
// map out in a single swap. Every in-memory counter in this package keeps its
// per-backend state this way: the charge path takes the read lock and touches
// an atomic, and the flush replaces the map wholesale rather than resetting
// keys one at a time, so a charge landing mid-flush is either counted in the
// batch that was taken or in the one that follows it, never dropped between
// two per-key resets.
// -------------------------------------------------------------------------------

package counter

import "sync"

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Registry is a set of per-key accumulators of type T, safe for concurrent use.
//
// T is expected to be a zero-value-usable accumulator - an atomic.Int64, or a
// struct of them - because a fresh entry is created by allocating T and nothing
// initializes it further. Entries are never removed: a key that stopped being
// charged holds a zeroed accumulator, which costs a pointer and keeps the swap
// free of bookkeeping about which keys are still live.
type Registry[T any] struct {
	mu      sync.RWMutex
	entries map[string]*T
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// NewRegistry creates a registry pre-populated with an accumulator for each
// key. Pre-populating matters for the callers that treat an unknown key as a
// charge against something that does not exist and want Peek to say so, rather
// than silently accumulating under a name nothing will ever flush.
func NewRegistry[T any](keys ...string) *Registry[T] {
	entries := make(map[string]*T, len(keys))
	for _, key := range keys {
		entries[key] = new(T)
	}
	return &Registry[T]{entries: entries}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Peek returns the accumulator for key, or nil when the registry does not hold
// one. The read path uses this so an unrecognised key is a no-op rather than an
// allocation.
func (r *Registry[T]) Peek(key string) *T {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.entries[key]
}

// Get returns the accumulator for key, creating it on first use. Double-checked
// so the common case - an entry that already exists - costs a read lock.
func (r *Registry[T]) Get(key string) *T {
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
		r.entries = make(map[string]*T)
	}
	r.entries[key] = created
	return created
}

// Keys returns the names the registry currently holds accumulators for.
func (r *Registry[T]) Keys() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	keys := make([]string, 0, len(r.entries))
	for key := range r.entries {
		keys = append(keys, key)
	}
	return keys
}

// All returns the current accumulators without resetting them, for readers that
// need every key in one pass. The pointers are live: a concurrent charge is
// visible through them after this returns.
func (r *Registry[T]) All() map[string]*T {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]*T, len(r.entries))
	for key, entry := range r.entries {
		out[key] = entry
	}
	return out
}

// SwapAll replaces every accumulator with a fresh one and returns the old set.
// The key set is preserved, so a backend that goes quiet keeps its entry.
//
// One swap rather than a reset per key: charges racing the flush land in
// whichever map they resolved to, and both maps are accounted for - the old one
// by the caller reading it, the new one by the next flush.
func (r *Registry[T]) SwapAll() map[string]*T {
	r.mu.Lock()
	old := r.entries
	fresh := make(map[string]*T, len(old))
	for key := range old {
		fresh[key] = new(T)
	}
	r.entries = fresh
	r.mu.Unlock()
	return old
}
