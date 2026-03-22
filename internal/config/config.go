// -------------------------------------------------------------------------------
// Configuration - S3 Orchestrator Settings
//
// Author: Alex Freidah
//
// Configuration types and loader for the S3 proxy. Supports environment variable
// expansion in YAML values using ${VAR} syntax. Types are split into domain
// files; this file holds the root Config struct, loader, cross-field validation,
// and hot-reload change detection.
// -------------------------------------------------------------------------------

// Package config provides YAML configuration loading with environment variable
// expansion, validation, and hot-reload support via SIGHUP.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// -------------------------------------------------------------------------
// ROOT CONFIG
// -------------------------------------------------------------------------

// Config holds the complete service configuration.
type Config struct {
	Server         ServerConfig         `yaml:"server"`
	Buckets        []BucketConfig       `yaml:"buckets"`
	Database       DatabaseConfig       `yaml:"database"`
	Backends       []BackendConfig      `yaml:"backends"`
	Telemetry      TelemetryConfig      `yaml:"telemetry"`
	Rebalance      RebalanceConfig      `yaml:"rebalance"`
	Replication    ReplicationConfig    `yaml:"replication"`
	RateLimit      RateLimitConfig      `yaml:"rate_limit"`
	CircuitBreaker        CircuitBreakerConfig        `yaml:"circuit_breaker"`
	BackendCircuitBreaker BackendCircuitBreakerConfig `yaml:"backend_circuit_breaker"`
	Encryption            EncryptionConfig            `yaml:"encryption"`
	UI                    UIConfig                    `yaml:"ui"`
	CleanupQueue    CleanupQueueConfig   `yaml:"cleanup_queue"`
	UsageFlush      UsageFlushConfig     `yaml:"usage_flush"`
	Lifecycle       LifecycleConfig      `yaml:"lifecycle"`
	Reconcile       ReconcileConfig      `yaml:"reconcile"`
	Redis           *RedisConfig         `yaml:"redis"`
	RoutingStrategy string               `yaml:"routing_strategy"` // "pack" (default) or "spread"
}

// -------------------------------------------------------------------------
// LOADER
// -------------------------------------------------------------------------

// LoadConfig reads and parses the configuration file with environment variable
// expansion. Returns an error if the file cannot be read, parsed, or validated.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	expanded := os.Expand(string(data), os.Getenv)

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

// -------------------------------------------------------------------------
// VALIDATION COORDINATOR
// -------------------------------------------------------------------------

// SetDefaultsAndValidate applies default values for optional fields and checks
// that all required configuration values are present. Delegates to per-type
// validators and performs cross-field validation.
func (c *Config) SetDefaultsAndValidate() error {
	var errs []string

	// Per-type defaults and validation
	errs = append(errs, c.Server.setDefaultsAndValidate()...)
	errs = append(errs, c.Database.setDefaultsAndValidate()...)
	errs = append(errs, validateBuckets(c.Buckets)...)
	errs = append(errs, validateBackends(c.Backends)...)
	errs = append(errs, c.Telemetry.setDefaultsAndValidate()...)
	errs = append(errs, c.Rebalance.setDefaultsAndValidate()...)
	errs = append(errs, c.Replication.setDefaultsAndValidate(len(c.Backends))...)
	errs = append(errs, c.RateLimit.setDefaultsAndValidate()...)
	errs = append(errs, c.Encryption.setDefaultsAndValidate()...)
	errs = append(errs, c.UI.setDefaultsAndValidate()...)
	errs = append(errs, c.UsageFlush.setDefaultsAndValidate()...)
	errs = append(errs, validateLifecycleRules(c.Lifecycle.Rules)...)

	// Defaults-only types (no validation errors)
	c.CircuitBreaker.setDefaults()
	c.BackendCircuitBreaker.setDefaults()

	if c.CleanupQueue.Concurrency <= 0 {
		c.CleanupQueue.Concurrency = 10
	}
	if c.Reconcile.Enabled && c.Reconcile.Interval <= 0 {
		c.Reconcile.Interval = 24 * time.Hour
	}
	if c.Redis != nil {
		errs = append(errs, c.Redis.setDefaultsAndValidate()...)
	}

	// Routing strategy
	if c.RoutingStrategy == "" {
		c.RoutingStrategy = "pack"
	}
	if c.RoutingStrategy != "pack" && c.RoutingStrategy != "spread" {
		errs = append(errs, "routing_strategy must be 'pack' or 'spread'")
	}

	// Cross-field: quota and replication combinations
	if len(c.Backends) > 1 {
		unlimitedCount := 0
		for i := range c.Backends {
			if c.Backends[i].QuotaBytes == 0 {
				unlimitedCount++
			}
		}
		quotaCount := len(c.Backends) - unlimitedCount

		if unlimitedCount > 0 && quotaCount > 0 {
			errs = append(errs, "cannot mix unlimited (quota_bytes: 0) and quota-limited backends; either all backends must have quotas for overflow routing or all must be unlimited with replication")
		}
		if unlimitedCount > 1 && c.Replication.Factor <= 1 {
			errs = append(errs, "multiple backends with unlimited quota (quota_bytes: 0) requires replication.factor >= 2; without quotas there is no overflow routing and only the first backend would receive writes")
		}
		if c.Replication.Factor <= 1 {
			slog.Warn("replication.factor <= 1 with multiple backends provides no redundancy — losing a backend will cause permanent data loss for objects stored exclusively on it", //nolint:sloglint // config validation has no request context
				"backends", len(c.Backends), "replication_factor", c.Replication.Factor)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// -------------------------------------------------------------------------
// HOT-RELOAD CHANGE DETECTION
// -------------------------------------------------------------------------

// NonReloadableFieldsChanged compares two configs and returns a list of
// non-reloadable field descriptions that differ. Used by the SIGHUP handler
// to warn about changes that require a restart.
func NonReloadableFieldsChanged(old, new *Config) []string {
	var changed []string

	if old.Server.ListenAddr != new.Server.ListenAddr {
		changed = append(changed, "server.listen_addr")
	}
	if old.Server.MaxConcurrentRequests != new.Server.MaxConcurrentRequests {
		changed = append(changed, "server.max_concurrent_requests")
	}
	if old.Server.MaxConcurrentReads != new.Server.MaxConcurrentReads {
		changed = append(changed, "server.max_concurrent_reads")
	}
	if old.Server.MaxConcurrentWrites != new.Server.MaxConcurrentWrites {
		changed = append(changed, "server.max_concurrent_writes")
	}
	if old.Server.LoadShedThreshold != new.Server.LoadShedThreshold {
		changed = append(changed, "server.load_shed_threshold")
	}
	if old.Server.AdmissionWait != new.Server.AdmissionWait {
		changed = append(changed, "server.admission_wait")
	}
	if old.Server.ReadHeaderTimeout != new.Server.ReadHeaderTimeout ||
		old.Server.ReadTimeout != new.Server.ReadTimeout ||
		old.Server.WriteTimeout != new.Server.WriteTimeout ||
		old.Server.IdleTimeout != new.Server.IdleTimeout {
		changed = append(changed, "server timeouts (read_header_timeout, read_timeout, write_timeout, idle_timeout)")
	}
	if old.Server.ShutdownDelay != new.Server.ShutdownDelay {
		changed = append(changed, "server.shutdown_delay")
	}
	if old.Server.TLS != new.Server.TLS {
		changed = append(changed, "server.tls")
	}
	if old.Database != new.Database {
		changed = append(changed, "database")
	}
	if old.Telemetry != new.Telemetry {
		changed = append(changed, "telemetry")
	}
	if old.UI != new.UI {
		changed = append(changed, "ui")
	}
	if old.CircuitBreaker != new.CircuitBreaker {
		changed = append(changed, "circuit_breaker")
	}
	if old.BackendCircuitBreaker != new.BackendCircuitBreaker {
		changed = append(changed, "backend_circuit_breaker")
	}
	if old.Encryption.Enabled != new.Encryption.Enabled ||
		old.Encryption.MasterKey != new.Encryption.MasterKey ||
		old.Encryption.MasterKeyFile != new.Encryption.MasterKeyFile ||
		old.Encryption.ChunkSize != new.Encryption.ChunkSize {
		changed = append(changed, "encryption")
	}
	if old.RoutingStrategy != new.RoutingStrategy {
		changed = append(changed, "routing_strategy")
	}
	oldHasRedis := old.Redis != nil
	newHasRedis := new.Redis != nil
	if oldHasRedis != newHasRedis {
		changed = append(changed, "redis")
	} else if oldHasRedis && newHasRedis && *old.Redis != *new.Redis {
		changed = append(changed, "redis")
	}

	// Backend structural changes (endpoints, S3 credentials) cannot be reloaded.
	// Quota and usage limit changes ARE reloadable and handled separately.
	if len(old.Backends) != len(new.Backends) {
		changed = append(changed, "backends (count changed)")
	} else {
		for i := range old.Backends {
			o, n := old.Backends[i], new.Backends[i]
			if o.Name != n.Name || o.Endpoint != n.Endpoint || o.Region != n.Region ||
				o.Bucket != n.Bucket || o.AccessKeyID != n.AccessKeyID ||
				o.SecretAccessKey != n.SecretAccessKey || o.ForcePathStyle != n.ForcePathStyle ||
				boolDefault(o.UnsignedPayload, true) != boolDefault(n.UnsignedPayload, true) ||
				o.DisableChecksum != n.DisableChecksum ||
				o.StripSDKHeaders != n.StripSDKHeaders {
				changed = append(changed, fmt.Sprintf("backends[%d] (%s) structural fields", i, o.Name))
			}
		}
	}

	return changed
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// boolDefault returns the value of a *bool, or the given default if nil.
func boolDefault(p *bool, def bool) bool {
	if p != nil {
		return *p
	}
	return def
}

// ParseLogLevel maps a log level string to a slog.Level. Returns slog.LevelInfo
// for unrecognized values. Callers should validate via SetDefaultsAndValidate
// before calling this function.
func ParseLogLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
