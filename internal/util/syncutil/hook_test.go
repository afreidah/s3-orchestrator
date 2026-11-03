// -------------------------------------------------------------------------------
// Hook Tests
//
// Author: Alex Freidah
//
// Pins that an empty hook is a no-op, a set hook receives each call, and
// setting nil clears it.
// -------------------------------------------------------------------------------

package syncutil

import "testing"

// TestHook_SetCallClear walks a hook from empty through set to cleared and
// checks which calls reach the callback.
func TestHook_SetCallClear(t *testing.T) {
	t.Parallel()
	var h Hook[string]
	var got []string

	h.Call("before")
	h.Set(func(s string) { got = append(got, s) })
	h.Call("during")
	h.Set(nil)
	h.Call("after")

	if len(got) != 1 || got[0] != "during" {
		t.Errorf("calls = %q, want only %q", got, "during")
	}
}
