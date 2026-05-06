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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"gopkg.in/yaml.v3"
	"context"
)

// -------------------------------------------------------------------------
// ROUTING STRATEGY
// -------------------------------------------------------------------------

// RoutingStrategy determines how write operations select a target backend.
type RoutingStrategy string

// RoutingPack and related constants used by this package.
const (
	// RoutingPack fills backends in order, returning the first with space.
	RoutingPack RoutingStrategy = "pack"

	// RoutingSpread distributes writes across backends by utilization ratio.
	RoutingSpread RoutingStrategy = "spread"
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
	WritePath       WritePathConfig      `yaml:"write_path"`
	UsageFlush      UsageFlushConfig     `yaml:"usage_flush"`
	Lifecycle       LifecycleConfig      `yaml:"lifecycle"`
	Reconcile       ReconcileConfig      `yaml:"reconcile"`
	Integrity       IntegrityConfig      `yaml:"integrity"`
	Cache           CacheConfig          `yaml:"cache"`
	Redis           *RedisConfig         `yaml:"redis"`
	Notifications   NotificationConfig   `yaml:"notifications"`
	RoutingStrategy RoutingStrategy      `yaml:"routing_strategy"` // "pack" (default) or "spread"
}

// -------------------------------------------------------------------------
// LOADER
// -------------------------------------------------------------------------

// LoadConfig reads and parses the configuration file with environment variable
// expansion. Returns an error if the file cannot be read, parsed, or validated.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, wrapped(ErrReadConfigFile, err)
	}

	expanded := os.Expand(string(data), os.Getenv)

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, wrapped(ErrParseConfig, err)
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		return nil, wrapped(ErrInvalidConfig, err)
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
	var errs []error
	errs = append(errs, c.validatePerTypeSections()...)
	c.applyDefaultsOnlyTypes()
	errs = append(errs, c.validateRoutingStrategy()...)
	errs = append(errs, c.validateQuotaReplicationCombo()...)
	return errors.Join(errs...)
}

// validatePerTypeSections delegates to each sub-type's
// setDefaultsAndValidate and collects the results. Keeping this list in one
// place makes it easy to see what the full validation surface covers.
func (c *Config) validatePerTypeSections() []error {
	var errs []error
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
	errs = append(errs, c.Integrity.setDefaultsAndValidate()...)
	errs = append(errs, c.Lifecycle.setDefaultsAndValidate()...)
	errs = append(errs, c.Cache.setDefaultsAndValidate()...)
	errs = append(errs, c.Notifications.setDefaultsAndValidate()...)
	if c.Redis != nil {
		errs = append(errs, c.Redis.setDefaultsAndValidate()...)
	}
	return errs
}

// applyDefaultsOnlyTypes handles sub-configs that have only zero-value
// defaults (no validation errors possible): circuit breakers, cleanup
// queue concurrency, reconcile interval.
func (c *Config) applyDefaultsOnlyTypes() {
	c.CircuitBreaker.setDefaults()
	c.BackendCircuitBreaker.setDefaults()
	if c.CleanupQueue.Concurrency <= 0 {
		c.CleanupQueue.Concurrency = 10
	}
	if c.Reconcile.Enabled && c.Reconcile.Interval <= 0 {
		c.Reconcile.Interval = 24 * time.Hour
	}
}

// validateRoutingStrategy applies the default and enforces the enum.
func (c *Config) validateRoutingStrategy() []error {
	if c.RoutingStrategy == "" {
		c.RoutingStrategy = RoutingPack
	}
	if c.RoutingStrategy != RoutingPack && c.RoutingStrategy != RoutingSpread {
		return []error{ErrInvalidRoutingStrategy}
	}
	return nil
}

// validateQuotaReplicationCombo enforces the cross-field invariants around
// mixing bounded and unlimited backend quotas, and emits a redundancy
// warning when multiple backends are configured without replication.
func (c *Config) validateQuotaReplicationCombo() []error {
	if len(c.Backends) <= 1 {
		return nil
	}
	var errs []error
	unlimitedCount := 0
	for i := range c.Backends {
		if c.Backends[i].QuotaBytes == 0 {
			unlimitedCount++
		}
	}
	quotaCount := len(c.Backends) - unlimitedCount

	if unlimitedCount > 0 && quotaCount > 0 {
		errs = append(errs, ErrQuotaMixNotAllowed)
	}
	if unlimitedCount > 1 && c.Replication.Factor <= 1 {
		errs = append(errs, ErrUnlimitedNeedsReplication)
	}
	if c.Replication.Factor <= 1 {
		slog.WarnContext(context.Background(), "replication.factor <= 1 with multiple backends provides no redundancy  -  losing a backend will cause permanent data loss for objects stored exclusively on it",
			"backends", len(c.Backends), "replication_factor", c.Replication.Factor)
	}
	return errs
}

// -------------------------------------------------------------------------
// HOT-RELOAD CHANGE DETECTION
// -------------------------------------------------------------------------

// NonReloadableFieldsChanged compares two configs and returns a list of
// non-reloadable field descriptions that differ. Used by the SIGHUP handler
// to warn about changes that require a restart.
func NonReloadableFieldsChanged(old, new *Config) []string {
	var changed []string
	changed = append(changed, serverFieldsChanged(&old.Server, &new.Server)...)
	changed = append(changed, topLevelFieldsChanged(old, new)...)
	changed = append(changed, redisFieldsChanged(old.Redis, new.Redis)...)
	changed = append(changed, backendStructuralChanges(old.Backends, new.Backends)...)
	return changed
}

// serverFieldsChanged enumerates non-reloadable server-config fields that
// differ between two snapshots.
func serverFieldsChanged(old, new *ServerConfig) []string {
	var changed []string
	if old.ListenAddr != new.ListenAddr {
		changed = append(changed, "server.listen_addr")
	}
	if old.MaxConcurrentRequests != new.MaxConcurrentRequests {
		changed = append(changed, "server.max_concurrent_requests")
	}
	if old.MaxConcurrentReads != new.MaxConcurrentReads {
		changed = append(changed, "server.max_concurrent_reads")
	}
	if old.MaxConcurrentWrites != new.MaxConcurrentWrites {
		changed = append(changed, "server.max_concurrent_writes")
	}
	if old.LoadShedThreshold != new.LoadShedThreshold {
		changed = append(changed, "server.load_shed_threshold")
	}
	if old.AdmissionWait != new.AdmissionWait {
		changed = append(changed, "server.admission_wait")
	}
	if old.ReadHeaderTimeout != new.ReadHeaderTimeout ||
		old.ReadTimeout != new.ReadTimeout ||
		old.WriteTimeout != new.WriteTimeout ||
		old.IdleTimeout != new.IdleTimeout {
		changed = append(changed, "server timeouts (read_header_timeout, read_timeout, write_timeout, idle_timeout)")
	}
	if old.ShutdownDelay != new.ShutdownDelay {
		changed = append(changed, "server.shutdown_delay")
	}
	if old.TLS != new.TLS {
		changed = append(changed, "server.tls")
	}
	return changed
}

// topLevelFieldsChanged enumerates non-reloadable top-level sub-configs
// that differ (database, telemetry, UI, circuit breakers, encryption,
// routing strategy).
func topLevelFieldsChanged(old, new *Config) []string {
	var changed []string
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
	return changed
}

// redisFieldsChanged handles the *RedisConfig pointer-nullable case  - 
// either presence change or struct inequality counts as a diff.
func redisFieldsChanged(old, new *RedisConfig) []string {
	oldHas := old != nil
	newHas := new != nil
	if oldHas != newHas {
		return []string{"redis"}
	}
	if oldHas && newHas && *old != *new {
		return []string{"redis"}
	}
	return nil
}

// backendStructuralChanges reports backend-list edits that cannot be hot-
// reloaded. Quota and usage limits are explicitly reloadable and handled
// by a separate code path; only endpoint/credential/routing-shape fields
// are checked here.
func backendStructuralChanges(old, new []BackendConfig) []string {
	if len(old) != len(new) {
		return []string{"backends (count changed)"}
	}
	var changed []string
	for i := range old {
		o, n := old[i], new[i]
		if o.Name != n.Name || o.Endpoint != n.Endpoint || o.Region != n.Region ||
			o.Bucket != n.Bucket || o.AccessKeyID != n.AccessKeyID ||
			o.SecretAccessKey != n.SecretAccessKey || o.ForcePathStyle != n.ForcePathStyle ||
			boolDefault(o.UnsignedPayload, true) != boolDefault(n.UnsignedPayload, true) ||
			o.DisableChecksum != n.DisableChecksum ||
			o.StripSDKHeaders != n.StripSDKHeaders {
			changed = append(changed, fmt.Sprintf("backends[%d] (%s) structural fields", i, o.Name))
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
