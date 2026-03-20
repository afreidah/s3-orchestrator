// -------------------------------------------------------------------------------
// Rate Limit and Circuit Breaker Configuration
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package config

import (
	"fmt"
	"net"
	"time"
)

// RateLimitConfig holds per-IP rate limiting settings. Disabled by default.
type RateLimitConfig struct {
	Enabled         bool          `yaml:"enabled"`
	RequestsPerSec  float64       `yaml:"requests_per_sec"`  // Token refill rate (default: 100)
	Burst           int           `yaml:"burst"`              // Max burst size (default: 200)
	TrustedProxies  []string      `yaml:"trusted_proxies"`    // CIDRs whose X-Forwarded-For is trusted (e.g. ["10.0.0.0/8", "172.16.0.0/12"])
	CleanupInterval time.Duration `yaml:"cleanup_interval"`   // How often stale entries are evicted (default: 1m)
	CleanupMaxAge   time.Duration `yaml:"cleanup_max_age"`    // Entries older than this are evicted (default: 5m)
}

// CircuitBreakerConfig holds settings for the database circuit breaker. When
// the database becomes unreachable, the proxy enters degraded mode: reads
// broadcast to all backends, writes return 503.
type CircuitBreakerConfig struct {
	FailureThreshold  int           `yaml:"failure_threshold"`  // Consecutive failures before opening (default: 3)
	OpenTimeout       time.Duration `yaml:"open_timeout"`       // Delay before probing recovery (default: 15s)
	CacheTTL          time.Duration `yaml:"cache_ttl"`          // TTL for key→backend cache during degraded reads (default: 60s)
	ParallelBroadcast bool          `yaml:"parallel_broadcast"` // Fan-out reads to all backends in parallel during degraded mode (default: false)
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

func (r *RateLimitConfig) setDefaultsAndValidate() []string {
	if !r.Enabled {
		return nil
	}

	var errs []string

	if r.RequestsPerSec == 0 {
		r.RequestsPerSec = 100
	}
	if r.Burst == 0 {
		r.Burst = 200
	}
	if r.CleanupInterval == 0 {
		r.CleanupInterval = 1 * time.Minute
	}
	if r.CleanupMaxAge == 0 {
		r.CleanupMaxAge = 5 * time.Minute
	}

	if r.RequestsPerSec <= 0 {
		errs = append(errs, "rate_limit.requests_per_sec must be positive")
	}
	if r.Burst <= 0 {
		errs = append(errs, "rate_limit.burst must be positive")
	}
	for _, cidr := range r.TrustedProxies {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			errs = append(errs, fmt.Sprintf("rate_limit.trusted_proxies: invalid CIDR %q: %v", cidr, err))
		}
	}

	return errs
}

func (cb *CircuitBreakerConfig) setDefaults() {
	if cb.FailureThreshold == 0 {
		cb.FailureThreshold = 3
	}
	if cb.OpenTimeout == 0 {
		cb.OpenTimeout = 15 * time.Second
	}
	if cb.CacheTTL == 0 {
		cb.CacheTTL = 60 * time.Second
	}
}

func (bcb *BackendCircuitBreakerConfig) setDefaults() {
	if !bcb.Enabled {
		return
	}
	if bcb.FailureThreshold == 0 {
		bcb.FailureThreshold = 5
	}
	if bcb.OpenTimeout == 0 {
		bcb.OpenTimeout = 5 * time.Minute
	}
}
