// -------------------------------------------------------------------------------
// AtomicConfig Tests
//
// Author: Alex Freidah
//
// Unit tests for the generic AtomicConfig type: zero-value safety,
// Store/Load round-trip, overwrite semantics, and concurrent access under
// the race detector.
// -------------------------------------------------------------------------------

package syncutil

import (
	"sync"
	"testing"
)

func TestAtomicConfig_ZeroValue(t *testing.T) {
	t.Parallel()
	var ac AtomicConfig[string]
	if got := ac.Load(); got != nil {
		t.Errorf("Load on zero value = %v, want nil", got)
	}
}

func TestAtomicConfig_StoreLoad(t *testing.T) {
	t.Parallel()
	var ac AtomicConfig[int]
	v := 42
	ac.Store(&v)

	got := ac.Load()
	if got == nil || *got != 42 {
		t.Errorf("Load = %v, want pointer to 42", got)
	}
}

func TestAtomicConfig_StoreOverwrite(t *testing.T) {
	t.Parallel()
	var ac AtomicConfig[string]
	first := "alpha"
	second := "beta"

	ac.Store(&first)
	ac.Store(&second)

	got := ac.Load()
	if got == nil || *got != "beta" {
		t.Errorf("Load after overwrite = %v, want pointer to beta", got)
	}
}

func TestAtomicConfig_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	var ac AtomicConfig[int]
	initial := 0
	ac.Store(&initial)

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			v := i
			ac.Store(&v)
			ac.Load()
		})
	}
	wg.Wait()

	if got := ac.Load(); got == nil {
		t.Error("Load after concurrent writes should not be nil")
	}
}
