// -------------------------------------------------------------------------------
// Metrics  -  Cache, Redis
//
// Author: Alex Freidah
//
// Domain-scoped slice of the s3o_* Prometheus surface. Split out of the
// original 784-line metrics.go to keep each subsystem under ~150 lines.
// -------------------------------------------------------------------------------

package telemetry

// Object data cache hit and miss metrics.
var (
	// CacheHitsTotal counts object data cache hits.
	CacheHitsTotal = counter("s3o_cache_hits_total", "Object data cache hits")

	// CacheMissesTotal counts object data cache misses.
	CacheMissesTotal = counter("s3o_cache_misses_total", "Object data cache misses")

	// CacheEvictionsTotal counts cache entries evicted by LRU or TTL.
	CacheEvictionsTotal = counter("s3o_cache_evictions_total", "Cache entries evicted (LRU or TTL)")

	// CacheSizeBytes tracks current cache utilization in bytes.
	CacheSizeBytes = gauge("s3o_cache_size_bytes", "Current object data cache size in bytes")

	// CacheEntries tracks the number of entries in the cache.
	CacheEntries = gauge("s3o_cache_entries", "Number of entries in the object data cache")

	// CacheFlushTotal counts admin cache-flush invocations. Useful for
	// auditing how often operators or perf runs reset cache state, and
	// for distinguishing organic eviction from explicit flushes when
	// reading cache_size_bytes / cache_entries dropouts on dashboards.
	CacheFlushTotal = counter("s3o_cache_flush_total", "Admin-triggered object data cache flushes")

	// CacheAdminInvalidationsTotal counts admin-triggered single-key
	// invalidations. Distinct from organic invalidations driven by
	// writes/deletes/replication so dashboards can separate operator
	// actions from background cache churn.
	CacheAdminInvalidationsTotal = counter("s3o_cache_admin_invalidations_total",
		"Admin-triggered single-key cache invalidations")

	// HeadServedFromMetadataTotal counts HEAD responses answered from the
	// object ledger, each one a backend round trip and a metered API call
	// that did not happen. Rises as pre-identity objects are read once and
	// learn their identity.
	HeadServedFromMetadataTotal = counter("s3o_head_served_from_metadata_total",
		"HEAD responses answered from stored metadata without a backend request")

	// RedisOperationsTotal counts Redis counter backend operations.
	RedisOperationsTotal = counterVec("s3o_redis_operations_total",
		"Total Redis counter backend operations", "operation", "status")

	// RedisFallbackActive is 1 when the Redis counter backend is in local
	// fallback mode due to circuit breaker, 0 during normal operation.
	RedisFallbackActive = gauge("s3o_redis_fallback_active",
		"Whether Redis counter backend is in local fallback mode")
)
