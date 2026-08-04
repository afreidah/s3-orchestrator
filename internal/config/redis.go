// -------------------------------------------------------------------------------
// Redis Configuration
//
// Author: Alex Freidah
//
// Defines the optional Redis block used for shared usage counters across
// multiple orchestrator instances. When absent, the orchestrator uses
// in-process atomic counters (single-instance only). When present, every
// instance reads and writes the same counter keys so quota enforcement
// stays consistent under horizontal scaling. Holds the connection
// address, pool size, and the timeouts that bound liveness and command
// latency.
// -------------------------------------------------------------------------------

package config

import (
	"cmp"
	"time"
)

// RedisConfig holds optional Redis connection settings for shared usage
// counters in multi-instance deployments. When omitted, counters are stored
// in local memory (single-instance default).
type RedisConfig struct {
	Address          string        `yaml:"address"`           // Redis address (host:port)
	Password         string        `yaml:"password"` //nolint:gosec // G117: config struct, not a hardcoded credential
	DB               int           `yaml:"db"`                // Redis database number (default: 0)
	TLS              bool          `yaml:"tls"`               // Use TLS for Redis connection
	KeyPrefix        string        `yaml:"key_prefix"`        // Key prefix for namespacing (default: "s3orch")
	FailureThreshold int           `yaml:"failure_threshold"` // Consecutive failures before circuit opens (default: 3)
	OpenTimeout      time.Duration `yaml:"open_timeout"`      // Delay before probing recovery (default: 15s)
}

// setDefaultsAndValidate sets defaults and validate.
func (r *RedisConfig) setDefaultsAndValidate() []error {
	var errs []error

	if r.Address == "" {
		errs = append(errs, ErrRedisAddressRequired)
	}
	r.KeyPrefix = cmp.Or(r.KeyPrefix, "s3orch")
	r.FailureThreshold = cmp.Or(r.FailureThreshold, 3)
	r.OpenTimeout = cmp.Or(r.OpenTimeout, 15*time.Second)

	if r.FailureThreshold < 0 {
		errs = append(errs, ErrRedisFailureThresholdNotPos)
	}
	if r.OpenTimeout < 0 {
		errs = append(errs, ErrRedisOpenTimeoutNotPos)
	}

	return errs
}
