// -------------------------------------------------------------------------------
// Rate Limit and Circuit Breaker Configuration
//
// Author: Alex Freidah
//
// Defines RateLimitConfig (per-IP login throttling tunables and the
// stale-entry eviction window) and CircuitBreakerConfig (failure
// threshold, open-state timeout, cache TTL during degraded reads).
// Validators enforce positive intervals and sensible relationships
// between fields so a misconfiguration cannot accidentally produce a
// no-op rate limiter or a circuit breaker that never trips.
// -------------------------------------------------------------------------------

package config

import (
	"fmt"
	"net"
	"time"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// RateLimitConfig holds per-IP rate limiting settings. Disabled by default.
type RateLimitConfig struct {
	Enabled         bool          `yaml:"enabled"`
	RequestsPerSec  float64       `yaml:"requests_per_sec"` // Token refill rate (default: 100)
	Burst           int           `yaml:"burst"`            // Max burst size (default: 200)
	TrustedProxies  []string      `yaml:"trusted_proxies"`  // CIDRs whose X-Forwarded-For is trusted (e.g. ["10.0.0.0/8", "172.16.0.0/12"])
	CleanupInterval time.Duration `yaml:"cleanup_interval"` // How often stale entries are evicted (default: 1m)
	CleanupMaxAge   time.Duration `yaml:"cleanup_max_age"`  // Entries older than this are evicted (default: 5m)
}

// CircuitBreakerConfig holds settings for the database circuit breaker. When
// the database becomes unreachable, the proxy enters degraded mode: reads
// broadcast to all backends, writes return 503.
//
// DegradedBroadcastParallelism only means anything when ParallelBroadcast is
// on. Zero probes every configured backend at once; a positive value makes the
// probes a rolling window, launching the first N and replenishing from the
// pending list as each one fails, so at most N are ever in flight.
type CircuitBreakerConfig struct {
	FailureThreshold  int           `yaml:"failure_threshold"`  // consecutive failures before opening (default 3)
	OpenTimeout       time.Duration `yaml:"open_timeout"`       // delay before probing recovery (default 15s)
	CacheTTL          time.Duration `yaml:"cache_ttl"`          // TTL for the key->backend cache during degraded reads (default 60s)
	ParallelBroadcast bool          `yaml:"parallel_broadcast"` // fan degraded reads out to all backends at once (default false)

	DegradedBroadcastParallelism int   `yaml:"degraded_broadcast_parallelism"` // 0 is uncapped
	DegradedReadsEnabled         *bool `yaml:"degraded_reads_enabled"`         // default true; false fails fast on a DB outage
}

// BackendCircuitBreakerConfig holds settings for per-backend circuit breakers.
// When a backend is unreachable or returns errors (e.g. expired credentials),
// the circuit opens and the backend is excluded from request routing until
// recovery is detected via a probe request.
type BackendCircuitBreakerConfig struct {
	Enabled          bool          `yaml:"enabled"`           // Enable per-backend circuit breakers (default: false)
	FailureThreshold int           `yaml:"failure_threshold"` // Consecutive failures before opening (default: 5)
	OpenTimeout      time.Duration `yaml:"open_timeout"`      // Delay before probing recovery (default: 5m)
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// setDefaultsAndValidate sets defaults and validate.
func (r *RateLimitConfig) setDefaultsAndValidate() []error {
	var errs []error

	// Validate CIDR syntax even when disabled so typos are caught at
	// startup rather than surfacing on a later SIGHUP that enables
	// rate limiting.
	for _, cidr := range r.TrustedProxies {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			errs = append(errs, fmt.Errorf("%w: %q: %w", ErrInvalidCIDR, cidr, err))
		}
	}

	if !r.Enabled {
		return errs
	}

	r.RequestsPerSec = defaulted(r.RequestsPerSec, 100)
	r.Burst = defaulted(r.Burst, 200)
	r.CleanupInterval = defaulted(r.CleanupInterval, 1*time.Minute)
	r.CleanupMaxAge = defaulted(r.CleanupMaxAge, 5*time.Minute)

	if r.RequestsPerSec <= 0 {
		errs = append(errs, ErrRateLimitRPSNotPositive)
	}
	if r.Burst <= 0 {
		errs = append(errs, ErrRateLimitBurstNotPositive)
	}

	return errs
}

// setDefaults sets defaults.
func (cb *CircuitBreakerConfig) setDefaults() {
	cb.FailureThreshold = defaulted(cb.FailureThreshold, 3)
	cb.OpenTimeout = defaulted(cb.OpenTimeout, 15*time.Second)
	cb.CacheTTL = defaulted(cb.CacheTTL, 60*time.Second)
	// DegradedBroadcastParallelism intentionally has no positive default:
	// zero means fan out to every backend, and operators opt into the
	// rolling-window cap by setting a positive value. A negative value is
	// normalised to zero so a typo cannot accidentally disable the
	// broadcast entirely.
	if cb.DegradedBroadcastParallelism < 0 {
		cb.DegradedBroadcastParallelism = 0
	}
	// *bool so unset (nil) is distinguishable from explicit false.
	if cb.DegradedReadsEnabled == nil {
		t := true
		cb.DegradedReadsEnabled = &t
	}
}

// setDefaults sets defaults.
func (bcb *BackendCircuitBreakerConfig) setDefaults() {
	if !bcb.Enabled {
		return
	}
	bcb.FailureThreshold = defaulted(bcb.FailureThreshold, 5)
	bcb.OpenTimeout = defaulted(bcb.OpenTimeout, 5*time.Minute)
}
