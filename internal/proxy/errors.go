// -------------------------------------------------------------------------------
// Manager Constructor Sentinel Errors
//
// Author: Alex Freidah
//
// Sentinels returned by NewBackendManager when the supplied configuration
// is missing a required field or violates a runtime invariant. Wrapped via
// fmt.Errorf("BackendManager: %w: <detail>", ErrX) at the call site so
// callers can both errors.Is the sentinel and read a descriptive message.
// Sentinel pattern mirrors internal/config/errors.go so tests can match on
// the wrapped value without depending on the message text.
// -------------------------------------------------------------------------------

package proxy

import "errors"

// Constructor input validation errors.
var (
	// ErrConfigNil signals that NewBackendManager was called with a nil
	// *BackendManagerConfig pointer.
	ErrConfigNil = errors.New("config is nil")

	// ErrStoresRequired signals that BackendManagerConfig.Stores is nil.
	// Stores carries the metadata-store contract and is dereferenced in
	// every write path, so a nil value would NPE on first use.
	ErrStoresRequired = errors.New("stores is required")

	// ErrDashboardRequired signals that BackendManagerConfig.Dashboard is
	// nil. The dashboard aggregator is constructed eagerly inside the
	// manager, so a nil value here would NPE at boot rather than at first
	// dashboard request.
	ErrDashboardRequired = errors.New("dashboard is required")

	// ErrMetricsRequired signals that BackendManagerConfig.Metrics is
	// nil. The metrics collector is constructed eagerly inside
	// backendCore, so an unset metrics.Deps would NPE the first time a
	// metric scrape happens.
	ErrMetricsRequired = errors.New("metrics is required")

	// ErrNegativeBackendTimeout signals that BackendManagerConfig.BackendTimeout
	// is negative. Zero is allowed (interpreted as "no per-call deadline")
	// because production paths derive deadlines from the request context.
	ErrNegativeBackendTimeout = errors.New("backend_timeout must not be negative")

	// ErrNegativeCacheTTL signals that BackendManagerConfig.CacheTTL is
	// negative. Zero is allowed (interpreted as "no TTL").
	ErrNegativeCacheTTL = errors.New("cache_ttl must not be negative")

	// ErrNegativeMultipartDEKCacheTTL signals that
	// BackendManagerConfig.MultipartDEKCacheTTL is negative. Zero is
	// allowed (interpreted as "use the 1h default").
	ErrNegativeMultipartDEKCacheTTL = errors.New("multipart_dek_cache_ttl must not be negative")
)
