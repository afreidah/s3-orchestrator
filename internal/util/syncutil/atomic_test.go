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

// TestAtomicConfig_ZeroValue verifies the atomic config zero value contract.
// Asserts that Load on zero value = , want nil.
func TestAtomicConfig_ZeroValue(t *testing.T) {
	t.Parallel()
	var ac AtomicConfig[string]
	if got := ac.Load(); got != nil {
		t.Errorf("Load on zero value = %v, want nil", got)
	}
}

// TestAtomicConfig_StoreLoad verifies the atomic config store load contract.
// Asserts that Load = , want pointer to 42.
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

// TestAtomicConfig_StoreOverwrite verifies the atomic config store overwrite contract.
// Asserts that Load after overwrite = , want pointer to beta.
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

// TestAtomicConfig_ConcurrentAccess verifies the atomic config concurrent access path by exercising ac.Store, wg.Go, ac.Load.
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

	if ac.Load() == nil {
		t.Error("Load after concurrent writes should not be nil")
	}
}
