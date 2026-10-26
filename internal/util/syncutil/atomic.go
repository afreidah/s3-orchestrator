// -------------------------------------------------------------------------------
// AtomicConfig - Generic Thread-Safe Configuration Holder
//
// Author: Alex Freidah
//
// Wraps sync/atomic.Pointer[T] to provide a reusable, type-safe container for
// hot-reloadable configuration values, and Reloadable, which a component
// embeds for its SetConfig/Config pair instead of writing the accessors itself.
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

// Load atomically returns the current pointer, or nil if none was stored or a
// itself is nil, so an optional config needs no nil check before loading.
func (a *AtomicConfig[T]) Load() *T {
	if a == nil {
		return nil
	}
	return a.p.Load()
}

// Reloadable is the reloadable config a component embeds to gain SetConfig
// and Config, the pair the reload hooks and services call, without writing
// them by hand.
type Reloadable[T any] struct {
	cfg AtomicConfig[T]
}

// SetConfig atomically replaces the configuration.
func (r *Reloadable[T]) SetConfig(cfg *T) {
	r.cfg.Store(cfg)
}

// Config returns the current configuration, or nil when none was set.
func (r *Reloadable[T]) Config() *T {
	return r.cfg.Load()
}
