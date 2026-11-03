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
	"reflect"
	"regexp"
	"slices"
	"strings"

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
	errs = append(errs, c.validateRoutingStrategy()...)
	errs = append(errs, c.validateQuotaReplicationCombo()...)
	errs = append(errs, c.validateParallelCopies()...)
	return errors.Join(errs...)
}

// HasBackend reports whether the config declares a backend named name.
func (c *Config) HasBackend(name string) bool {
	return slices.ContainsFunc(c.Backends, func(b BackendConfig) bool { return b.Name == name })
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
	pc.MaxInFlight = cmp.Or(pc.MaxInFlight, c.Server.Admission().WriteCapacity())
	if !pc.Enabled {
		return nil
	}
	errs := failed(
		positive("write_path.parallel_copies.count", pc.Count),
		positive("write_path.parallel_copies.max_in_flight", pc.MaxInFlight),
	)
	if pc.Count > c.Replication.Factor {
		errs = append(errs, ErrParallelCopiesOverFactor)
	}
	return errs
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
	errs = append(errs, c.Lifecycle.setDefaultsAndValidate()...)
	errs = append(errs, c.Integrity.setDefaultsAndValidate()...)
	errs = append(errs, c.Cache.setDefaultsAndValidate()...)
	errs = append(errs, c.Notifications.setDefaultsAndValidate()...)
	errs = append(errs, c.CircuitBreaker.setDefaultsAndValidate()...)
	errs = append(errs, c.BackendCircuitBreaker.setDefaultsAndValidate()...)
	errs = append(errs, c.Debug.FlightRecorder.setDefaultsAndValidate()...)
	errs = append(errs, c.CleanupQueue.setDefaultsAndValidate()...)
	errs = append(errs, c.Reconcile.setDefaultsAndValidate()...)
	errs = append(errs, c.WritePath.PendingPattern.setDefaultsAndValidate()...)
	c.WritePath.Multipart.EnforceMinPartSize = new(c.WritePath.Multipart.IsMinPartSizeEnforced())
	if c.Redis != nil {
		errs = append(errs, c.Redis.setDefaultsAndValidate()...)
	}
	return errs
}

// validateRoutingStrategy applies the default and enforces the enum.
func (c *Config) validateRoutingStrategy() []error {
	c.RoutingStrategy = cmp.Or(c.RoutingStrategy, RoutingPack)
	if !c.RoutingStrategy.Valid() {
		return []error{fmt.Errorf("routing_%w", ErrInvalidRoutingStrategy)}
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

// reloadablePaths are the config paths a reload applies to the running
// process, through config.Live or a reload hook. A path covers everything
// below it, and [*] matches any index. Every other path is read once at
// startup, so a change to it waits for a restart.
var reloadablePaths = []string{
	"server.log_level",
	"server.shutdown_delay",
	"buckets",
	"auth",
	"rate_limit.requests_per_sec",
	"rate_limit.burst",
	"backends[*].quota_bytes",
	"backends[*].api_request_limit",
	"backends[*].egress_byte_limit",
	"backends[*].ingress_byte_limit",
	"backends[*].request_limits",
	"backends[*].unmetered",
	"backends[*].usage_reset_day",
	"backends[*].usage_reset_timezone",
	"rebalance.enabled",
	"rebalance.strategy",
	"rebalance.batch_size",
	"rebalance.threshold",
	"rebalance.concurrency",
	"replication.factor",
	"replication.batch_size",
	"replication.concurrency",
	"replication.unhealthy_threshold",
	"integrity.enabled",
	"integrity.verify_on_read",
	"integrity.verify_on_replicate",
	"integrity.scrubber_batch_size",
	"integrity.scrubber_min_age",
	"integrity.scrubber_egress_reserve",
	"lifecycle",
	"usage_flush",
}

// indexPattern matches a list index in a config path.
var indexPattern = regexp.MustCompile(`\[\d+\]`)

// NonReloadableFieldsChanged returns the paths, in field order, of the config
// values that differ between two validated configs and that a reload cannot
// apply, so the operator knows what still waits for a restart. Values are never
// reported, so no secret reaches a log.
func NonReloadableFieldsChanged(old, next *Config) []string {
	var changed []string
	diffPaths("", reflect.ValueOf(*old), reflect.ValueOf(*next), &changed)
	return changed
}

// diffPaths appends the path of every value under a and b that differs, outside
// the reloadable paths. A list of different length, or a section present in
// one config only, is reported whole.
func diffPaths(path string, a, b reflect.Value, out *[]string) {
	if reloadable(path) {
		return
	}
	switch a.Kind() {
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			reportIf(a.IsNil() != b.IsNil(), path, out)
			return
		}
		diffPaths(path, a.Elem(), b.Elem(), out)
	case reflect.Struct:
		diffFields(path, a, b, out)
	case reflect.Slice:
		if a.Len() != b.Len() || a.Type().Elem().Kind() != reflect.Struct {
			reportIf(!reflect.DeepEqual(a.Interface(), b.Interface()), path, out)
			return
		}
		for i := range a.Len() {
			diffPaths(fmt.Sprintf("%s[%d]", path, i), a.Index(i), b.Index(i), out)
		}
	default:
		reportIf(!reflect.DeepEqual(a.Interface(), b.Interface()), path, out)
	}
}

// diffFields walks a struct's fields under their YAML names, skipping the ones
// the file never holds.
func diffFields(path string, a, b reflect.Value, out *[]string) {
	for i := range a.NumField() {
		name, _, _ := strings.Cut(a.Type().Field(i).Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		diffPaths(strings.TrimPrefix(path+"."+name, "."), a.Field(i), b.Field(i), out)
	}
}

// reportIf appends path when changed holds.
func reportIf(changed bool, path string, out *[]string) {
	if changed {
		*out = append(*out, path)
	}
}

// reloadable reports whether path lies at or under a reloadable path.
func reloadable(path string) bool {
	general := indexPattern.ReplaceAllString(path, "[*]")
	for _, p := range reloadablePaths {
		if general == p || strings.HasPrefix(general, p+".") || strings.HasPrefix(general, p+"[") {
			return true
		}
	}
	return false
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// ParseLogLevel maps a log level name, in any case, to a slog.Level. An empty
// name is info, so a log query that names no level withholds DEBUG entries;
// any other unknown name is an ErrInvalidLogLevel.
func ParseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("%w: %q (want one of debug, info, warn, error)", ErrInvalidLogLevel, s)
	}
}
