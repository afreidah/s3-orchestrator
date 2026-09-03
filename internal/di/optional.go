// -------------------------------------------------------------------------------
// DI - Optional Dependency Resolution
//
// Author: Alex Freidah
//
// Production-grade resolution helpers for optional services. Distinguishes
// "feature intentionally disabled" (no provider registered) from "feature
// configured but failed to initialize" (provider registered, construction
// or dependency wiring returned an error) so operators can tell a quiet
// disabled feature from a broken one. Optional[T] is the typed surface;
// IsRegistered[T] is a cheap presence check that does not invoke the
// constructor.
// -------------------------------------------------------------------------------

package di

import (
	"fmt"

	"github.com/samber/do/v2"

	objcache "github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Resolution classifies the outcome of an optional dependency lookup.
type Resolution string

const (
	ResolutionDisabled Resolution = "disabled" // no provider registered for T
	ResolutionApplied  Resolution = "applied"  // the provider resolved cleanly
	ResolutionFailed   Resolution = "failed"   // registered, but construction failed
)

// OptionalResult carries the outcome of an Optional[T] lookup. Callers
// pick the field they care about: Value for the resolved instance,
// Resolution for the operational classification, Err for the failure
// detail when Resolution is Failed.
type OptionalResult[T any] struct {
	Value      T
	Resolution Resolution
	Err        error
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Failed reports whether the provider was registered but failed to
// construct (or one of its transitive dependencies failed).
func (r OptionalResult[T]) Failed() bool { return r.Resolution == ResolutionFailed }

// Optional resolves T as an optional dependency. Inspects the injector's
// registered service list first so a missing provider is distinguished
// from a constructor failure: a Disabled result means the feature was
// never wired in this run mode, while a Failed result means a provider
// was wired but its constructor (or a transitive dependency it tried
// to resolve) returned an error.
func Optional[T any](inj do.Injector) OptionalResult[T] {
	if inj == nil {
		return OptionalResult[T]{Resolution: ResolutionDisabled}
	}
	if !IsRegistered[T](inj) {
		return OptionalResult[T]{Resolution: ResolutionDisabled}
	}
	v, err := do.Invoke[T](inj)
	if err != nil {
		return OptionalResult[T]{Resolution: ResolutionFailed, Err: err}
	}
	return OptionalResult[T]{Value: v, Resolution: ResolutionApplied}
}

// IsRegistered reports whether T has a provider registered in the
// injector or any ancestor scope. Cheap; does not invoke the constructor.
func IsRegistered[T any](inj do.Injector) bool {
	if inj == nil {
		return false
	}
	name := do.NameOf[T]()
	for _, svc := range inj.ListProvidedServices() {
		if svc.Service == name {
			return true
		}
	}
	return false
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// resolveOptionalCounterBackend returns the configured Redis counter
// backend, or nil when Redis is disabled / not registered. The runtime
// builder treats nil as "use the local counter backend".
func resolveOptionalCounterBackend(i do.Injector) counter.Backend {
	rb, err := do.Invoke[*counter.RedisCounterBackend](i)
	if err != nil {
		return nil
	}
	return rb
}

// resolveOptionalCache returns the object data cache, or nil when
// caching is disabled / not registered. The object manager treats nil as
// "object data caching is off" (read path bypasses the cache layer).
func resolveOptionalCache(i do.Injector) objcache.ObjectCache {
	c, err := do.Invoke[objcache.ObjectCache](i)
	if err != nil {
		return nil
	}
	return c
}

// resolveOptionalEncryptor returns the live *encryption.Encryptor when
// encryption is enabled, or nil otherwise. A configured-but-failing
// encryptor surfaces as an error so the admin handler does not quietly
// run without encryption support.
func resolveOptionalEncryptor(i do.Injector, enabled bool) (*encryption.Encryptor, error) {
	if !enabled {
		return nil, nil //nolint:nilnil // encryption disabled is a valid state
	}
	e, err := do.Invoke[*encryption.Encryptor](i)
	if err != nil {
		return nil, fmt.Errorf("encryption enabled but encryptor failed to initialize: %w", err)
	}
	return e, nil
}
