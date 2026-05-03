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

// CacheHitsTotal and related package-level variables used by this package.
var (
	// --- Cache metrics ---

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

	// --- Redis metrics ---

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
