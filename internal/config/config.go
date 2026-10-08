// -------------------------------------------------------------------------------
// Configuration - S3 Orchestrator Settings
//
// Author: Alex Freidah
//
// Configuration types and loader for the S3 proxy. Supports environment variable
// expansion in YAML values using ${VAR} syntax. Types are split into domain
// files; this file holds the root Config struct, loader and its redacted
// reverse, cross-field validation, and hot-reload change detection.
// -------------------------------------------------------------------------------

package config

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
)

// -------------------------------------------------------------------------
// ROUTING STRATEGY
// -------------------------------------------------------------------------

// RoutingStrategy determines how write operations select a target backend.
type RoutingStrategy string

// RoutingPack and RoutingSpread are the supported routing strategies.
const (
	RoutingPack   RoutingStrategy = "pack"   // fills backends in order, first with space wins
	RoutingSpread RoutingStrategy = "spread" // distributes writes by utilization ratio
)

// -------------------------------------------------------------------------
// ROOT CONFIG
// -------------------------------------------------------------------------

// Config holds the complete service configuration.
type Config struct {
	Server                ServerConfig                `yaml:"server"`
	Buckets               []BucketConfig              `yaml:"buckets"`
	Database              DatabaseConfig              `yaml:"database"`
	Backends              []BackendConfig             `yaml:"backends"`
	Telemetry             TelemetryConfig             `yaml:"telemetry"`
	Rebalance             RebalanceConfig             `yaml:"rebalance"`
	Replication           ReplicationConfig           `yaml:"replication"`
	RateLimit             RateLimitConfig             `yaml:"rate_limit"`
	CircuitBreaker        CircuitBreakerConfig        `yaml:"circuit_breaker"`
	BackendCircuitBreaker BackendCircuitBreakerConfig `yaml:"backend_circuit_breaker"`
	Encryption            EncryptionConfig            `yaml:"encryption"`
	Compression           CompressionConfig           `yaml:"compression"`
	UI                    UIConfig                    `yaml:"ui"`
	Auth                  AuthConfig                  `yaml:"auth"`
	CleanupQueue          CleanupQueueConfig          `yaml:"cleanup_queue"`
	WritePath             WritePathConfig             `yaml:"write_path"`
	UsageFlush            UsageFlushConfig            `yaml:"usage_flush"`
	Lifecycle             LifecycleConfig             `yaml:"lifecycle"`
	Reconcile             ReconcileConfig             `yaml:"reconcile"`
	Integrity             IntegrityConfig             `yaml:"integrity"`
	Cache                 CacheConfig                 `yaml:"cache"`
	Redis                 *RedisConfig                `yaml:"redis"`
	Notifications         NotificationConfig          `yaml:"notifications"`
	Debug                 DebugConfig                 `yaml:"debug"`
	RoutingStrategy       RoutingStrategy             `yaml:"routing_strategy"` // "pack" (default) or "spread"
}

// -------------------------------------------------------------------------
// LOADER
// -------------------------------------------------------------------------

// LoadConfig reads and parses the configuration file with environment variable
// expansion. Returns an error if the file cannot be read, parsed, or validated.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, wrappedPath(ErrReadConfigFile, path, err)
	}

	expanded := os.Expand(string(data), os.Getenv)

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, wrappedPath(ErrParseConfig, path, err)
	}

	// Checked against the document rather than the parsed struct: the fields are
	// gone, and YAML drops what it cannot map, so a removed key would otherwise
	// be ignored and the deployment would run with authentication it thinks it
	// configured.
	if errs := checkRemovedKeys([]byte(expanded)); len(errs) > 0 {
		return nil, wrappedPath(ErrInvalidConfig, path, errors.Join(errs...))
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		return nil, wrappedPath(ErrInvalidConfig, path, err)
	}

	return &cfg, nil
}

// redactedValue replaces a configured secret in MarshalRedacted's output.
const redactedValue = "(redacted)"

// MarshalRedacted renders the configuration as YAML, the form LoadConfig reads,
// with defaults filled in and every secret replaced by a placeholder.
func (c *Config) MarshalRedacted() ([]byte, error) {
	return yaml.Marshal(c.redacted())
}

// redacted returns a copy of c with every secret replaced. Slices and pointed-to
// sections are copied before they are changed, so c itself is left untouched.
func (c *Config) redacted() *Config {
	r := *c
	r.Database.Password = redact(r.Database.Password)
	r.Auth.Root.SecretAccessKey = redact(r.Auth.Root.SecretAccessKey)
	r.UI.SessionSecret = redact(r.UI.SessionSecret)
	r.Encryption.MasterKey = redact(r.Encryption.MasterKey)
	r.Encryption.PreviousKeys = slices.Clone(r.Encryption.PreviousKeys)
	for i := range r.Encryption.PreviousKeys {
		r.Encryption.PreviousKeys[i] = redact(r.Encryption.PreviousKeys[i])
	}
	if r.Encryption.Vault != nil {
		vault := *r.Encryption.Vault
		vault.Token = redact(vault.Token)
		r.Encryption.Vault = &vault
	}
	if r.Redis != nil {
		redis := *r.Redis
		redis.Password = redact(redis.Password)
		r.Redis = &redis
	}
	r.Backends = slices.Clone(r.Backends)
	for i := range r.Backends {
		r.Backends[i].SecretAccessKey = redact(r.Backends[i].SecretAccessKey)
	}
	r.Buckets = slices.Clone(r.Buckets)
	for i := range r.Buckets {
		creds := slices.Clone(r.Buckets[i].Credentials)
		for j := range creds {
			creds[j].SecretAccessKey = redact(creds[j].SecretAccessKey)
		}
		r.Buckets[i].Credentials = creds
	}
	r.Notifications.Endpoints = slices.Clone(r.Notifications.Endpoints)
	for i := range r.Notifications.Endpoints {
		r.Notifications.Endpoints[i].Secret = redact(r.Notifications.Endpoints[i].Secret)
	}
	return &r
}

// redact replaces a set secret with redactedValue and leaves an unset one
// empty, so the output still shows which secrets are configured.
func redact(secret string) string {
	if secret == "" {
		return ""
	}
	return redactedValue
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
	errs = append(errs, c.validateParallelCopies()...)
	return errors.Join(errs...)
}

// CopiesPerWrite reports how many copies a single-object PUT places itself:
// 1 unless fan-out is on, and always 1 at replication factor 1.
func (c *Config) CopiesPerWrite() int {
	pc := c.WritePath.ParallelCopies
	if !pc.Enabled || c.Replication.Factor <= 1 {
		return 1
	}
	return min(pc.Count, c.Replication.Factor)
}

// validateParallelCopies settles how many copies a write places itself. The
// count defaults to the replication factor and may never exceed it, since the
// over-replication cleaner deletes anything past the factor about as fast as
// writes could create it. It must run after the replication section applies
// the factor's default.
func (c *Config) validateParallelCopies() []error {
	pc := &c.WritePath.ParallelCopies
	pc.Count = cmp.Or(pc.Count, c.Replication.Factor)
	pc.MaxInFlight = cmp.Or(pc.MaxInFlight, c.writeAdmissionCapacity())
	if !pc.Enabled {
		return nil
	}
	var errs []error
	if pc.Count < 1 {
		errs = append(errs, ErrParallelCopiesMin)
	}
	if pc.Count > c.Replication.Factor {
		errs = append(errs, ErrParallelCopiesOverFactor)
	}
	if pc.MaxInFlight < 1 {
		errs = append(errs, ErrParallelCopiesInFlightMin)
	}
	return errs
}

// writeAdmissionCapacity is how many writes this instance admits at once: the
// write limit, else the shared request limit, else a floor. The in-flight tail
// ceiling defaults to it.
func (c *Config) writeAdmissionCapacity() int {
	return cmp.Or(c.Server.MaxConcurrentWrites, c.Server.MaxConcurrentRequests, DefaultDetachedUploadCeiling)
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
	errs = append(errs, c.Compression.setDefaultsAndValidate()...)
	errs = append(errs, c.UI.setDefaultsAndValidate()...)
	// The dashboard has no login without a root credential, so enabling it
	// requires one.
	errs = append(errs, c.Auth.setDefaultsAndValidate(c.UI.Enabled)...)
	errs = append(errs, c.UsageFlush.setDefaultsAndValidate()...)
	errs = append(errs, validateLifecycleRules(c.Lifecycle.Rules)...)
	errs = append(errs, c.Integrity.setDefaultsAndValidate()...)
	errs = append(errs, c.Lifecycle.setDefaultsAndValidate()...)
	errs = append(errs, c.Cache.setDefaultsAndValidate()...)
	errs = append(errs, c.Notifications.setDefaultsAndValidate()...)
	errs = append(errs, c.Debug.FlightRecorder.setDefaultsAndValidate()...)
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
	if c.CleanupQueue.ClaimGracePeriod <= 0 {
		c.CleanupQueue.ClaimGracePeriod = 5 * time.Minute
	}
	if c.Reconcile.Enabled && c.Reconcile.Interval <= 0 {
		c.Reconcile.Interval = 24 * time.Hour
	}
}

// validateRoutingStrategy applies the default and enforces the enum.
func (c *Config) validateRoutingStrategy() []error {
	c.RoutingStrategy = cmp.Or(c.RoutingStrategy, RoutingPack)
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
		slog.WarnContext(context.Background(),
			"replication.factor <= 1 with multiple backends - losing a backend will cause permanent data loss for objects stored exclusively on it",
			logfmt.Component("config"),
			"backends", len(c.Backends),
			"replication_factor", c.Replication.Factor,
		)
	}
	return errs
}

// -------------------------------------------------------------------------
// HOT-RELOAD CHANGE DETECTION
// -------------------------------------------------------------------------

// NonReloadableFieldsChanged compares two configs and returns a list of
// non-reloadable field descriptions that differ. Used by the SIGHUP handler
// to warn about changes that require a restart.
func NonReloadableFieldsChanged(old, next *Config) []string {
	var changed []string
	changed = append(changed, serverFieldsChanged(&old.Server, &next.Server)...)
	changed = append(changed, topLevelFieldsChanged(old, next)...)
	changed = append(changed, redisFieldsChanged(old.Redis, next.Redis)...)
	changed = append(changed, backendStructuralChanges(old.Backends, next.Backends)...)
	return changed
}

// serverFieldsChanged enumerates non-reloadable server-config fields that
// differ between two snapshots.
func serverFieldsChanged(old, next *ServerConfig) []string {
	var changed []string
	if old.ListenAddr != next.ListenAddr {
		changed = append(changed, "server.listen_addr")
	}
	if old.MaxConcurrentRequests != next.MaxConcurrentRequests {
		changed = append(changed, "server.max_concurrent_requests")
	}
	if old.MaxConcurrentReads != next.MaxConcurrentReads {
		changed = append(changed, "server.max_concurrent_reads")
	}
	if old.MaxConcurrentWrites != next.MaxConcurrentWrites {
		changed = append(changed, "server.max_concurrent_writes")
	}
	if old.MaxHeaderBytes != next.MaxHeaderBytes {
		changed = append(changed, "server.max_header_bytes")
	}
	if old.MaxHeaderValueCount != next.MaxHeaderValueCount {
		changed = append(changed, "server.max_header_value_count")
	}
	if old.LoadShedThreshold != next.LoadShedThreshold {
		changed = append(changed, "server.load_shed_threshold")
	}
	if old.AdmissionWait != next.AdmissionWait {
		changed = append(changed, "server.admission_wait")
	}
	if old.ReadHeaderTimeout != next.ReadHeaderTimeout ||
		old.ReadTimeout != next.ReadTimeout ||
		old.WriteTimeout != next.WriteTimeout ||
		old.IdleTimeout != next.IdleTimeout {
		changed = append(changed, "server timeouts (read_header_timeout, read_timeout, write_timeout, idle_timeout)")
	}
	if old.ShutdownDelay != next.ShutdownDelay {
		changed = append(changed, "server.shutdown_delay")
	}
	if old.TLS != next.TLS {
		changed = append(changed, "server.tls")
	}
	return changed
}

// topLevelFieldsChanged enumerates non-reloadable top-level sub-configs
// that differ (database, telemetry, UI, circuit breakers, encryption,
// routing strategy).
func topLevelFieldsChanged(old, next *Config) []string {
	var changed []string
	if old.Database != next.Database {
		changed = append(changed, "database")
	}
	if old.Telemetry != next.Telemetry {
		changed = append(changed, "telemetry")
	}
	if old.UI != next.UI {
		changed = append(changed, "ui")
	}
	if circuitBreakerChanged(old.CircuitBreaker, next.CircuitBreaker) {
		changed = append(changed, "circuit_breaker")
	}
	if old.BackendCircuitBreaker != next.BackendCircuitBreaker {
		changed = append(changed, "backend_circuit_breaker")
	}
	if old.Encryption.Enabled != next.Encryption.Enabled ||
		old.Encryption.MasterKey != next.Encryption.MasterKey ||
		old.Encryption.MasterKeyFile != next.Encryption.MasterKeyFile ||
		old.Encryption.ChunkSize != next.Encryption.ChunkSize {
		changed = append(changed, "encryption")
	}
	// The codec is built once at startup from level and chunk_size, and
	// enabling compression mid-flight would leave the write path without one,
	// so the whole block follows encryption in requiring a restart.
	if old.Compression != next.Compression {
		changed = append(changed, "compression")
	}
	if old.RoutingStrategy != next.RoutingStrategy {
		changed = append(changed, "routing_strategy")
	}
	return changed
}

// circuitBreakerChanged is field-by-field because *bool DegradedReadsEnabled defeats ==.
func circuitBreakerChanged(old, next CircuitBreakerConfig) bool {
	if old.FailureThreshold != next.FailureThreshold ||
		old.OpenTimeout != next.OpenTimeout ||
		old.CacheTTL != next.CacheTTL ||
		old.ParallelBroadcast != next.ParallelBroadcast ||
		old.DegradedBroadcastParallelism != next.DegradedBroadcastParallelism {
		return true
	}
	return derefBoolDefault(old.DegradedReadsEnabled, true) != derefBoolDefault(next.DegradedReadsEnabled, true)
}

func derefBoolDefault(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// redisFieldsChanged handles the *RedisConfig pointer-nullable case  -
// either presence change or struct inequality counts as a diff.
func redisFieldsChanged(old, next *RedisConfig) []string {
	oldHas := old != nil
	nextHas := next != nil
	if oldHas != nextHas {
		return []string{"redis"}
	}
	if oldHas && nextHas && *old != *next {
		return []string{"redis"}
	}
	return nil
}

// backendStructuralChanges reports backend-list edits that cannot be hot-
// reloaded. Quota and usage limits are explicitly reloadable and handled
// by a separate code path; only endpoint/credential/routing-shape fields
// are checked here.
func backendStructuralChanges(old, next []BackendConfig) []string {
	if len(old) != len(next) {
		return []string{"backends (count changed)"}
	}
	var changed []string
	for i := range old {
		o, n := old[i], next[i]
		if o.Name != n.Name || o.Endpoint != n.Endpoint || o.Region != n.Region ||
			o.Bucket != n.Bucket || o.AccessKeyID != n.AccessKeyID ||
			o.SecretAccessKey != n.SecretAccessKey || o.ForcePathStyle != n.ForcePathStyle ||
			boolDefault(o.UnsignedPayload, true) != boolDefault(n.UnsignedPayload, true) ||
			o.DisableChecksum != n.DisableChecksum ||
			o.StripSDKHeaders != n.StripSDKHeaders ||
			httpTransportChanged(o.HTTP, n.HTTP) {
			changed = append(changed, fmt.Sprintf("backends[%d] (%s) structural fields", i, o.Name))
		}
	}
	return changed
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// httpTransportChanged reports whether a backend's transport settings differ.
// The transport is built once when the backend client is constructed, so a
// changed pool size or HTTP/2 setting needs a restart to take effect - which
// the reload path reports rather than silently ignoring.
func httpTransportChanged(o, n BackendHTTPConfig) bool {
	return o.MaxIdleConns != n.MaxIdleConns ||
		o.MaxIdleConnsPerHost != n.MaxIdleConnsPerHost ||
		o.MaxConnsPerHost != n.MaxConnsPerHost ||
		o.ResponseHeaderTimeout != n.ResponseHeaderTimeout ||
		o.HTTP2Enabled() != n.HTTP2Enabled()
}

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
