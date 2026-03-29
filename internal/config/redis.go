// -------------------------------------------------------------------------------
// Redis Configuration
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package config

import "time"

// RedisConfig holds optional Redis connection settings for shared usage
// counters in multi-instance deployments. When omitted, counters are stored
// in local memory (single-instance default).
type RedisConfig struct {
	Address          string        `yaml:"address"`           // Redis address (host:port)
	Password         string        `yaml:"password"`          // Redis password (optional)
	DB               int           `yaml:"db"`                // Redis database number (default: 0)
	TLS              bool          `yaml:"tls"`               // Use TLS for Redis connection
	KeyPrefix        string        `yaml:"key_prefix"`        // Key prefix for namespacing (default: "s3orch")
	FailureThreshold int           `yaml:"failure_threshold"` // Consecutive failures before circuit opens (default: 3)
	OpenTimeout      time.Duration `yaml:"open_timeout"`      // Delay before probing recovery (default: 15s)
}

func (r *RedisConfig) setDefaultsAndValidate() []string {
	var errs []string

	if r.Address == "" {
		errs = append(errs, "redis.address is required when redis section is present")
	}
	if r.KeyPrefix == "" {
		r.KeyPrefix = "s3orch"
	}
	if r.FailureThreshold == 0 {
		r.FailureThreshold = 3
	}
	if r.OpenTimeout == 0 {
		r.OpenTimeout = 15 * time.Second
	}

	if r.FailureThreshold < 0 {
		errs = append(errs, "redis.failure_threshold must be positive")
	}
	if r.OpenTimeout < 0 {
		errs = append(errs, "redis.open_timeout must be positive")
	}

	return errs
}
