// -------------------------------------------------------------------------------
// Metrics  -  Cache, Redis
//
// Author: Alex Freidah
//
// Domain-scoped slice of the s3o_* Prometheus surface. Split out of the
// original 784-line metrics.go to keep each subsystem under ~150 lines.
// -------------------------------------------------------------------------------

package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Object data cache hit and miss metrics.
var (
	// CacheHitsTotal counts object data cache hits.
	CacheHitsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_cache_hits_total",
			Help: "Object data cache hits",
		},
	)

	// CacheMissesTotal counts object data cache misses.
	CacheMissesTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_cache_misses_total",
			Help: "Object data cache misses",
		},
	)

	// CacheEvictionsTotal counts cache entries evicted by LRU or TTL.
	CacheEvictionsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_cache_evictions_total",
			Help: "Cache entries evicted (LRU or TTL)",
		},
	)

	// CacheSizeBytes tracks current cache utilization in bytes.
	CacheSizeBytes = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "s3o_cache_size_bytes",
			Help: "Current object data cache size in bytes",
		},
	)

	// CacheEntries tracks the number of entries in the cache.
	CacheEntries = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "s3o_cache_entries",
			Help: "Number of entries in the object data cache",
		},
	)

	// CacheFlushTotal counts admin cache-flush invocations. Useful for
	// auditing how often operators or perf runs reset cache state, and
	// for distinguishing organic eviction from explicit flushes when
	// reading cache_size_bytes / cache_entries dropouts on dashboards.
	CacheFlushTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_cache_flush_total",
			Help: "Admin-triggered object data cache flushes",
		},
	)

	// CacheAdminInvalidationsTotal counts admin-triggered single-key
	// invalidations. Distinct from organic invalidations driven by
	// writes/deletes/replication so dashboards can separate operator
	// actions from background cache churn.
	CacheAdminInvalidationsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_cache_admin_invalidations_total",
			Help: "Admin-triggered single-key cache invalidations",
		},
	)

	// HeadServedFromMetadataTotal counts HEAD responses answered from the
	// object ledger, each one a backend round trip and a metered API call
	// that did not happen. Rises as pre-identity objects are read once and
	// learn their identity.
	HeadServedFromMetadataTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_head_served_from_metadata_total",
			Help: "HEAD responses answered from stored metadata without a backend request",
		},
	)

	// RedisOperationsTotal counts Redis counter backend operations.
	RedisOperationsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_redis_operations_total",
			Help: "Total Redis counter backend operations",
		},
		[]string{"operation", "status"},
	)

	// RedisFallbackActive is 1 when the Redis counter backend is in local
	// fallback mode due to circuit breaker, 0 during normal operation.
	RedisFallbackActive = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "s3o_redis_fallback_active",
			Help: "Whether Redis counter backend is in local fallback mode",
		},
	)
)
