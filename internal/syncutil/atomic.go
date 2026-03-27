// -------------------------------------------------------------------------------
// AtomicConfig - Generic Thread-Safe Configuration Holder
//
// Author: Alex Freidah
//
// Wraps sync/atomic.Pointer[T] to provide a reusable, type-safe container for
// hot-reloadable configuration values. Eliminates the repeated Store/Load
// accessor boilerplate across workers, managers, and server types.
// -------------------------------------------------------------------------------

package syncutil

import "sync/atomic"

// AtomicConfig is a generic thread-safe holder for a pointer to T. It wraps
// atomic.Pointer[T] for hot-reloadable configuration or any value that must
// be read and written concurrently without a mutex. The zero value is usable;
// Load returns nil before any Store.
type AtomicConfig[T any] struct {
	p atomic.Pointer[T]
}

// Store atomically replaces the stored pointer.
func (a *AtomicConfig[T]) Store(val *T) {
	a.p.Store(val)
}

// Load atomically returns the current pointer, or nil if none was stored.
func (a *AtomicConfig[T]) Load() *T {
	return a.p.Load()
}
