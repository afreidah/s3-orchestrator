// -------------------------------------------------------------------------------
// must - Tests
//
// Author: Alex Freidah
//
// Pins the panic-on-nil contract and the typed-nil-interface case the
// bare equality comparison misses. Both branches must trigger or
// future constructors lose the early-failure guarantee.
// -------------------------------------------------------------------------------

package must

import (
	"strings"
	"testing"
)

// concreteStore is a minimal pointer type used to drive the
// typed-nil-interface case below.
type concreteStore struct{}

// storer is the interface a typed-nil pointer satisfies; the test
// passes a (*concreteStore)(nil) through this interface to exercise
// the reflect-based check.
type storer interface {
	Get() string
}

func (c *concreteStore) Get() string { return "" }

// TestNotNil_AcceptsNonNilPointer pins the happy path: a real
// dependency must not panic. Without this, a refactor that breaks the
// canonical case slips through the other tests.
func TestNotNil_AcceptsNonNilPointer(t *testing.T) {
	t.Parallel()
	NotNil("dep", &concreteStore{})
}

// TestNotNil_PanicsOnUntypedNil pins the obvious case: passing a
// literal nil. Panic message must name the dependency so triage knows
// which one was missing.
func TestNotNil_PanicsOnUntypedNil(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on nil")
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, "dep") {
			t.Errorf("panic message = %v, want to contain dependency name", r)
		}
	}()
	NotNil("dep", nil)
}

// TestNotNil_PanicsOnTypedNilInterface pins the trap case the
// reflect-based check exists to catch. v != nil at the interface
// level because it carries type info, but the underlying pointer is
// nil and dereferencing it would NPE the same way an untyped nil
// would.
func TestNotNil_PanicsOnTypedNilInterface(t *testing.T) {
	t.Parallel()
	var s storer = (*concreteStore)(nil) // typed nil; s != nil at interface level
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on typed-nil interface")
		}
	}()
	NotNil("dep", s)
}

// TestNotNil_PanicsOnNilMap covers a non-pointer nilable kind so a
// future caller passing a nil map (e.g. a routing table) still gets
// the early-failure guarantee instead of a deferred NPE during the
// first lookup.
func TestNotNil_PanicsOnNilMap(t *testing.T) {
	t.Parallel()
	var m map[string]int
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on nil map")
		}
	}()
	NotNil("routes", m)
}
