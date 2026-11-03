// -------------------------------------------------------------------------------
// Hook - Optional Callback Registered at Runtime
//
// Author: Alex Freidah
//
// A process-wide callback that startup or a reload registers while request
// goroutines call it.
// -------------------------------------------------------------------------------

package syncutil

import "sync/atomic"

// Hook holds an optional func(T) that can be set and cleared while other
// goroutines call it. The zero value holds none, and calling it does nothing.
type Hook[T any] struct {
	p atomic.Pointer[func(T)]
}

// Set registers fn, replacing any earlier one. A nil fn clears the hook.
func (h *Hook[T]) Set(fn func(T)) {
	if fn == nil {
		h.p.Store(nil)
		return
	}
	h.p.Store(&fn)
}

// Call passes v to the registered callback, or does nothing when none is
// registered.
func (h *Hook[T]) Call(v T) {
	if fn := h.p.Load(); fn != nil {
		(*fn)(v)
	}
}
