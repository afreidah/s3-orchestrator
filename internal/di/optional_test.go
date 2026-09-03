// -------------------------------------------------------------------------------
// DI - Optional[T] Resolution Tests
//
// Author: Alex Freidah
//
// Covers the three Resolution outcomes Optional[T] reports: Disabled (no
// provider registered for T), Applied (provider resolved cleanly), and
// Failed (provider registered but invoke returned an error, either from
// the constructor itself or a transitive dependency the constructor
// tried to resolve). IsRegistered is exercised directly to confirm it
// agrees with the provider list without invoking the constructor.
// -------------------------------------------------------------------------------

package di

import (
	"errors"
	"testing"

	"github.com/samber/do/v2"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// optionalProbe is a simple service type used only in these tests; it
// keeps the test independent of the rest of the package's wiring.
type optionalProbe struct {
	name string
}

// optionalDep is a probe-only dependency the failing-transitive test
// expects to be missing.
type optionalDep struct{}

// errBoom is the sentinel a failing constructor returns so the test can
// assert the error propagates through Optional[T] without losing
// identity.
var errBoom = errors.New("boom")

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestOptional_Disabled confirms that with no provider registered for T,
// Optional[T] reports Disabled, a zero value, and no error.
func TestOptional_Disabled(t *testing.T) {
	inj := do.New()
	res := Optional[*optionalProbe](inj)
	if res.Resolution != ResolutionDisabled {
		t.Fatalf("expected Disabled, got %s", res.Resolution)
	}
	if res.Value != nil {
		t.Fatalf("expected zero value, got %+v", res.Value)
	}
	if res.Err != nil {
		t.Fatalf("expected nil err, got %v", res.Err)
	}
}

// TestOptional_Applied confirms a registered provider that resolves
// cleanly produces Applied with the constructed value.
func TestOptional_Applied(t *testing.T) {
	inj := do.New()
	do.Provide(inj, func(do.Injector) (*optionalProbe, error) {
		return &optionalProbe{name: "ok"}, nil
	})
	res := Optional[*optionalProbe](inj)
	if res.Resolution != ResolutionApplied {
		t.Fatalf("expected Applied, got %s (err=%v)", res.Resolution, res.Err)
	}
	if res.Value == nil || res.Value.name != "ok" {
		t.Fatalf("unexpected value: %+v", res.Value)
	}
	if res.Err != nil {
		t.Fatalf("expected nil err, got %v", res.Err)
	}
}

// TestOptional_FailedConstructor confirms a provider whose constructor
// returns an error surfaces as Failed with the original error wrapped
// in OptionalResult.Err.
func TestOptional_FailedConstructor(t *testing.T) {
	inj := do.New()
	do.Provide(inj, func(do.Injector) (*optionalProbe, error) {
		return nil, errBoom
	})
	res := Optional[*optionalProbe](inj)
	if !res.Failed() {
		t.Fatalf("expected Failed, got %s", res.Resolution)
	}
	if !errors.Is(res.Err, errBoom) {
		t.Fatalf("expected wrapped errBoom, got %v", res.Err)
	}
}

// TestOptional_FailedTransitiveDep confirms a provider that fails
// because a transitive dependency is missing is reported as Failed (a
// provider is registered) rather than Disabled (which would only fire
// when no provider for T itself is registered).
func TestOptional_FailedTransitiveDep(t *testing.T) {
	inj := do.New()
	do.Provide(inj, func(i do.Injector) (*optionalProbe, error) {
		if _, err := do.Invoke[*optionalDep](i); err != nil {
			return nil, err
		}
		return &optionalProbe{name: "unreachable"}, nil
	})
	res := Optional[*optionalProbe](inj)
	if !res.Failed() {
		t.Fatalf("expected Failed, got %s (err=%v)", res.Resolution, res.Err)
	}
	if res.Err == nil {
		t.Fatalf("expected non-nil err on Failed resolution")
	}
}

// TestOptional_NilInjector confirms a nil injector is treated as
// Disabled rather than panicking; the production code paths can pass a
// nil injector in reduced run modes.
func TestOptional_NilInjector(t *testing.T) {
	res := Optional[*optionalProbe](nil)
	if res.Resolution != ResolutionDisabled {
		t.Fatalf("expected Disabled, got %s", res.Resolution)
	}
}

// TestIsRegistered confirms the presence check agrees with the provider
// list both before and after registration, without invoking the
// constructor (the constructor would panic if invoked).
func TestIsRegistered(t *testing.T) {
	inj := do.New()
	if IsRegistered[*optionalProbe](inj) {
		t.Fatalf("expected unregistered probe to report false")
	}
	do.Provide(inj, func(do.Injector) (*optionalProbe, error) {
		t.Fatalf("constructor should not run for IsRegistered check")
		return nil, nil
	})
	if !IsRegistered[*optionalProbe](inj) {
		t.Fatalf("expected registered probe to report true")
	}
}

// TestIsRegistered_NilInjector confirms a nil injector reports false
// rather than panicking on ListProvidedServices.
func TestIsRegistered_NilInjector(t *testing.T) {
	if IsRegistered[*optionalProbe](nil) {
		t.Fatalf("expected false for nil injector")
	}
}
