// -------------------------------------------------------------------------------
// DI - Optional Dependency Resolution Helpers
//
// Author: Alex Freidah
//
// Small helpers used by the provider files to resolve dependencies that are
// only present when a feature flag is enabled. The generic invokeOptional
// returns the zero value when the service is not registered; the typed
// resolveOptional* helpers add per-feature semantics (nil-on-absence vs.
// error-on-failure for the encryptor).
// -------------------------------------------------------------------------------

package di

import (
	"fmt"

	"github.com/samber/do/v2"

	objcache "github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
)

// invokeOptional resolves a provider that may not be registered, returning
// the zero value of T when the service is absent. Used for admin handler
// dependencies that only register under specific modes/features.
func invokeOptional[T any](i do.Injector) T {
	v, _ := do.Invoke[T](i)
	return v
}

// resolveOptionalCounterBackend returns the configured Redis counter
// backend, or nil when Redis is disabled / not registered. The
// CounterBackend field on BackendManagerConfig accepts nil to mean
// "use the local counter backend".
func resolveOptionalCounterBackend(i do.Injector) counter.CounterBackend {
	rb, err := do.Invoke[*counter.RedisCounterBackend](i)
	if err != nil {
		return nil
	}
	return rb
}

// resolveOptionalCache returns the object data cache, or nil when
// caching is disabled / not registered. NewBackendManager treats nil as
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
// encryptor surfaces as an error so the admin handler doesn't quietly
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
