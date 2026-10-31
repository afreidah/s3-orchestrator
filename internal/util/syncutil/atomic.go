// -------------------------------------------------------------------------------
// AtomicConfig - Generic Thread-Safe Configuration Holder
//
// Author: Alex Freidah
//
// Wraps sync/atomic.Pointer[T] to provide a reusable, type-safe container for
// hot-reloadable configuration values, and Section, the reader of one part of
// it a component embeds for its Config accessor.
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

// Section reads one section of the running configuration, so a reload that
// replaces the configuration reaches every reader without a copy to update. A
// component embeds it to gain Config.
type Section[T any] func() *T

// Config returns the section as it stands now, or nil for a nil Section.
func (s Section[T]) Config() *T {
	if s == nil {
		return nil
	}
	return s()
}

// Fixed is a Section that always returns v, for a value no reload replaces.
func Fixed[T any](v *T) Section[T] {
	return func() *T { return v }
}
