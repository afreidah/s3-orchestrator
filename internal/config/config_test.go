// -------------------------------------------------------------------------------
// Configuration Tests - Validation and Defaults
//
// Author: Alex Freidah
//
// Unit tests for configuration validation, default value application, duplicate
// backend detection, bucket validation, and PostgreSQL connection string generation.
// -------------------------------------------------------------------------------

package config

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestConfigValidation_MinimalValid verifies the config validation minimal valid contract.
// Asserts that valid config should pass validation:.
func TestConfigValidation_MinimalValid(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Server: ServerConfig{
			ListenAddr: "0.0.0.0:9000",
		},
		Buckets: []BucketConfig{
			{Name: "unified", Credentials: []CredentialConfig{
				{AccessKeyID: "AKID", SecretAccessKey: "secret"},
			}},
		},
		Database: DatabaseConfig{
			Host:     "localhost",
			Database: "s3proxy",
			User:     "s3proxy",
		},
		Backends: []BackendConfig{
			{
				Name:            "test",
				Endpoint:        "https://s3.example.com",
				Bucket:          "mybucket",
				AccessKeyID:     "AKID",
				SecretAccessKey: "secret",
				QuotaBytes:      1024,
			},
		},
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("valid config should pass validation: %v", err)
	}

	// Check defaults were set
	if cfg.Database.Port != 5432 {
		t.Errorf("database port default = %d, want 5432", cfg.Database.Port)
	}
	if cfg.Database.SSLMode != "require" {
		t.Errorf("database ssl_mode default = %q, want 'require'", cfg.Database.SSLMode)
	}
}

// TestConfigValidation_MissingRequired verifies the config validation missing required path by exercising cfg.SetDefaultsAndValidate.
func TestConfigValidation_MissingRequired(t *testing.T) {
	t.Parallel()
	cfg := Config{}
	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("empty config should fail validation")
	}
}

// TestConfigValidation_FieldRules covers the per-field rules
// SetDefaultsAndValidate enforces. Every case starts from a config that
// validates, changes one thing, and says whether the result is still allowed -
// so the rule under test is the line of the table, not a function of its own.
func TestConfigValidation_FieldRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"duplicate backend names", func(c *Config) {
			dup := BackendConfig{Name: "dup", Endpoint: "e", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s", QuotaBytes: 1}
			c.Backends = []BackendConfig{dup, dup}
		}, true},
		{"negative quota", func(c *Config) { c.Backends[0].QuotaBytes = -1 }, true},
		{"negative max_concurrent_requests", func(c *Config) { c.Server.MaxConcurrentRequests = -1 }, true},
		{"negative max_concurrent_reads", func(c *Config) { c.Server.MaxConcurrentReads = -1 }, true},
		{"negative max_concurrent_writes", func(c *Config) { c.Server.MaxConcurrentWrites = -1 }, true},
		{"load_shed_threshold at or above 1.0", func(c *Config) { c.Server.LoadShedThreshold = 1.5 }, true},
		{"negative load_shed_threshold", func(c *Config) { c.Server.LoadShedThreshold = -0.5 }, true},
		{"load_shed_threshold in range", func(c *Config) { c.Server.LoadShedThreshold = 0.8 }, false},
		{"negative admission_wait", func(c *Config) { c.Server.AdmissionWait = -1 * time.Second }, true},
		{"negative api_request_limit", func(c *Config) { c.Backends[0].APIRequestLimit = -1 }, true},
		{"negative egress_byte_limit", func(c *Config) { c.Backends[0].EgressByteLimit = -1 }, true},
		{"negative ingress_byte_limit", func(c *Config) { c.Backends[0].IngressByteLimit = -1 }, true},
		{"zero usage limits mean unlimited", func(c *Config) {
			c.Backends[0].APIRequestLimit = 0
			c.Backends[0].EgressByteLimit = 0
			c.Backends[0].IngressByteLimit = 0
		}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := validBaseConfig()
			tt.mutate(&cfg)

			err := cfg.SetDefaultsAndValidate()
			if tt.wantErr && err == nil {
				t.Error("expected validation to fail")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("expected validation to pass, got %v", err)
			}
		})
	}
}

// TestConfigValidation_ZeroMaxConcurrentRequestsDefaultsTo1000 verifies the config validation zero max concurrent requests defaults to1000 contract.
// Asserts that zero max_concurrent_requests should pass validation:.
func TestConfigValidation_ZeroMaxConcurrentRequestsDefaultsTo1000(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Server.MaxConcurrentRequests = 0

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("zero max_concurrent_requests should pass validation: %v", err)
	}
	if cfg.Server.MaxConcurrentRequests != 1000 {
		t.Errorf("expected default 1000, got %d", cfg.Server.MaxConcurrentRequests)
	}
}

// TestConfigValidation_SplitPoolsSkipGlobalDefault verifies the config validation split pools skip global default contract.
// Asserts that split pools should pass validation:.
func TestConfigValidation_SplitPoolsSkipGlobalDefault(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Server.MaxConcurrentReads = 200
	cfg.Server.MaxConcurrentWrites = 200

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("split pools should pass validation: %v", err)
	}
	if cfg.Server.MaxConcurrentRequests != 0 {
		t.Errorf("global limit should stay 0 when split pools are set, got %d", cfg.Server.MaxConcurrentRequests)
	}
}

// TestConfigValidation_ZeroQuotaMeansUnlimited verifies the config validation zero quota means unlimited contract.
// Asserts that zero quota (unlimited) should pass validation:.
func TestConfigValidation_ZeroQuotaMeansUnlimited(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Backends[0].QuotaBytes = 0

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("zero quota (unlimited) should pass validation: %v", err)
	}
}

// TestConfigValidation_OmittedQuotaMeansUnlimited verifies the config validation omitted quota means unlimited contract.
// Asserts that omitted quota (unlimited) should pass validation:.
func TestConfigValidation_OmittedQuotaMeansUnlimited(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Backends[0].QuotaBytes = 0

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("omitted quota (unlimited) should pass validation: %v", err)
	}
}

// TestConnectionString verifies the connection string contract.
// Asserts that ConnectionString() = , want.
func TestConnectionString(t *testing.T) {
	t.Parallel()
	db := DatabaseConfig{
		Host:     "localhost",
		Port:     5433,
		Database: "s3proxy",
		User:     "s3proxy",
		Password: "secret",
		SSLMode:  "require",
	}

	got := db.ConnectionString()
	want := "postgres://s3proxy:secret@localhost:5433/s3proxy?sslmode=require" //nolint:gosec // G101: test credential string
	if got != want {
		t.Errorf("ConnectionString() = %q, want %q", got, want)
	}
}

// TestConnectionString_SpecialChars verifies the connection string special chars contract.
// Asserts that ConnectionString() = , want.
func TestConnectionString_SpecialChars(t *testing.T) {
	t.Parallel()
	db := DatabaseConfig{ //nolint:gosec // G101: test config values
		Host:     "db.example.com",
		Port:     5432,
		Database: "mydb",
		User:     "admin",
		Password: "p@ss=w ord&special",
		SSLMode:  "disable",
	}

	got := db.ConnectionString()
	// url.UserPassword percent-encodes @ but preserves = and &
	want := "postgres://admin:p%40ss=w%20ord&special@db.example.com:5432/mydb?sslmode=disable" //nolint:gosec // G101: test credential string
	if got != want {
		t.Errorf("ConnectionString() = %q, want %q", got, want)
	}
}

// TestRebalanceConfig_Defaults verifies the rebalance config defaults contract.
// Asserts that valid rebalance config should pass:.
func TestRebalanceConfig_Defaults(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Rebalance = RebalanceConfig{Enabled: true}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("valid rebalance config should pass: %v", err)
	}

	if cfg.Rebalance.Strategy != "pack" {
		t.Errorf("strategy default = %q, want %q", cfg.Rebalance.Strategy, "pack")
	}
	if cfg.Rebalance.Interval != 6*time.Hour {
		t.Errorf("interval default = %v, want %v", cfg.Rebalance.Interval, 6*time.Hour)
	}
	if cfg.Rebalance.BatchSize != 100 {
		t.Errorf("batch_size default = %d, want 100", cfg.Rebalance.BatchSize)
	}
	if cfg.Rebalance.Threshold != 0.1 {
		t.Errorf("threshold default = %f, want 0.1", cfg.Rebalance.Threshold)
	}
	if cfg.Rebalance.Concurrency != 5 {
		t.Errorf("concurrency default = %d, want 5", cfg.Rebalance.Concurrency)
	}
}

// TestRebalanceConfig_InvalidStrategy verifies the rebalance config invalid strategy path by exercising cfg.SetDefaultsAndValidate.
func TestRebalanceConfig_InvalidStrategy(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Rebalance = RebalanceConfig{
		Enabled:   true,
		Strategy:  "invalid",
		Interval:  time.Hour,
		BatchSize: 10,
		Threshold: 0.1,
	}

	if err := cfg.SetDefaultsAndValidate(); err == nil {
		t.Error("invalid strategy should fail validation")
	}
}

// TestRebalanceConfig_DisabledSkipsValidation verifies the rebalance config disabled skips validation contract.
// Asserts that disabled rebalance should skip validation:.
func TestRebalanceConfig_DisabledSkipsValidation(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Rebalance = RebalanceConfig{
		Enabled:  false,
		Strategy: "garbage",
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("disabled rebalance should skip validation: %v", err)
	}
}

// TestRebalanceConfig_InvalidThreshold verifies the rebalance config invalid threshold path by exercising cfg.SetDefaultsAndValidate.
func TestRebalanceConfig_InvalidThreshold(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Rebalance = RebalanceConfig{
		Enabled:   true,
		Strategy:  "spread",
		Interval:  time.Hour,
		BatchSize: 10,
		Threshold: 1.5,
	}

	if err := cfg.SetDefaultsAndValidate(); err == nil {
		t.Error("threshold > 1 should fail validation")
	}
}

// TestReplicationConfig_DefaultsWhenDisabled verifies the replication config defaults when disabled contract.
// Asserts that disabled replication should pass:.
func TestReplicationConfig_DefaultsWhenDisabled(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	// factor=0 should default to 1 (disabled)

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("disabled replication should pass: %v", err)
	}

	if cfg.Replication.Factor != 1 {
		t.Errorf("factor default = %d, want 1", cfg.Replication.Factor)
	}
}

// TestReplicationConfig_DefaultsWhenEnabled verifies the replication config defaults when enabled contract.
// Asserts that valid replication config should pass:.
func TestReplicationConfig_DefaultsWhenEnabled(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfigTwoBackends()
	cfg.Replication = ReplicationConfig{Factor: 2}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("valid replication config should pass: %v", err)
	}

	if cfg.Replication.WorkerInterval != 5*time.Minute {
		t.Errorf("worker_interval default = %v, want %v", cfg.Replication.WorkerInterval, 5*time.Minute)
	}
	if cfg.Replication.BatchSize != 50 {
		t.Errorf("batch_size default = %d, want 50", cfg.Replication.BatchSize)
	}
}

// TestReplicationConfig_FactorExceedsBackends verifies the replication config factor exceeds backends path by exercising cfg.SetDefaultsAndValidate.
func TestReplicationConfig_FactorExceedsBackends(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig() // 1 backend
	cfg.Replication = ReplicationConfig{Factor: 2}

	if err := cfg.SetDefaultsAndValidate(); err == nil {
		t.Error("factor > backends should fail validation")
	}
}

// TestReplicationConfig_FactorNegative verifies the replication config factor negative path by exercising cfg.SetDefaultsAndValidate.
func TestReplicationConfig_FactorNegative(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Replication = ReplicationConfig{Factor: -1}

	if err := cfg.SetDefaultsAndValidate(); err == nil {
		t.Error("negative factor should fail validation")
	}
}

// TestReplicationConfig_DisabledSkipsValidation verifies the replication config disabled skips validation contract.
// Asserts that factor=1 should skip interval validation:.
func TestReplicationConfig_DisabledSkipsValidation(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Replication = ReplicationConfig{Factor: 1, WorkerInterval: -1}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("factor=1 should skip interval validation: %v", err)
	}
}

// TestCircuitBreakerDefaults verifies the circuit breaker defaults contract.
// Asserts that valid config should pass:.
func TestCircuitBreakerDefaults(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("valid config should pass: %v", err)
	}

	if cfg.CircuitBreaker.FailureThreshold != 3 {
		t.Errorf("failure_threshold default = %d, want 3", cfg.CircuitBreaker.FailureThreshold)
	}
	if cfg.CircuitBreaker.OpenTimeout != 15*time.Second {
		t.Errorf("open_timeout default = %v, want 15s", cfg.CircuitBreaker.OpenTimeout)
	}
	if cfg.CircuitBreaker.CacheTTL != 60*time.Second {
		t.Errorf("cache_ttl default = %v, want 60s", cfg.CircuitBreaker.CacheTTL)
	}
	if cfg.CircuitBreaker.ParallelBroadcast {
		t.Error("parallel_broadcast default should be false")
	}
}

// TestCircuitBreakerConfig_ParallelBroadcastSet verifies the circuit breaker config parallel broadcast set contract.
// Asserts that parallel_broadcast=true should pass:.
func TestCircuitBreakerConfig_ParallelBroadcastSet(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.CircuitBreaker.ParallelBroadcast = true

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("parallel_broadcast=true should pass: %v", err)
	}
	if !cfg.CircuitBreaker.ParallelBroadcast {
		t.Error("parallel_broadcast should be true when set")
	}
}

// TestServerTimeoutDefaults verifies the server timeout defaults contract.
// Asserts that valid config should pass:.
func TestServerTimeoutDefaults(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("valid config should pass: %v", err)
	}

	if cfg.Server.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("read_header_timeout default = %v, want 10s", cfg.Server.ReadHeaderTimeout)
	}
	if cfg.Server.ReadTimeout != 5*time.Minute {
		t.Errorf("read_timeout default = %v, want 5m", cfg.Server.ReadTimeout)
	}
	if cfg.Server.WriteTimeout != 5*time.Minute {
		t.Errorf("write_timeout default = %v, want 5m", cfg.Server.WriteTimeout)
	}
	if cfg.Server.IdleTimeout != 120*time.Second {
		t.Errorf("idle_timeout default = %v, want 120s", cfg.Server.IdleTimeout)
	}
}

// TestServerTimeoutCustomValues verifies the server timeout custom values contract.
// Asserts that custom timeouts should pass:.
func TestServerTimeoutCustomValues(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Server.ReadHeaderTimeout = 5 * time.Second
	cfg.Server.ReadTimeout = 2 * time.Minute
	cfg.Server.WriteTimeout = 3 * time.Minute
	cfg.Server.IdleTimeout = 60 * time.Second

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("custom timeouts should pass: %v", err)
	}

	if cfg.Server.ReadHeaderTimeout != 5*time.Second {
		t.Errorf("read_header_timeout = %v, want 5s", cfg.Server.ReadHeaderTimeout)
	}
	if cfg.Server.ReadTimeout != 2*time.Minute {
		t.Errorf("read_timeout = %v, want 2m", cfg.Server.ReadTimeout)
	}
	if cfg.Server.WriteTimeout != 3*time.Minute {
		t.Errorf("write_timeout = %v, want 3m", cfg.Server.WriteTimeout)
	}
	if cfg.Server.IdleTimeout != 60*time.Second {
		t.Errorf("idle_timeout = %v, want 60s", cfg.Server.IdleTimeout)
	}
}

// serverTimeoutCase parameterises one cross-validation case for the
// server.* timeout sanity checks.
type serverTimeoutCase struct {
	name              string
	readHeaderTimeout time.Duration
	readTimeout       time.Duration
	backendTimeout    time.Duration
	writeTimeout      time.Duration
	wantErr           string
}

// TestServerTimeoutCrossValidation drives each cross-validation rule for
// server.* timeouts through SetDefaultsAndValidate and asserts that
// invalid combinations surface the expected error.
func TestServerTimeoutCrossValidation(t *testing.T) {
	t.Parallel()
	tests := []serverTimeoutCase{
		{
			name:              "read_header_timeout exceeds read_timeout",
			readHeaderTimeout: 10 * time.Minute,
			readTimeout:       1 * time.Minute,
			wantErr:           "read_header_timeout must not exceed server.read_timeout",
		},
		{
			name:           "backend_timeout exceeds write_timeout",
			backendTimeout: 10 * time.Minute,
			writeTimeout:   1 * time.Minute,
			wantErr:        "backend_timeout must not exceed server.write_timeout",
		},
		{
			name:              "equal values are valid",
			readHeaderTimeout: 5 * time.Minute,
			readTimeout:       5 * time.Minute,
			backendTimeout:    5 * time.Minute,
			writeTimeout:      5 * time.Minute,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { runServerTimeoutCase(t, tt) })
	}
}

// runServerTimeoutCase applies one serverTimeoutCase to a base config
// and asserts SetDefaultsAndValidate returns the expected outcome.
func runServerTimeoutCase(t *testing.T, tt serverTimeoutCase) {
	cfg := validBaseConfig()
	applyServerTimeoutOverrides(&cfg, tt)
	err := cfg.SetDefaultsAndValidate()
	assertServerTimeoutResult(t, err, tt.wantErr)
}

// applyServerTimeoutOverrides applies the non-zero fields of tt to the
// cfg.Server timeouts so each case starts from a known-good baseline.
func applyServerTimeoutOverrides(cfg *Config, tt serverTimeoutCase) {
	if tt.readHeaderTimeout != 0 {
		cfg.Server.ReadHeaderTimeout = tt.readHeaderTimeout
	}
	if tt.readTimeout != 0 {
		cfg.Server.ReadTimeout = tt.readTimeout
	}
	if tt.backendTimeout != 0 {
		cfg.Server.BackendTimeout = tt.backendTimeout
	}
	if tt.writeTimeout != 0 {
		cfg.Server.WriteTimeout = tt.writeTimeout
	}
}

// assertServerTimeoutResult fails the test when the validation outcome
// does not match the case's wantErr (substring match for failures).
func assertServerTimeoutResult(t *testing.T, err error, wantErr string) {
	t.Helper()
	if wantErr == "" {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), wantErr) {
		t.Errorf("error = %q, want substring %q", err.Error(), wantErr)
	}
}

// TestNonReloadableFieldsChanged_ServerTimeouts verifies the non reloadable fields changed server timeouts contract.
// Asserts that expected server timeouts in changed list, got.
func TestNonReloadableFieldsChanged_ServerTimeouts(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	b.Server.ReadHeaderTimeout = 20 * time.Second
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	if len(changed) == 0 {
		t.Fatal("expected non-reloadable change for server timeouts")
	}
	found := false
	for _, c := range changed {
		if c == "server timeouts (read_header_timeout, read_timeout, write_timeout, idle_timeout)" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected server timeouts in changed list, got %v", changed)
	}
}

// TestShutdownDelayDefault verifies the shutdown delay default contract.
// Asserts that valid config should pass:.
func TestShutdownDelayDefault(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("valid config should pass: %v", err)
	}

	if cfg.Server.ShutdownDelay != 0 {
		t.Errorf("shutdown_delay default = %v, want 0", cfg.Server.ShutdownDelay)
	}
}

// TestShutdownDelayCustomValue verifies the shutdown delay custom value contract.
// Asserts that custom shutdown_delay should pass:.
func TestShutdownDelayCustomValue(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Server.ShutdownDelay = 5 * time.Second

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("custom shutdown_delay should pass: %v", err)
	}

	if cfg.Server.ShutdownDelay != 5*time.Second {
		t.Errorf("shutdown_delay = %v, want 5s", cfg.Server.ShutdownDelay)
	}
}

// TestNonReloadableFieldsChanged_ShutdownDelay verifies the non reloadable fields changed shutdown delay contract.
// Asserts that expected server.shutdown_delay in changed list, got.
func TestNonReloadableFieldsChanged_ShutdownDelay(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	b.Server.ShutdownDelay = 5 * time.Second
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	if len(changed) == 0 {
		t.Fatal("expected non-reloadable change for shutdown_delay")
	}
	found := false
	for _, c := range changed {
		if c == "server.shutdown_delay" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected server.shutdown_delay in changed list, got %v", changed)
	}
}

// TestNonReloadableFieldsChanged_CircuitBreaker verifies the non reloadable fields changed circuit breaker contract.
// Asserts that expected circuit_breaker in changed fields, got.
func TestNonReloadableFieldsChanged_CircuitBreaker(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	b.CircuitBreaker.ParallelBroadcast = true
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	found := false
	for _, c := range changed {
		if c == "circuit_breaker" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected circuit_breaker in changed fields, got %v", changed)
	}
}

// TestConfigValidation_MixedQuotaAndUnlimited verifies the config validation mixed quota and unlimited path by exercising cfg.SetDefaultsAndValidate.
func TestConfigValidation_MixedQuotaAndUnlimited(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Backends = []BackendConfig{
		{Name: "quota", Endpoint: "e", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s", QuotaBytes: 1024},
		{Name: "unlimited", Endpoint: "e", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s", QuotaBytes: 0},
	}
	cfg.Replication = ReplicationConfig{Factor: 2}

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("mixing quota'd and unlimited backends should fail validation")
	}
}

// TestConfigValidation_MultipleUnlimitedWithoutReplication verifies the config validation multiple unlimited without replication path by exercising cfg.SetDefaultsAndValidate.
func TestConfigValidation_MultipleUnlimitedWithoutReplication(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Backends = []BackendConfig{
		{Name: "u1", Endpoint: "e", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s", QuotaBytes: 0},
		{Name: "u2", Endpoint: "e", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s", QuotaBytes: 0},
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("multiple unlimited backends without replication should fail validation")
	}
}

// TestConfigValidation_MultipleUnlimitedWithReplication verifies the config validation multiple unlimited with replication contract.
// Asserts that multiple unlimited backends with replication should pass:.
func TestConfigValidation_MultipleUnlimitedWithReplication(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Backends = []BackendConfig{
		{Name: "u1", Endpoint: "e", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s", QuotaBytes: 0},
		{Name: "u2", Endpoint: "e", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s", QuotaBytes: 0},
	}
	cfg.Replication = ReplicationConfig{Factor: 2}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("multiple unlimited backends with replication should pass: %v", err)
	}
}

// TestConfigValidation_QuotaBackendsWithReplication verifies the config validation quota backends with replication contract.
// Asserts that quota'd backends with replication should pass:.
func TestConfigValidation_QuotaBackendsWithReplication(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Backends = []BackendConfig{
		{Name: "q1", Endpoint: "e", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s", QuotaBytes: 1024},
		{Name: "q2", Endpoint: "e", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s", QuotaBytes: 2048},
	}
	cfg.Replication = ReplicationConfig{Factor: 2}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("quota'd backends with replication should pass: %v", err)
	}
}

// TestConfigValidation_MultiBackendNoReplicationWarns verifies the config validation multi backend no replication warns contract.
// Asserts that multi-backend with factor=1 should pass validation (warn only):.
func TestConfigValidation_MultiBackendNoReplicationWarns(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Backends = []BackendConfig{
		{Name: "b1", Endpoint: "e", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s", QuotaBytes: 1024},
		{Name: "b2", Endpoint: "e2", Bucket: "b2", AccessKeyID: "a2", SecretAccessKey: "s2", QuotaBytes: 1024},
	}
	cfg.Replication = ReplicationConfig{Factor: 1}

	// Should pass validation (warning, not error)  -  replication.factor=1 is valid
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("multi-backend with factor=1 should pass validation (warn only): %v", err)
	}
}

// -------------------------------------------------------------------------
// BUCKET VALIDATION TESTS
// -------------------------------------------------------------------------

// TestConfigValidation_NoBuckets verifies the config validation no buckets contract.
// Asserts that error should mention missing buckets, got:.
func TestConfigValidation_NoBuckets(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Buckets = nil

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("no buckets should fail validation")
	}
	if !strings.Contains(err.Error(), "at least one bucket") {
		t.Errorf("error should mention missing buckets, got: %v", err)
	}
}

// TestConfigValidation_DuplicateBucketNames verifies the config validation duplicate bucket names contract.
// Asserts that error should mention duplicate bucket, got:.
func TestConfigValidation_DuplicateBucketNames(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Buckets = []BucketConfig{
		{Name: "dup", Credentials: []CredentialConfig{{AccessKeyID: "A1", SecretAccessKey: "s1"}}},
		{Name: "dup", Credentials: []CredentialConfig{{AccessKeyID: "A2", SecretAccessKey: "s2"}}},
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("duplicate bucket names should fail validation")
	}
	if !strings.Contains(err.Error(), "duplicate bucket name") {
		t.Errorf("error should mention duplicate bucket, got: %v", err)
	}
}

// TestConfigValidation_DuplicateAccessKeysAcrossBuckets verifies the config validation duplicate access keys across buckets contract.
// Asserts that error should mention duplicate access key, got:.
func TestConfigValidation_DuplicateAccessKeysAcrossBuckets(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Buckets = []BucketConfig{
		{Name: "b1", Credentials: []CredentialConfig{{AccessKeyID: "SAME", SecretAccessKey: "s1"}}},
		{Name: "b2", Credentials: []CredentialConfig{{AccessKeyID: "SAME", SecretAccessKey: "s2"}}},
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("duplicate access keys across buckets should fail validation")
	}
	if !strings.Contains(err.Error(), "duplicate access_key_id") {
		t.Errorf("error should mention duplicate access key, got: %v", err)
	}
}

// TestConfigValidation_BucketMissingCredentials verifies the config validation bucket missing credentials contract.
// Asserts that error should mention missing credentials, got:.
func TestConfigValidation_BucketMissingCredentials(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Buckets = []BucketConfig{
		{Name: "empty", Credentials: nil},
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("bucket with no credentials should fail validation")
	}
	if !strings.Contains(err.Error(), "at least one credential") {
		t.Errorf("error should mention missing credentials, got: %v", err)
	}
}

// TestConfigValidation_CredentialWithNoAuthMethod verifies the config validation credential with no auth method contract.
// Asserts that error should mention missing auth, got:.
func TestConfigValidation_CredentialWithNoAuthMethod(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Buckets = []BucketConfig{
		{Name: "bad", Credentials: []CredentialConfig{{}}},
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("credential with no auth method should fail validation")
	}
	if !strings.Contains(err.Error(), "must have access_key_id+secret_access_key or token") {
		t.Errorf("error should mention missing auth, got: %v", err)
	}
}

// TestConfigValidation_BucketNameWithSlash verifies the config validation bucket name with slash contract.
// Asserts that error should mention slash in name, got:.
func TestConfigValidation_BucketNameWithSlash(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Buckets = []BucketConfig{
		{Name: "bad/name", Credentials: []CredentialConfig{{AccessKeyID: "A", SecretAccessKey: "s"}}},
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("bucket name with '/' should fail validation")
	}
	if !strings.Contains(err.Error(), "must not contain '/'") {
		t.Errorf("error should mention slash in name, got: %v", err)
	}
}

// TestConfigValidation_MultipleCredentialsOnSameBucket verifies the config validation multiple credentials on same bucket contract.
// Asserts that multiple credentials on same bucket should pass:.
func TestConfigValidation_MultipleCredentialsOnSameBucket(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Buckets = []BucketConfig{
		{Name: "shared", Credentials: []CredentialConfig{
			{AccessKeyID: "WRITER", SecretAccessKey: "ws"},
			{AccessKeyID: "READER", SecretAccessKey: "rs"},
		}},
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("multiple credentials on same bucket should pass: %v", err)
	}
}

// TestConfigValidation_TokenCredential verifies the config validation token credential contract.
// Asserts that token-only credential should pass:.
func TestConfigValidation_TokenCredential(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Buckets = []BucketConfig{
		{Name: "legacy", Credentials: []CredentialConfig{
			{Token: "my-token"},
		}},
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("token-only credential should pass: %v", err)
	}
}

// TestConfigValidation_BucketMissingName verifies the config validation bucket missing name contract.
// Asserts that error should mention missing name, got:.
func TestConfigValidation_BucketMissingName(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Buckets = []BucketConfig{
		{Name: "", Credentials: []CredentialConfig{{AccessKeyID: "A", SecretAccessKey: "s"}}},
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("bucket with empty name should fail validation")
	}
	if !strings.Contains(err.Error(), "name is required") {
		t.Errorf("error should mention missing name, got: %v", err)
	}
}

// TestConfigValidation_NegativeMaxMultipartUploads verifies the config validation negative max multipart uploads path by exercising cfg.SetDefaultsAndValidate.
func TestConfigValidation_NegativeMaxMultipartUploads(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Buckets[0].MaxMultipartUploads = -1

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("negative max_multipart_uploads should fail validation")
	}
}

// TestConfigValidation_ZeroMaxMultipartUploads verifies the config validation zero max multipart uploads contract.
// Asserts that zero max_multipart_uploads (unlimited) should pass:.
func TestConfigValidation_ZeroMaxMultipartUploads(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Buckets[0].MaxMultipartUploads = 0

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("zero max_multipart_uploads (unlimited) should pass: %v", err)
	}
}

// TestConfigValidation_PositiveMaxMultipartUploads verifies the config validation positive max multipart uploads contract.
// Asserts that positive max_multipart_uploads should pass:.
func TestConfigValidation_PositiveMaxMultipartUploads(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Buckets[0].MaxMultipartUploads = 100

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("positive max_multipart_uploads should pass: %v", err)
	}
}

// -------------------------------------------------------------------------
// NON-RELOADABLE FIELDS CHANGED TESTS
// -------------------------------------------------------------------------

// TestNonReloadableFieldsChanged_IdenticalConfigs verifies the non reloadable fields changed identical configs contract.
// Asserts that identical configs should return empty slice, got.
func TestNonReloadableFieldsChanged_IdenticalConfigs(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	if len(changed) != 0 {
		t.Errorf("identical configs should return empty slice, got %v", changed)
	}
}

// TestNonReloadableFieldsChanged_ListenAddr verifies the non reloadable fields changed listen addr contract.
// Asserts that expected [server.listen_addr], got.
func TestNonReloadableFieldsChanged_ListenAddr(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	b.Server.ListenAddr = ":8080"
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	if len(changed) != 1 || changed[0] != "server.listen_addr" {
		t.Errorf("expected [server.listen_addr], got %v", changed)
	}
}

// TestNonReloadableFieldsChanged_MaxConcurrentRequests verifies the non reloadable fields changed max concurrent requests contract.
// Asserts that expected [server.max_concurrent_requests], got.
func TestNonReloadableFieldsChanged_MaxConcurrentRequests(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	b.Server.MaxConcurrentRequests = 100
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	if len(changed) != 1 || changed[0] != "server.max_concurrent_requests" {
		t.Errorf("expected [server.max_concurrent_requests], got %v", changed)
	}
}

// TestNonReloadableFieldsChanged_MaxConcurrentReads verifies the non reloadable fields changed max concurrent reads contract.
// Asserts that expected [server.max_concurrent_reads], got.
func TestNonReloadableFieldsChanged_MaxConcurrentReads(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	b.Server.MaxConcurrentRequests = a.Server.MaxConcurrentRequests // preserve defaulted global
	b.Server.MaxConcurrentReads = 50
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	if len(changed) != 1 || changed[0] != "server.max_concurrent_reads" {
		t.Errorf("expected [server.max_concurrent_reads], got %v", changed)
	}
}

// TestNonReloadableFieldsChanged_MaxConcurrentWrites verifies the non reloadable fields changed max concurrent writes contract.
// Asserts that expected [server.max_concurrent_writes], got.
func TestNonReloadableFieldsChanged_MaxConcurrentWrites(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	b.Server.MaxConcurrentRequests = a.Server.MaxConcurrentRequests // preserve defaulted global
	b.Server.MaxConcurrentWrites = 25
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	if len(changed) != 1 || changed[0] != "server.max_concurrent_writes" {
		t.Errorf("expected [server.max_concurrent_writes], got %v", changed)
	}
}

// TestNonReloadableFieldsChanged_LoadShedThreshold verifies the non reloadable fields changed load shed threshold contract.
// Asserts that expected [server.load_shed_threshold], got.
func TestNonReloadableFieldsChanged_LoadShedThreshold(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	b.Server.LoadShedThreshold = 0.8
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	if len(changed) != 1 || changed[0] != "server.load_shed_threshold" {
		t.Errorf("expected [server.load_shed_threshold], got %v", changed)
	}
}

// TestNonReloadableFieldsChanged_AdmissionWait verifies the non reloadable fields changed admission wait contract.
// Asserts that expected [server.admission_wait], got.
func TestNonReloadableFieldsChanged_AdmissionWait(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	b.Server.AdmissionWait = 100 * time.Millisecond
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	if len(changed) != 1 || changed[0] != "server.admission_wait" {
		t.Errorf("expected [server.admission_wait], got %v", changed)
	}
}

// TestNonReloadableFieldsChanged_Database verifies the non reloadable fields changed database contract.
// Asserts that expected 'database' in changed list, got.
func TestNonReloadableFieldsChanged_Database(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	b.Database.Host = "newhost"
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	found := false
	for _, c := range changed {
		if c == "database" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'database' in changed list, got %v", changed)
	}
}

// TestNonReloadableFieldsChanged_BackendStructuralFields verifies the non reloadable fields changed backend structural fields contract.
// Asserts that expected backend structural fields change, got.
func TestNonReloadableFieldsChanged_BackendStructuralFields(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	b.Backends[0].Endpoint = "https://new-endpoint.example.com"
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	found := false
	for _, c := range changed {
		if strings.Contains(c, "structural fields") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected backend structural fields change, got %v", changed)
	}
}

// TestNonReloadableFieldsChanged_BackendCredentials verifies the non reloadable fields changed backend credentials contract.
// Asserts that expected backend structural fields change for credentials, got.
func TestNonReloadableFieldsChanged_BackendCredentials(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	b.Backends[0].SecretAccessKey = "new-secret"
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	found := false
	for _, c := range changed {
		if strings.Contains(c, "structural fields") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected backend structural fields change for credentials, got %v", changed)
	}
}

// TestNonReloadableFieldsChanged_BackendCountChanged verifies the non reloadable fields changed backend count changed contract.
// Asserts that expected 'backends (count changed)', got.
func TestNonReloadableFieldsChanged_BackendCountChanged(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfigTwoBackends()
	_ = a.SetDefaultsAndValidate()
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	found := false
	for _, c := range changed {
		if strings.Contains(c, "count changed") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'backends (count changed)', got %v", changed)
	}
}

// TestNonReloadableFieldsChanged_ReloadableOnlyChanges verifies the non reloadable fields changed reloadable only changes contract.
// Asserts that reloadable-only changes should return empty slice, got.
func TestNonReloadableFieldsChanged_ReloadableOnlyChanges(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	_ = b.SetDefaultsAndValidate()

	// These are reloadable fields  -  should NOT appear in the result
	b.Backends[0].QuotaBytes = 9999
	b.Backends[0].APIRequestLimit = 5000
	b.Backends[0].EgressByteLimit = 1000
	b.Backends[0].IngressByteLimit = 2000
	b.RateLimit = RateLimitConfig{Enabled: true, RequestsPerSec: 50, Burst: 100}
	b.Rebalance = RebalanceConfig{Enabled: true, Strategy: "spread", Interval: time.Hour, BatchSize: 50, Threshold: 0.2}
	b.Replication = ReplicationConfig{Factor: 1, WorkerInterval: time.Minute, BatchSize: 25}
	b.Buckets = []BucketConfig{
		{Name: "new-bucket", Credentials: []CredentialConfig{{AccessKeyID: "NEW", SecretAccessKey: "newsecret"}}},
	}

	changed := NonReloadableFieldsChanged(&a, &b)
	if len(changed) != 0 {
		t.Errorf("reloadable-only changes should return empty slice, got %v", changed)
	}
}

// TestNonReloadableFieldsChanged_UnsignedPayloadChanged verifies the non reloadable fields changed unsigned payload changed contract.
// Asserts that expected backend structural fields change for unsigned_payload, got.
func TestNonReloadableFieldsChanged_UnsignedPayloadChanged(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	f := false
	b.Backends[0].UnsignedPayload = &f
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	found := false
	for _, c := range changed {
		if strings.Contains(c, "structural fields") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected backend structural fields change for unsigned_payload, got %v", changed)
	}
}

// TestNonReloadableFieldsChanged_UnsignedPayloadBothNil verifies the non reloadable fields changed unsigned payload both nil contract.
// Asserts that both nil unsigned_payload should be identical, got.
func TestNonReloadableFieldsChanged_UnsignedPayloadBothNil(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	// Both nil  -  should be treated as identical (both default to true)
	_ = a.SetDefaultsAndValidate()
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	if len(changed) != 0 {
		t.Errorf("both nil unsigned_payload should be identical, got %v", changed)
	}
}

// TestNonReloadableFieldsChanged_UnsignedPayloadExplicitTrue verifies the non reloadable fields changed unsigned payload explicit true contract.
// Asserts that explicit true should match nil default, got.
func TestNonReloadableFieldsChanged_UnsignedPayloadExplicitTrue(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	// Explicitly true should match nil (default true)
	tr := true
	b.Backends[0].UnsignedPayload = &tr
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	if len(changed) != 0 {
		t.Errorf("explicit true should match nil default, got %v", changed)
	}
}

// TestNonReloadableFieldsChanged_DisableChecksumChanged verifies the non reloadable fields changed disable checksum changed contract.
// Asserts that expected backend structural fields change for disable_checksum, got.
func TestNonReloadableFieldsChanged_DisableChecksumChanged(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	b.Backends[0].DisableChecksum = true
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	found := false
	for _, c := range changed {
		if strings.Contains(c, "structural fields") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected backend structural fields change for disable_checksum, got %v", changed)
	}
}

// TestNonReloadableFieldsChanged_DisableChecksumBothTrue verifies the non reloadable fields changed disable checksum both true contract.
// Asserts that both disable_checksum=true should be identical, got.
func TestNonReloadableFieldsChanged_DisableChecksumBothTrue(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	a.Backends[0].DisableChecksum = true
	b.Backends[0].DisableChecksum = true
	_ = a.SetDefaultsAndValidate()
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	if len(changed) != 0 {
		t.Errorf("both disable_checksum=true should be identical, got %v", changed)
	}
}

// TestNonReloadableFieldsChanged_DisableChecksumBothFalse verifies the non reloadable fields changed disable checksum both false contract.
// Asserts that both disable_checksum=false (default) should be identical, got.
func TestNonReloadableFieldsChanged_DisableChecksumBothFalse(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	if len(changed) != 0 {
		t.Errorf("both disable_checksum=false (default) should be identical, got %v", changed)
	}
}

// TestNonReloadableFieldsChanged_StripSDKHeadersChanged verifies the non reloadable fields changed strip sdkheaders changed contract.
// Asserts that expected backend structural fields change for strip_sdk_headers, got.
func TestNonReloadableFieldsChanged_StripSDKHeadersChanged(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	b.Backends[0].StripSDKHeaders = true
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	found := false
	for _, c := range changed {
		if strings.Contains(c, "structural fields") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected backend structural fields change for strip_sdk_headers, got %v", changed)
	}
}

// TestNonReloadableFieldsChanged_StripSDKHeadersBothTrue verifies the non reloadable fields changed strip sdkheaders both true contract.
// Asserts that both strip_sdk_headers=true should be identical, got.
func TestNonReloadableFieldsChanged_StripSDKHeadersBothTrue(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	a.Backends[0].StripSDKHeaders = true
	b.Backends[0].StripSDKHeaders = true
	_ = a.SetDefaultsAndValidate()
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	if len(changed) != 0 {
		t.Errorf("both strip_sdk_headers=true should be identical, got %v", changed)
	}
}

// TestNonReloadableFieldsChanged_StripSDKHeadersBothFalse verifies the non reloadable fields changed strip sdkheaders both false contract.
// Asserts that both strip_sdk_headers=false (default) should be identical, got.
func TestNonReloadableFieldsChanged_StripSDKHeadersBothFalse(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	if len(changed) != 0 {
		t.Errorf("both strip_sdk_headers=false (default) should be identical, got %v", changed)
	}
}

// TestBoolDefault verifies the bool default contract.
// Asserts that boolDefault(nil, true) = , want true.
func TestBoolDefault(t *testing.T) {
	t.Parallel()
	tr := true
	f := false

	if got := boolDefault(nil, true); got != true {
		t.Errorf("boolDefault(nil, true) = %v, want true", got)
	}
	if got := boolDefault(nil, false); got != false {
		t.Errorf("boolDefault(nil, false) = %v, want false", got)
	}
	if got := boolDefault(&tr, false); got != true {
		t.Errorf("boolDefault(&true, false) = %v, want true", got)
	}
	if got := boolDefault(&f, true); got != false {
		t.Errorf("boolDefault(&false, true) = %v, want false", got)
	}
}

// TestConfigValidation_TLS_CertWithoutKey verifies the config validation tls cert without key contract.
// Asserts that expected cert+key pair error, got.
func TestConfigValidation_TLS_CertWithoutKey(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Server.TLS.CertFile = "/etc/cert.pem"
	err := cfg.SetDefaultsAndValidate()
	if err == nil || !strings.Contains(err.Error(), "both cert_file and key_file") {
		t.Errorf("expected cert+key pair error, got %v", err)
	}
}

// TestConfigValidation_TLS_KeyWithoutCert verifies the config validation tls key without cert contract.
// Asserts that expected cert+key pair error, got.
func TestConfigValidation_TLS_KeyWithoutCert(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Server.TLS.KeyFile = "/etc/key.pem"
	err := cfg.SetDefaultsAndValidate()
	if err == nil || !strings.Contains(err.Error(), "both cert_file and key_file") {
		t.Errorf("expected cert+key pair error, got %v", err)
	}
}

// TestConfigValidation_TLS_ValidPair verifies the config validation tls valid pair contract.
// Asserts that valid TLS config should pass:.
func TestConfigValidation_TLS_ValidPair(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Server.TLS.CertFile = "/etc/cert.pem"
	cfg.Server.TLS.KeyFile = "/etc/key.pem"
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("valid TLS config should pass: %v", err)
	}
	if cfg.Server.TLS.MinVersion != "1.2" {
		t.Errorf("min_version default = %q, want \"1.2\"", cfg.Server.TLS.MinVersion)
	}
}

// TestConfigValidation_TLS_InvalidMinVersion verifies the config validation tls invalid min version contract.
// Asserts that expected min_version error, got.
func TestConfigValidation_TLS_InvalidMinVersion(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Server.TLS.CertFile = "/etc/cert.pem"
	cfg.Server.TLS.KeyFile = "/etc/key.pem"
	cfg.Server.TLS.MinVersion = "1.1"
	err := cfg.SetDefaultsAndValidate()
	if err == nil || !strings.Contains(err.Error(), "min_version") {
		t.Errorf("expected min_version error, got %v", err)
	}
}

// TestConfigValidation_TLS_MinVersion13 verifies the config validation tls min version13 contract.
// Asserts that TLS 1.3 should be valid:.
func TestConfigValidation_TLS_MinVersion13(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Server.TLS.CertFile = "/etc/cert.pem"
	cfg.Server.TLS.KeyFile = "/etc/key.pem"
	cfg.Server.TLS.MinVersion = "1.3"
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("TLS 1.3 should be valid: %v", err)
	}
}

// TestConfigValidation_TLS_NoTLSIsValid verifies the config validation tls no tlsis valid contract.
// Asserts that no TLS config should pass:.
func TestConfigValidation_TLS_NoTLSIsValid(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("no TLS config should pass: %v", err)
	}
}

// TestNonReloadableFieldsChanged_TLS verifies the non reloadable fields changed tls contract.
// Asserts that expected server.tls in changed fields, got.
func TestNonReloadableFieldsChanged_TLS(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()

	b.Server.TLS.CertFile = "/etc/cert.pem"
	b.Server.TLS.KeyFile = "/etc/key.pem"
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	found := false
	for _, c := range changed {
		if c == "server.tls" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected server.tls in changed fields, got %v", changed)
	}
}

// TestNonReloadableFieldsChanged_MultipleChanges verifies the non reloadable fields changed multiple changes contract.
// Asserts that expected at least 3 changed fields, got.
func TestNonReloadableFieldsChanged_MultipleChanges(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()

	b.Server.ListenAddr = ":8080"
	b.Database.Host = "newhost"
	b.RoutingStrategy = RoutingSpread
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	if len(changed) < 3 {
		t.Errorf("expected at least 3 changed fields, got %v", changed)
	}
}

// -------------------------------------------------------------------------
// USAGE FLUSH CONFIG TESTS
// -------------------------------------------------------------------------

// TestUsageFlushConfig_Defaults verifies the usage flush config defaults contract.
// Asserts that valid config should pass:.
func TestUsageFlushConfig_Defaults(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("valid config should pass: %v", err)
	}

	if cfg.UsageFlush.Interval != 30*time.Second {
		t.Errorf("interval default = %v, want 30s", cfg.UsageFlush.Interval)
	}
	if cfg.UsageFlush.AdaptiveThreshold != 0.8 {
		t.Errorf("adaptive_threshold default = %f, want 0.8", cfg.UsageFlush.AdaptiveThreshold)
	}
	if cfg.UsageFlush.FastInterval != 5*time.Second {
		t.Errorf("fast_interval default = %v, want 5s", cfg.UsageFlush.FastInterval)
	}
	if cfg.UsageFlush.AdaptiveEnabled {
		t.Error("adaptive_enabled default should be false")
	}
}

// TestUsageFlushConfig_CustomValues verifies the usage flush config custom values contract.
// Asserts that valid custom usage flush config should pass:.
func TestUsageFlushConfig_CustomValues(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.UsageFlush = UsageFlushConfig{
		Interval:          15 * time.Second,
		AdaptiveEnabled:   true,
		AdaptiveThreshold: 0.9,
		FastInterval:      2 * time.Second,
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("valid custom usage flush config should pass: %v", err)
	}

	if cfg.UsageFlush.Interval != 15*time.Second {
		t.Errorf("interval = %v, want 15s", cfg.UsageFlush.Interval)
	}
	if cfg.UsageFlush.AdaptiveThreshold != 0.9 {
		t.Errorf("adaptive_threshold = %f, want 0.9", cfg.UsageFlush.AdaptiveThreshold)
	}
}

// TestUsageFlushConfig_FastIntervalExceedsInterval verifies the usage flush config fast interval exceeds interval contract.
// Asserts that error should mention fast_interval, got:.
func TestUsageFlushConfig_FastIntervalExceedsInterval(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.UsageFlush = UsageFlushConfig{
		Interval:          10 * time.Second,
		AdaptiveThreshold: 0.8,
		FastInterval:      20 * time.Second, // bigger than interval
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("fast_interval >= interval should fail validation")
	}
	if !strings.Contains(err.Error(), "fast_interval must be less than") {
		t.Errorf("error should mention fast_interval, got: %v", err)
	}
}

// TestUsageFlushConfig_InvalidThreshold verifies the usage flush config invalid threshold contract.
// Asserts that error should mention adaptive_threshold, got:.
func TestUsageFlushConfig_InvalidThreshold(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.UsageFlush = UsageFlushConfig{
		Interval:          30 * time.Second,
		AdaptiveThreshold: 1.5, // out of range
		FastInterval:      5 * time.Second,
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("threshold > 1 should fail validation")
	}
	if !strings.Contains(err.Error(), "adaptive_threshold must be between") {
		t.Errorf("error should mention adaptive_threshold, got: %v", err)
	}
}

// -------------------------------------------------------------------------
// LIFECYCLE CONFIG TESTS
// -------------------------------------------------------------------------

// TestLifecycleConfig_EmptyRulesValid verifies the lifecycle config empty rules valid contract.
// Asserts that empty lifecycle rules should pass:.
func TestLifecycleConfig_EmptyRulesValid(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	// No lifecycle rules  -  should be valid (disabled)
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("empty lifecycle rules should pass: %v", err)
	}
}

// TestLifecycleConfig_ValidRules verifies the lifecycle config valid rules contract.
// Asserts that valid lifecycle rules should pass:.
func TestLifecycleConfig_ValidRules(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Lifecycle = LifecycleConfig{
		Rules: []LifecycleRule{
			{Prefix: "tmp/", ExpirationDays: 7},
			{Prefix: "uploads/staging/", ExpirationDays: 1},
		},
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("valid lifecycle rules should pass: %v", err)
	}
}

// TestLifecycleConfig_MissingPrefix verifies the lifecycle config missing prefix contract.
// Asserts that error should wrap ErrLifecyclePrefixRequired, got:.
func TestLifecycleConfig_MissingPrefix(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Lifecycle = LifecycleConfig{
		Rules: []LifecycleRule{
			{Prefix: "", ExpirationDays: 7},
		},
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("empty prefix should fail validation")
	}
	if !errors.Is(err, ErrLifecycleFilterRequired) {
		t.Errorf("error should wrap ErrLifecycleFilterRequired, got: %v", err)
	}
}

// TestLifecycleConfig_ZeroExpirationDays verifies the lifecycle config zero expiration days contract.
// Asserts that error should mention expiration_days, got:.
func TestLifecycleConfig_ZeroExpirationDays(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Lifecycle = LifecycleConfig{
		Rules: []LifecycleRule{
			{Prefix: "tmp/", ExpirationDays: 0},
		},
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("zero expiration_days should fail validation")
	}
	if !strings.Contains(err.Error(), "expiration_days must be positive") {
		t.Errorf("error should mention expiration_days, got: %v", err)
	}
}

// TestLifecycleConfig_NegativeExpirationDays verifies the lifecycle config negative expiration days path by exercising cfg.SetDefaultsAndValidate.
func TestLifecycleConfig_NegativeExpirationDays(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Lifecycle = LifecycleConfig{
		Rules: []LifecycleRule{
			{Prefix: "tmp/", ExpirationDays: -1},
		},
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("negative expiration_days should fail validation")
	}
}

// TestLifecycleConfig_DuplicatePrefix verifies the lifecycle config duplicate prefix contract.
// Asserts that error should mention duplicate prefix, got:.
func TestLifecycleConfig_DuplicatePrefix(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Lifecycle = LifecycleConfig{
		Rules: []LifecycleRule{
			{Prefix: "tmp/", ExpirationDays: 7},
			{Prefix: "tmp/", ExpirationDays: 3},
		},
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("duplicate prefix should fail validation")
	}
	if !strings.Contains(err.Error(), "duplicate filter") {
		t.Errorf("error should mention a duplicate filter, got: %v", err)
	}
}

// TestLifecycleConfig_TagsSatisfyTheFilterRequirement verifies a rule may
// filter on tags alone. The prefix requirement exists to stop a rule matching
// the whole namespace, which a tag filter also prevents.
func TestLifecycleConfig_TagsSatisfyTheFilterRequirement(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Lifecycle = LifecycleConfig{
		Rules: []LifecycleRule{
			{Tags: map[string]string{"scratch": "true"}, ExpirationDays: 7},
		},
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("a tags-only rule should validate, got: %v", err)
	}
}

// TestLifecycleConfig_NoFilterRejected verifies a rule with neither a prefix
// nor tags is refused rather than expiring every object in the namespace.
func TestLifecycleConfig_NoFilterRejected(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Lifecycle = LifecycleConfig{
		Rules: []LifecycleRule{{ExpirationDays: 7}},
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Fatal("a rule with no filter should fail validation")
	}
	if !errors.Is(err, ErrLifecycleFilterRequired) {
		t.Errorf("error should wrap ErrLifecycleFilterRequired, got: %v", err)
	}
}

// TestLifecycleConfig_EmptyTagKeyRejected verifies an empty tag key is caught
// at startup rather than becoming a filter that silently matches nothing.
func TestLifecycleConfig_EmptyTagKeyRejected(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Lifecycle = LifecycleConfig{
		Rules: []LifecycleRule{
			{Prefix: "tmp/", Tags: map[string]string{"": "x"}, ExpirationDays: 7},
		},
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Fatal("an empty tag key should fail validation")
	}
	if !errors.Is(err, ErrLifecycleEmptyTagKey) {
		t.Errorf("error should wrap ErrLifecycleEmptyTagKey, got: %v", err)
	}
}

// TestLifecycleConfig_SamePrefixDifferentTags verifies two rules sharing a
// prefix but differing by tag are a legitimate pair: they select different
// objects, so the duplicate check has to compare the whole filter.
func TestLifecycleConfig_SamePrefixDifferentTags(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Lifecycle = LifecycleConfig{
		Rules: []LifecycleRule{
			{Prefix: "logs/", Tags: map[string]string{"env": "staging"}, ExpirationDays: 7},
			{Prefix: "logs/", Tags: map[string]string{"env": "prod"}, ExpirationDays: 90},
		},
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("rules differing only by tag should validate, got: %v", err)
	}
}

// TestLifecycleRule_FilterIDIgnoresMapOrder verifies two rules carrying the
// same tags compare equal regardless of the order the map yields them, so
// duplicate detection cannot depend on Go's map iteration.
func TestLifecycleRule_FilterIDIgnoresMapOrder(t *testing.T) {
	t.Parallel()
	a := LifecycleRule{Prefix: "p/", Tags: map[string]string{"x": "1", "y": "2"}}
	b := LifecycleRule{Prefix: "p/", Tags: map[string]string{"y": "2", "x": "1"}}

	if a.filterID() != b.filterID() {
		t.Errorf("filterID differs by map order: %q vs %q", a.filterID(), b.filterID())
	}
}

// -------------------------------------------------------------------------
// RATE LIMIT CONFIG TESTS
// -------------------------------------------------------------------------

// TestRateLimitConfig_Defaults verifies the rate limit config defaults contract.
// Asserts that valid rate limit config should pass:.
func TestRateLimitConfig_Defaults(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.RateLimit = RateLimitConfig{Enabled: true}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("valid rate limit config should pass: %v", err)
	}

	if cfg.RateLimit.RequestsPerSec != 100 {
		t.Errorf("requests_per_sec default = %f, want 100", cfg.RateLimit.RequestsPerSec)
	}
	if cfg.RateLimit.Burst != 200 {
		t.Errorf("burst default = %d, want 200", cfg.RateLimit.Burst)
	}
}

// TestRateLimitConfig_DisabledSkipsValidation verifies the rate limit config disabled skips validation contract.
// Asserts that disabled rate limit should skip validation:.
func TestRateLimitConfig_DisabledSkipsValidation(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.RateLimit = RateLimitConfig{Enabled: false, RequestsPerSec: -1, Burst: -1}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("disabled rate limit should skip validation: %v", err)
	}
}

// -------------------------------------------------------------------------
// ROUTING STRATEGY TESTS
// -------------------------------------------------------------------------

// TestRoutingStrategy_DefaultsPack verifies the routing strategy defaults pack contract.
// Asserts that valid config should pass:.
func TestRoutingStrategy_DefaultsPack(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("valid config should pass: %v", err)
	}

	if cfg.RoutingStrategy != RoutingPack {
		t.Errorf("routing_strategy default = %q, want \"pack\"", cfg.RoutingStrategy)
	}
}

// TestRoutingStrategy_InvalidValue verifies the routing strategy invalid value contract.
// Asserts that error should mention routing_strategy, got:.
func TestRoutingStrategy_InvalidValue(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.RoutingStrategy = "invalid"

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("invalid routing_strategy should fail validation")
	}
	if !strings.Contains(err.Error(), "routing_strategy") {
		t.Errorf("error should mention routing_strategy, got: %v", err)
	}
}

// -------------------------------------------------------------------------
// TRACING CONFIG TESTS
// -------------------------------------------------------------------------

// TestTracingConfig_EnabledWithoutEndpoint verifies the tracing config enabled without endpoint contract.
// Asserts that error should mention tracing endpoint, got:.
func TestTracingConfig_EnabledWithoutEndpoint(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Telemetry.Tracing = TracingConfig{
		Enabled: true,
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("tracing enabled without endpoint should fail validation")
	}
	if !strings.Contains(err.Error(), "tracing.endpoint is required") {
		t.Errorf("error should mention tracing endpoint, got: %v", err)
	}
}

// TestTracingConfig_EnabledWithEndpoint verifies the tracing config enabled with endpoint contract.
// Asserts that tracing with endpoint should pass:.
func TestTracingConfig_EnabledWithEndpoint(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Telemetry.Tracing = TracingConfig{
		Enabled:  true,
		Endpoint: "localhost:4317",
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("tracing with endpoint should pass: %v", err)
	}

	if cfg.Telemetry.Tracing.SampleRate != 1.0 {
		t.Errorf("sample_rate default = %f, want 1.0", cfg.Telemetry.Tracing.SampleRate)
	}
}

// TestTracingConfig_DisabledSkipsEndpointValidation verifies the tracing config disabled skips endpoint validation contract.
// Asserts that disabled tracing should skip endpoint validation:.
func TestTracingConfig_DisabledSkipsEndpointValidation(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Telemetry.Tracing = TracingConfig{
		Enabled: false,
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("disabled tracing should skip endpoint validation: %v", err)
	}
}

// TestMetricsConfig_DefaultPath verifies the metrics config default path contract.
// Asserts that valid config should pass:.
func TestMetricsConfig_DefaultPath(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("valid config should pass: %v", err)
	}

	if cfg.Telemetry.Metrics.Path != "/metrics" {
		t.Errorf("metrics path default = %q, want \"/metrics\"", cfg.Telemetry.Metrics.Path)
	}
}

// TestMetricsConfig_ListenOptional verifies the metrics config listen optional contract.
// Asserts that valid config should pass:.
func TestMetricsConfig_ListenOptional(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Telemetry.Metrics.Enabled = true

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("valid config should pass: %v", err)
	}

	// Listen should be empty by default (metrics on main listener)
	if cfg.Telemetry.Metrics.Listen != "" {
		t.Errorf("metrics listen default = %q, want empty", cfg.Telemetry.Metrics.Listen)
	}
}

// TestMetricsConfig_ListenSet verifies the metrics config listen set contract.
// Asserts that valid config with metrics listen should pass:.
func TestMetricsConfig_ListenSet(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Telemetry.Metrics.Enabled = true
	cfg.Telemetry.Metrics.Listen = "127.0.0.1:9091"

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("valid config with metrics listen should pass: %v", err)
	}

	if cfg.Telemetry.Metrics.Listen != "127.0.0.1:9091" {
		t.Errorf("metrics listen = %q, want 127.0.0.1:9091", cfg.Telemetry.Metrics.Listen)
	}
}

// -------------------------------------------------------------------------
// LOADCONFIG TESTS
// -------------------------------------------------------------------------

// TestLoadConfig_ValidFile verifies the load config valid file contract.
// Asserts that LoadConfig:.
func TestLoadConfig_ValidFile(t *testing.T) {
	t.Parallel()
	yaml := `
server:
  listen_addr: ":9000"
buckets:
  - name: test
    credentials:
      - access_key_id: AKID
        secret_access_key: secret
database:
  host: localhost
  database: s3proxy
  user: s3proxy
backends:
  - name: b1
    endpoint: https://s3.example.com
    bucket: mybucket
    access_key_id: AKID
    secret_access_key: secret
    quota_bytes: 1024
`
	path := writeTempConfig(t, yaml)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Server.ListenAddr != ":9000" {
		t.Errorf("listen_addr = %q, want \":9000\"", cfg.Server.ListenAddr)
	}
	if cfg.Backends[0].Name != "b1" {
		t.Errorf("backend name = %q, want \"b1\"", cfg.Backends[0].Name)
	}
}

// TestLoadConfig_DisableChecksum verifies the load config disable checksum contract.
// Asserts that LoadConfig:.
func TestLoadConfig_DisableChecksum(t *testing.T) {
	t.Parallel()
	yaml := `
server:
  listen_addr: ":9000"
buckets:
  - name: test
    credentials:
      - access_key_id: AKID
        secret_access_key: secret
database:
  host: localhost
  database: s3proxy
  user: s3proxy
backends:
  - name: gcp
    endpoint: https://storage.googleapis.com
    bucket: mybucket
    access_key_id: AKID
    secret_access_key: secret
    disable_checksum: true
    quota_bytes: 1024
`
	path := writeTempConfig(t, yaml)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.Backends[0].DisableChecksum {
		t.Errorf("DisableChecksum = false, want true")
	}
}

// TestLoadConfig_DisableChecksumDefaultFalse verifies the load config disable checksum default false contract.
// Asserts that LoadConfig:.
func TestLoadConfig_DisableChecksumDefaultFalse(t *testing.T) {
	t.Parallel()
	yaml := `
server:
  listen_addr: ":9000"
buckets:
  - name: test
    credentials:
      - access_key_id: AKID
        secret_access_key: secret
database:
  host: localhost
  database: s3proxy
  user: s3proxy
backends:
  - name: b1
    endpoint: https://s3.example.com
    bucket: mybucket
    access_key_id: AKID
    secret_access_key: secret
    quota_bytes: 1024
`
	path := writeTempConfig(t, yaml)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Backends[0].DisableChecksum {
		t.Errorf("DisableChecksum = true, want false (default)")
	}
}

// TestLoadConfig_StripSDKHeaders verifies the load config strip sdkheaders contract.
// Asserts that LoadConfig:.
func TestLoadConfig_StripSDKHeaders(t *testing.T) {
	t.Parallel()
	yaml := `
server:
  listen_addr: ":9000"
buckets:
  - name: test
    credentials:
      - access_key_id: AKID
        secret_access_key: secret
database:
  host: localhost
  database: s3proxy
  user: s3proxy
backends:
  - name: gcp
    endpoint: https://storage.googleapis.com
    bucket: mybucket
    access_key_id: AKID
    secret_access_key: secret
    strip_sdk_headers: true
    quota_bytes: 1024
`
	path := writeTempConfig(t, yaml)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.Backends[0].StripSDKHeaders {
		t.Errorf("StripSDKHeaders = false, want true")
	}
}

// TestLoadConfig_StripSDKHeadersDefaultFalse verifies the load config strip sdkheaders default false contract.
// Asserts that LoadConfig:.
func TestLoadConfig_StripSDKHeadersDefaultFalse(t *testing.T) {
	t.Parallel()
	yaml := `
server:
  listen_addr: ":9000"
buckets:
  - name: test
    credentials:
      - access_key_id: AKID
        secret_access_key: secret
database:
  host: localhost
  database: s3proxy
  user: s3proxy
backends:
  - name: b1
    endpoint: https://s3.example.com
    bucket: mybucket
    access_key_id: AKID
    secret_access_key: secret
    quota_bytes: 1024
`
	path := writeTempConfig(t, yaml)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Backends[0].StripSDKHeaders {
		t.Errorf("StripSDKHeaders = true, want false (default)")
	}
}

// TestLoadConfig_NonexistentFile verifies the load config nonexistent file contract.
// Asserts that error should mention reading file, got:.
func TestLoadConfig_NonexistentFile(t *testing.T) {
	t.Parallel()
	_, err := LoadConfig("/tmp/nonexistent-config-file-abc123.yaml")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
	if !strings.Contains(err.Error(), "read config file") {
		t.Errorf("error should mention reading file, got: %v", err)
	}
}

// TestLoadConfig_InvalidYAML verifies the load config invalid yaml contract.
// Asserts that error should mention parsing, got:.
func TestLoadConfig_InvalidYAML(t *testing.T) {
	t.Parallel()
	path := writeTempConfig(t, "{{invalid yaml")

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
	if !strings.Contains(err.Error(), "parse config") {
		t.Errorf("error should mention parsing, got: %v", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error should include the config file path %q, got: %v", path, err)
	}
}

// TestLoadConfig_ValidationFailure verifies the load config validation failure contract.
// Asserts that error should mention invalid config, got:.
func TestLoadConfig_ValidationFailure(t *testing.T) {
	t.Parallel()
	// Valid YAML but fails validation (missing required fields)
	path := writeTempConfig(t, "server:\n  listen_addr: \"\"\n")

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "invalid config") {
		t.Errorf("error should mention invalid config, got: %v", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error should include the config file path %q, got: %v", path, err)
	}
}

// TestLoadConfig_MissingFile verifies that a non-existent config path
// surfaces both the loader sentinel and the path in the error message.
func TestLoadConfig_MissingFile(t *testing.T) {
	t.Parallel()
	path := "/nonexistent/path/config.yaml"
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !errors.Is(err, ErrReadConfigFile) {
		t.Errorf("expected ErrReadConfigFile in chain, got: %v", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error should include the config file path %q, got: %v", path, err)
	}
}

// TestLoadConfig_EnvVarExpansion verifies the load config env var expansion contract.
// Asserts that LoadConfig:.
func TestLoadConfig_EnvVarExpansion(t *testing.T) {
	t.Setenv("TEST_S3O_HOST", "envhost.example.com")
	t.Setenv("TEST_S3O_PASS", "envpass123")

	yaml := `
server:
  listen_addr: ":9000"
buckets:
  - name: test
    credentials:
      - access_key_id: AKID
        secret_access_key: secret
database:
  host: ${TEST_S3O_HOST}
  database: s3proxy
  user: s3proxy
  password: ${TEST_S3O_PASS}
backends:
  - name: b1
    endpoint: https://s3.example.com
    bucket: mybucket
    access_key_id: AKID
    secret_access_key: secret
    quota_bytes: 1024
`
	path := writeTempConfig(t, yaml)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Database.Host != "envhost.example.com" {
		t.Errorf("host = %q, want \"envhost.example.com\"", cfg.Database.Host)
	}
	if cfg.Database.Password != "envpass123" {
		t.Errorf("password = %q, want \"envpass123\"", cfg.Database.Password)
	}
}

// -------------------------------------------------------------------------
// UI CONFIG TESTS
// -------------------------------------------------------------------------

// TestUIConfig_EnabledMissingCredentials verifies the uiconfig enabled missing credentials contract.
// Asserts that error = , want mention of admin_key and admin_secret.
func TestUIConfig_EnabledMissingCredentials(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.UI = UIConfig{Enabled: true}

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Fatal("expected validation error for UI enabled without credentials")
	}
	if !strings.Contains(err.Error(), "admin_key") || !strings.Contains(err.Error(), "admin_secret") {
		t.Errorf("error = %q, want mention of admin_key and admin_secret", err)
	}
}

// TestUIConfig_EnabledMissingSessionSecret verifies the uiconfig enabled missing session secret contract.
// Asserts that error = , want mention of session_secret.
func TestUIConfig_EnabledMissingSessionSecret(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.UI = UIConfig{Enabled: true, AdminKey: "key", AdminSecret: "secret"}

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Fatal("expected validation error for UI enabled without session_secret")
	}
	if !strings.Contains(err.Error(), "session_secret") {
		t.Errorf("error = %q, want mention of session_secret", err)
	}
}

// TestUIConfig_EnabledWithCredentials verifies the uiconfig enabled with credentials contract.
// Asserts that valid UI config should pass:.
func TestUIConfig_EnabledWithCredentials(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.UI = UIConfig{Enabled: true, AdminKey: "key", AdminSecret: "secret", SessionSecret: "sess"}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("valid UI config should pass: %v", err)
	}
	if cfg.UI.Path != "/ui" {
		t.Errorf("UI.Path = %q, want /ui (default)", cfg.UI.Path)
	}
}

// TestUIConfig_DisabledSkipsValidation verifies the uiconfig disabled skips validation contract.
// Asserts that disabled UI should skip credential validation:.
func TestUIConfig_DisabledSkipsValidation(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.UI = UIConfig{Enabled: false}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("disabled UI should skip credential validation: %v", err)
	}
}

// TestUIConfig_SessionSecret verifies the uiconfig session secret contract.
// Asserts that UI config with session_secret should pass:.
func TestUIConfig_SessionSecret(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.UI = UIConfig{ //nolint:gosec // G101: test config values
		Enabled:       true,
		AdminKey:      "key",
		AdminSecret:   "secret",
		SessionSecret: "my-session-secret",
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("UI config with session_secret should pass: %v", err)
	}
	if cfg.UI.SessionSecret != "my-session-secret" {
		t.Errorf("SessionSecret = %q, want %q", cfg.UI.SessionSecret, "my-session-secret")
	}
}

// TestLogLevel_DefaultsToInfo verifies the log level defaults to info contract.
// Asserts that log_level default = , want.
func TestLogLevel_DefaultsToInfo(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Server.LogLevel != "info" {
		t.Errorf("log_level default = %q, want %q", cfg.Server.LogLevel, "info")
	}
}

// TestLogLevel_CustomValue verifies the log level custom value contract.
// Asserts that log_level should be valid:.
func TestLogLevel_CustomValue(t *testing.T) {
	t.Parallel()
	for _, level := range []string{"debug", "info", "warn", "error"} {
		cfg := validBaseConfig()
		cfg.Server.LogLevel = level
		if err := cfg.SetDefaultsAndValidate(); err != nil {
			t.Errorf("log_level %q should be valid: %v", level, err)
		}
	}
}

// TestLogLevel_InvalidValue verifies the log level invalid value contract.
// Asserts that error should mention log_level:.
func TestLogLevel_InvalidValue(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Server.LogLevel = "trace"
	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Fatal("expected error for invalid log_level")
	}
	if !strings.Contains(err.Error(), "log_level") {
		t.Errorf("error should mention log_level: %v", err)
	}
}

// -------------------------------------------------------------------------
// BackendCircuitBreakerConfig
// -------------------------------------------------------------------------

// TestBackendCircuitBreakerDefaults verifies the backend circuit breaker defaults contract.
// Asserts that unexpected error:.
func TestBackendCircuitBreakerDefaults(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.BackendCircuitBreaker = BackendCircuitBreakerConfig{Enabled: true}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BackendCircuitBreaker.FailureThreshold != 5 {
		t.Errorf("FailureThreshold = %d, want 5", cfg.BackendCircuitBreaker.FailureThreshold)
	}
	if cfg.BackendCircuitBreaker.OpenTimeout != 5*time.Minute {
		t.Errorf("OpenTimeout = %v, want 5m", cfg.BackendCircuitBreaker.OpenTimeout)
	}
}

// TestBackendCircuitBreakerDefaults_Disabled verifies the backend circuit breaker defaults disabled contract.
// Asserts that unexpected error:.
func TestBackendCircuitBreakerDefaults_Disabled(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	// Disabled (default)  -  defaults should NOT be applied
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BackendCircuitBreaker.FailureThreshold != 0 {
		t.Errorf("FailureThreshold should stay 0 when disabled, got %d", cfg.BackendCircuitBreaker.FailureThreshold)
	}
}

// TestBackendCircuitBreakerDefaults_CustomValues verifies the backend circuit breaker defaults custom values contract.
// Asserts that unexpected error:.
func TestBackendCircuitBreakerDefaults_CustomValues(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.BackendCircuitBreaker = BackendCircuitBreakerConfig{
		Enabled:          true,
		FailureThreshold: 10,
		OpenTimeout:      30 * time.Second,
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BackendCircuitBreaker.FailureThreshold != 10 {
		t.Errorf("FailureThreshold = %d, want 10 (custom)", cfg.BackendCircuitBreaker.FailureThreshold)
	}
	if cfg.BackendCircuitBreaker.OpenTimeout != 30*time.Second {
		t.Errorf("OpenTimeout = %v, want 30s (custom)", cfg.BackendCircuitBreaker.OpenTimeout)
	}
}

// TestNonReloadableFieldsChanged_BackendCircuitBreaker verifies the non reloadable fields changed backend circuit breaker contract.
// Asserts that expected backend_circuit_breaker in changed list, got.
func TestNonReloadableFieldsChanged_BackendCircuitBreaker(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	b.BackendCircuitBreaker = BackendCircuitBreakerConfig{Enabled: true, FailureThreshold: 5, OpenTimeout: 5 * time.Minute}
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	found := false
	for _, c := range changed {
		if c == "backend_circuit_breaker" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected backend_circuit_breaker in changed list, got %v", changed)
	}
}

// -------------------------------------------------------------------------
// ENCRYPTION VALIDATION
// -------------------------------------------------------------------------

// -------------------------------------------------------------------------
// REDIS VALIDATION
// -------------------------------------------------------------------------

// TestRedisConfig_MissingAddress verifies the redis config missing address contract.
// Asserts that missing redis address should fail, got:.
func TestRedisConfig_MissingAddress(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Redis = &RedisConfig{}

	err := cfg.SetDefaultsAndValidate()
	if err == nil || !strings.Contains(err.Error(), "redis.address is required") {
		t.Errorf("missing redis address should fail, got: %v", err)
	}
}

// TestRedisConfig_Defaults verifies the redis config defaults contract.
// Asserts that valid redis config should pass:.
func TestRedisConfig_Defaults(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Redis = &RedisConfig{Address: "localhost:6379"}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("valid redis config should pass: %v", err)
	}
	if cfg.Redis.KeyPrefix != "s3orch" {
		t.Errorf("KeyPrefix = %q, want 's3orch'", cfg.Redis.KeyPrefix)
	}
	if cfg.Redis.FailureThreshold != 3 {
		t.Errorf("FailureThreshold = %d, want 3", cfg.Redis.FailureThreshold)
	}
	if cfg.Redis.OpenTimeout != 15*time.Second {
		t.Errorf("OpenTimeout = %v, want 15s", cfg.Redis.OpenTimeout)
	}
}

// TestRedisConfig_CustomValues verifies the redis config custom values contract.
// Asserts that custom redis config should pass:.
func TestRedisConfig_CustomValues(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Redis = &RedisConfig{
		Address:          "redis:6379",
		KeyPrefix:        "myapp",
		FailureThreshold: 5,
		OpenTimeout:      30 * time.Second,
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("custom redis config should pass: %v", err)
	}
	if cfg.Redis.KeyPrefix != "myapp" {
		t.Errorf("KeyPrefix = %q, want 'myapp'", cfg.Redis.KeyPrefix)
	}
	if cfg.Redis.FailureThreshold != 5 {
		t.Errorf("FailureThreshold = %d, want 5", cfg.Redis.FailureThreshold)
	}
}

// TestNonReloadableFieldsChanged_RedisAdded verifies the non reloadable fields changed redis added contract.
// Asserts that expected redis in changed list, got.
func TestNonReloadableFieldsChanged_RedisAdded(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	b.Redis = &RedisConfig{Address: "localhost:6379"}
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	found := false
	for _, c := range changed {
		if c == "redis" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected redis in changed list, got %v", changed)
	}
}

// TestNonReloadableFieldsChanged_RedisRemoved verifies the non reloadable fields changed redis removed contract.
// Asserts that expected redis in changed list, got.
func TestNonReloadableFieldsChanged_RedisRemoved(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	a.Redis = &RedisConfig{Address: "localhost:6379"}
	_ = a.SetDefaultsAndValidate()
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	found := false
	for _, c := range changed {
		if c == "redis" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected redis in changed list, got %v", changed)
	}
}

// TestNonReloadableFieldsChanged_RedisModified verifies the non reloadable fields changed redis modified contract.
// Asserts that expected redis in changed list, got.
func TestNonReloadableFieldsChanged_RedisModified(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	a.Redis = &RedisConfig{Address: "localhost:6379"}
	b.Redis = &RedisConfig{Address: "redis:6379"}
	_ = a.SetDefaultsAndValidate()
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	found := false
	for _, c := range changed {
		if c == "redis" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected redis in changed list, got %v", changed)
	}
}

// TestNonReloadableFieldsChanged_RedisBothNil verifies the non reloadable fields changed redis both nil contract.
// Asserts that both nil redis should not appear in changed list.
func TestNonReloadableFieldsChanged_RedisBothNil(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	for _, c := range changed {
		if c == "redis" {
			t.Errorf("both nil redis should not appear in changed list")
		}
	}
}

// TestNonReloadableFieldsChanged_RedisIdentical verifies the non reloadable fields changed redis identical contract.
// Asserts that identical redis configs should not appear in changed list.
func TestNonReloadableFieldsChanged_RedisIdentical(t *testing.T) {
	t.Parallel()
	a := validBaseConfig()
	b := validBaseConfig()
	a.Redis = &RedisConfig{Address: "localhost:6379"}
	b.Redis = &RedisConfig{Address: "localhost:6379"}
	_ = a.SetDefaultsAndValidate()
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	for _, c := range changed {
		if c == "redis" {
			t.Errorf("identical redis configs should not appear in changed list")
		}
	}
}

// -------------------------------------------------------------------------
// ENCRYPTION VALIDATION
// -------------------------------------------------------------------------

// TestEncryptionConfig_ValidMasterKey verifies the encryption config valid master key contract.
// Asserts that valid encryption config should pass:.
func TestEncryptionConfig_ValidMasterKey(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Encryption = EncryptionConfig{
		Enabled:   true,
		MasterKey: "F2rpnHM7TmwJ4/DalNfk0cvCCPmHTfvB9LyhBLPoCVc=",
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("valid encryption config should pass: %v", err)
	}
	if cfg.Encryption.ChunkSize != 65536 {
		t.Errorf("ChunkSize default = %d, want 65536", cfg.Encryption.ChunkSize)
	}
}

// TestEncryptionConfig_CustomChunkSize verifies the encryption config custom chunk size contract.
// Asserts that 16KB chunk size should pass:.
func TestEncryptionConfig_CustomChunkSize(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Encryption = EncryptionConfig{
		Enabled:   true,
		MasterKey: "F2rpnHM7TmwJ4/DalNfk0cvCCPmHTfvB9LyhBLPoCVc=",
		ChunkSize: 16384,
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("16KB chunk size should pass: %v", err)
	}
}

// TestEncryptionConfig_ChunkSizeTooSmall verifies the encryption config chunk size too small contract.
// Asserts that chunk size below 4096 should fail, got:.
func TestEncryptionConfig_ChunkSizeTooSmall(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Encryption = EncryptionConfig{
		Enabled:   true,
		MasterKey: "F2rpnHM7TmwJ4/DalNfk0cvCCPmHTfvB9LyhBLPoCVc=",
		ChunkSize: 1024,
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil || !strings.Contains(err.Error(), "chunk_size must be between") {
		t.Errorf("chunk size below 4096 should fail, got: %v", err)
	}
}

// TestEncryptionConfig_ChunkSizeTooLarge verifies the encryption config chunk size too large contract.
// Asserts that chunk size above 1MB should fail, got:.
func TestEncryptionConfig_ChunkSizeTooLarge(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Encryption = EncryptionConfig{
		Enabled:   true,
		MasterKey: "F2rpnHM7TmwJ4/DalNfk0cvCCPmHTfvB9LyhBLPoCVc=",
		ChunkSize: 2097152,
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil || !strings.Contains(err.Error(), "chunk_size must be between") {
		t.Errorf("chunk size above 1MB should fail, got: %v", err)
	}
}

// TestEncryptionConfig_ChunkSizeNotPowerOf2 verifies the encryption config chunk size not power of2 contract.
// Asserts that non-power-of-2 chunk size should fail, got:.
func TestEncryptionConfig_ChunkSizeNotPowerOf2(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Encryption = EncryptionConfig{
		Enabled:   true,
		MasterKey: "F2rpnHM7TmwJ4/DalNfk0cvCCPmHTfvB9LyhBLPoCVc=",
		ChunkSize: 5000,
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil || !strings.Contains(err.Error(), "power of 2") {
		t.Errorf("non-power-of-2 chunk size should fail, got: %v", err)
	}
}

// TestEncryptionConfig_NoKeySource verifies the encryption config no key source contract.
// Asserts that missing key source should fail, got:.
func TestEncryptionConfig_NoKeySource(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Encryption = EncryptionConfig{Enabled: true}

	err := cfg.SetDefaultsAndValidate()
	if err == nil || !strings.Contains(err.Error(), "exactly one of master_key") {
		t.Errorf("missing key source should fail, got: %v", err)
	}
}

// TestEncryptionConfig_MultipleKeySources verifies the encryption config multiple key sources contract.
// Asserts that multiple key sources should fail, got:.
func TestEncryptionConfig_MultipleKeySources(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Encryption = EncryptionConfig{
		Enabled:       true,
		MasterKey:     "F2rpnHM7TmwJ4/DalNfk0cvCCPmHTfvB9LyhBLPoCVc=",
		MasterKeyFile: "/some/path",
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil || !strings.Contains(err.Error(), "only one of") {
		t.Errorf("multiple key sources should fail, got: %v", err)
	}
}

// TestEncryptionConfig_InvalidBase64MasterKey verifies the encryption config invalid base64 master key contract.
// Asserts that invalid base64 should fail, got:.
func TestEncryptionConfig_InvalidBase64MasterKey(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Encryption = EncryptionConfig{
		Enabled:   true,
		MasterKey: "not-valid-base64!!!",
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil || !strings.Contains(err.Error(), "invalid base64") {
		t.Errorf("invalid base64 should fail, got: %v", err)
	}
}

// TestEncryptionConfig_WrongKeyLength verifies the encryption config wrong key length contract.
// Asserts that wrong key length should fail, got:.
func TestEncryptionConfig_WrongKeyLength(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Encryption = EncryptionConfig{
		Enabled:   true,
		MasterKey: "dG9vc2hvcnQ=", // "tooshort" = 8 bytes
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil || !strings.Contains(err.Error(), "must be 256 bits") {
		t.Errorf("wrong key length should fail, got: %v", err)
	}
}

// TestEncryptionConfig_InvalidPreviousKey verifies the encryption config invalid previous key contract.
// Asserts that invalid previous key should fail, got:.
func TestEncryptionConfig_InvalidPreviousKey(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Encryption = EncryptionConfig{
		Enabled:      true,
		MasterKey:    "F2rpnHM7TmwJ4/DalNfk0cvCCPmHTfvB9LyhBLPoCVc=",
		PreviousKeys: []string{"not-valid!!!"},
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil || !strings.Contains(err.Error(), "previous_keys[0]") {
		t.Errorf("invalid previous key should fail, got: %v", err)
	}
}

// TestEncryptionConfig_PreviousKeyWrongLength verifies the encryption config previous key wrong length contract.
// Asserts that previous key wrong length should fail, got:.
func TestEncryptionConfig_PreviousKeyWrongLength(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Encryption = EncryptionConfig{
		Enabled:      true,
		MasterKey:    "F2rpnHM7TmwJ4/DalNfk0cvCCPmHTfvB9LyhBLPoCVc=",
		PreviousKeys: []string{"dG9vc2hvcnQ="}, // 8 bytes
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil || !strings.Contains(err.Error(), "previous_keys[0]") {
		t.Errorf("previous key wrong length should fail, got: %v", err)
	}
}

// TestEncryptionConfig_VaultMissingFields verifies the encryption config vault missing fields contract.
// Asserts that error should mention , got:.
func TestEncryptionConfig_VaultMissingFields(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Encryption = EncryptionConfig{
		Enabled: true,
		Vault:   &VaultTransitConfig{},
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("vault with missing fields should fail")
	}
	errStr := err.Error()
	for _, want := range []string{"vault.address", "token or token_file", "vault.key_name"} {
		if !strings.Contains(errStr, want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

// TestEncryptionConfig_VaultBothTokenAndTokenFile verifies the encryption config vault both token and token file contract.
// Asserts that unexpected error:.
func TestEncryptionConfig_VaultBothTokenAndTokenFile(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Encryption = EncryptionConfig{
		Enabled: true,
		Vault: &VaultTransitConfig{
			Address:   "https://vault.example.com",
			Token:     "static-token",
			TokenFile: "/path/to/token",
			KeyName:   "my-key",
		},
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("setting both token and token_file should fail")
	}
	if !strings.Contains(err.Error(), "only one of token or token_file") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestEncryptionConfig_VaultRenewIntervalDefault verifies the encryption config vault renew interval default contract.
// Asserts that unexpected error:.
func TestEncryptionConfig_VaultRenewIntervalDefault(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Encryption = EncryptionConfig{
		Enabled: true,
		Vault: &VaultTransitConfig{
			Address: "https://vault.example.com",
			Token:   "test-token",
			KeyName: "my-key",
		},
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Encryption.Vault.RenewInterval != 5*time.Minute {
		t.Errorf("RenewInterval = %v, want 5m", cfg.Encryption.Vault.RenewInterval)
	}
}

// TestEncryptionConfig_VaultDefaultMountPath verifies the encryption config vault default mount path contract.
// Asserts that valid vault config should pass:.
func TestEncryptionConfig_VaultDefaultMountPath(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Encryption = EncryptionConfig{
		Enabled: true,
		Vault: &VaultTransitConfig{
			Address: "http://vault:8200",
			Token:   "token",
			KeyName: "mykey",
		},
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("valid vault config should pass: %v", err)
	}
	if cfg.Encryption.Vault.MountPath != "transit" {
		t.Errorf("MountPath default = %q, want 'transit'", cfg.Encryption.Vault.MountPath)
	}
}

// TestEncryptionConfig_DisabledSkipsValidation verifies the encryption config disabled skips validation contract.
// Asserts that disabled encryption should skip validation:.
func TestEncryptionConfig_DisabledSkipsValidation(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Encryption = EncryptionConfig{
		Enabled:   false,
		MasterKey: "garbage",
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("disabled encryption should skip validation: %v", err)
	}
}

// TestParseLogLevel verifies the parse log level contract.
// Asserts that ParseLogLevel() = , want.
func TestParseLogLevel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"unknown", slog.LevelInfo},
	}
	for _, tt := range tests {
		got := ParseLogLevel(tt.input)
		if got != tt.want {
			t.Errorf("ParseLogLevel(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// writeTempConfig writes content to a temporary YAML file and returns its path.
func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	return path
}

// validBaseConfig returns a Config with all required fields populated (1 backend, 1 bucket).
func validBaseConfig() Config {
	return Config{
		Server: ServerConfig{ListenAddr: ":9000"},
		Buckets: []BucketConfig{
			{Name: "b", Credentials: []CredentialConfig{
				{AccessKeyID: "AKID", SecretAccessKey: "secret"},
			}},
		},
		Database: DatabaseConfig{Host: "h", Database: "d", User: "u"},
		Backends: []BackendConfig{
			{Name: "b1", Endpoint: "e", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s", QuotaBytes: 1024},
		},
	}
}

// validBaseConfigTwoBackends returns a Config with 2 backends for replication tests.
func validBaseConfigTwoBackends() Config {
	return Config{
		Server: ServerConfig{ListenAddr: ":9000"},
		Buckets: []BucketConfig{
			{Name: "b", Credentials: []CredentialConfig{
				{AccessKeyID: "AKID", SecretAccessKey: "secret"},
			}},
		},
		Database: DatabaseConfig{Host: "h", Database: "d", User: "u"},
		Backends: []BackendConfig{
			{Name: "b1", Endpoint: "e", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s", QuotaBytes: 1024},
			{Name: "b2", Endpoint: "e", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s", QuotaBytes: 2048},
		},
	}
}

// -------------------------------------------------------------------------
// Sub-validator direct tests  -  cover negative-value guards that are
// reachable via explicit YAML (e.g. interval: -5m) but not via the
// validBaseConfig() helper which always uses valid values.
// -------------------------------------------------------------------------

// TestBackendValidation_MissingFields verifies the backend validation missing fields contract.
// Asserts that expected error mentioning.
func TestBackendValidation_MissingFields(t *testing.T) {
	t.Parallel()
	errs := validateBackends([]BackendConfig{{}})
	for _, want := range []string{"endpoint", "bucket", "access_key_id", "secret_access_key"} {
		found := false
		for _, e := range errs {
			if strings.Contains(e.Error(), want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected error mentioning %q", want)
		}
	}
}

// TestBackendValidation_DefaultChainAcceptsEmptyKeys pins that a
// backend with credential_source=default_chain validates cleanly when
// no static keys are configured.
func TestBackendValidation_DefaultChainAcceptsEmptyKeys(t *testing.T) {
	t.Parallel()
	errs := validateBackends([]BackendConfig{{
		Name: "b1", Endpoint: "https://s3.amazonaws.com", Bucket: "b",
		CredentialSource: CredentialSourceDefaultChain,
	}})
	for _, e := range errs {
		if errors.Is(e, ErrAccessKeyIDReqd) || errors.Is(e, ErrSecretAccessKeyReqd) {
			t.Errorf("default_chain backend should not require static keys: %v", e)
		}
	}
}

// TestBackendValidation_DefaultChainRejectsStaticKeys ensures stale
// keys left in a default_chain backend are flagged rather than silently
// shadowing the SDK-resolved credentials.
func TestBackendValidation_DefaultChainRejectsStaticKeys(t *testing.T) {
	t.Parallel()
	errs := validateBackends([]BackendConfig{{
		Name: "b1", Endpoint: "https://s3.amazonaws.com", Bucket: "b",
		AccessKeyID: "ak", SecretAccessKey: "sk",
		CredentialSource: CredentialSourceDefaultChain,
	}})
	found := false
	for _, e := range errs {
		if errors.Is(e, ErrCredentialsWithDefaultChain) {
			found = true
		}
	}
	if !found {
		t.Errorf("expected ErrCredentialsWithDefaultChain, got %v", errs)
	}
}

// TestBackendValidation_StaticDefaultsRequireKeys keeps the original
// validation behaviour intact when CredentialSource is unset (defaults
// to "static").
func TestBackendValidation_StaticDefaultsRequireKeys(t *testing.T) {
	t.Parallel()
	errs := validateBackends([]BackendConfig{{
		Name: "b1", Endpoint: "https://s3.example.com", Bucket: "b",
	}})
	foundAK, foundSK := false, false
	for _, e := range errs {
		if errors.Is(e, ErrAccessKeyIDReqd) {
			foundAK = true
		}
		if errors.Is(e, ErrSecretAccessKeyReqd) {
			foundSK = true
		}
	}
	if !foundAK || !foundSK {
		t.Errorf("static (default) must require both keys: foundAK=%v foundSK=%v errs=%v", foundAK, foundSK, errs)
	}
}

// TestBackendValidation_UnknownCredentialSourceRejected pins that a
// typo in credential_source surfaces as a typed error rather than
// silently defaulting.
func TestBackendValidation_UnknownCredentialSourceRejected(t *testing.T) {
	t.Parallel()
	errs := validateBackends([]BackendConfig{{
		Name: "b1", Endpoint: "https://s3.example.com", Bucket: "b",
		AccessKeyID: "ak", SecretAccessKey: "sk",
		CredentialSource: "imds_only",
	}})
	for _, e := range errs {
		if errors.Is(e, ErrInvalidCredentialSource) {
			return
		}
	}
	t.Errorf("expected ErrInvalidCredentialSource, got %v", errs)
}

// TestBackendValidation_NegativeMaxObjectSize verifies the backend validation negative max object size path by exercising strings.Contains, e.Error.
func TestBackendValidation_NegativeMaxObjectSize(t *testing.T) {
	t.Parallel()
	errs := validateBackends([]BackendConfig{{
		Name: "b1", Endpoint: "https://s3.example.com", Bucket: "b",
		AccessKeyID: "ak", SecretAccessKey: "sk", MaxObjectSize: -1,
	}})
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "max_object_size") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected error mentioning max_object_size for negative value")
	}
}

// TestRebalanceValidation_NegativeValues verifies the rebalance validation negative values contract.
// Asserts that expected multiple errors, got :.
func TestRebalanceValidation_NegativeValues(t *testing.T) {
	t.Parallel()
	cfg := RebalanceConfig{Enabled: true, Strategy: "pack", Interval: -1, BatchSize: -1, Threshold: 2.0, Concurrency: -1}
	errs := cfg.setDefaultsAndValidate()
	if len(errs) < 3 {
		t.Errorf("expected multiple errors, got %d: %v", len(errs), errs)
	}
}

// TestReplicationValidation_NegativeValues verifies the replication validation negative values contract.
// Asserts that expected error mentioning.
func TestReplicationValidation_NegativeValues(t *testing.T) {
	t.Parallel()
	cfg := ReplicationConfig{Factor: 3, WorkerInterval: -1, BatchSize: -1}
	errs := cfg.setDefaultsAndValidate(2)
	for _, want := range []string{"factor", "worker_interval", "batch_size"} {
		found := false
		for _, e := range errs {
			if strings.Contains(e.Error(), want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected error mentioning %q", want)
		}
	}
}

// TestRateLimitValidation_NegativeValues verifies the rate limit validation negative values contract.
// Asserts that expected error mentioning.
func TestRateLimitValidation_NegativeValues(t *testing.T) {
	t.Parallel()
	cfg := RateLimitConfig{Enabled: true, RequestsPerSec: -1, Burst: -1, TrustedProxies: []string{"invalid"}}
	errs := cfg.setDefaultsAndValidate()
	for _, want := range []string{"requests_per_sec", "burst", "CIDR"} {
		found := false
		for _, e := range errs {
			if strings.Contains(e.Error(), want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected error mentioning %q", want)
		}
	}
}

// TestUsageFlushValidation_NegativeValues verifies the usage flush validation negative values contract.
// Asserts that expected multiple errors, got :.
func TestUsageFlushValidation_NegativeValues(t *testing.T) {
	t.Parallel()
	cfg := UsageFlushConfig{Interval: -1, AdaptiveThreshold: 2.0, FastInterval: -1}
	errs := cfg.setDefaultsAndValidate()
	if len(errs) < 3 {
		t.Errorf("expected multiple errors, got %d: %v", len(errs), errs)
	}
}

// TestReconcileDefaultInterval verifies the reconcile default interval contract.
// Asserts that unexpected error:.
func TestReconcileDefaultInterval(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Reconcile = ReconcileConfig{Enabled: true}
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Reconcile.Interval != 24*time.Hour {
		t.Errorf("Reconcile.Interval = %v, want 24h", cfg.Reconcile.Interval)
	}
}

// -------------------------------------------------------------------------
// CACHE CONFIG
// -------------------------------------------------------------------------

// TestCacheConfig_Defaults verifies the cache config defaults contract.
// Asserts that unexpected error:.
func TestCacheConfig_Defaults(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Cache = CacheConfig{Enabled: true}
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Cache.TTL != 5*time.Minute {
		t.Errorf("TTL = %v, want 5m", cfg.Cache.TTL)
	}
	if cfg.Cache.MaxSizeBytes != 256*1024*1024 {
		t.Errorf("MaxSizeBytes = %d, want %d", cfg.Cache.MaxSizeBytes, 256*1024*1024)
	}
	if cfg.Cache.MaxObjectSizeBytes != 10*1024*1024 {
		t.Errorf("MaxObjectSizeBytes = %d, want %d", cfg.Cache.MaxObjectSizeBytes, 10*1024*1024)
	}
}

// TestCacheConfig_CustomValues verifies the cache config custom values contract.
// Asserts that unexpected error:.
func TestCacheConfig_CustomValues(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Cache = CacheConfig{
		Enabled:       true,
		MaxSize:       "1GB",
		MaxObjectSize: "50MB",
		TTL:           10 * time.Minute,
	}
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Cache.MaxSizeBytes != 1024*1024*1024 {
		t.Errorf("MaxSizeBytes = %d, want %d", cfg.Cache.MaxSizeBytes, 1024*1024*1024)
	}
	if cfg.Cache.MaxObjectSizeBytes != 50*1024*1024 {
		t.Errorf("MaxObjectSizeBytes = %d, want %d", cfg.Cache.MaxObjectSizeBytes, 50*1024*1024)
	}
}

// TestCacheConfig_Disabled verifies the cache config disabled contract.
// Asserts that unexpected error:.
func TestCacheConfig_Disabled(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Cache = CacheConfig{Enabled: false}
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Disabled cache should not parse sizes
	if cfg.Cache.MaxSizeBytes != 0 {
		t.Errorf("disabled cache should have zero MaxSizeBytes, got %d", cfg.Cache.MaxSizeBytes)
	}
}

// TestCacheConfig_MaxObjectExceedsMaxSize verifies the cache config max object exceeds max size path by exercising cfg.SetDefaultsAndValidate.
func TestCacheConfig_MaxObjectExceedsMaxSize(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Cache = CacheConfig{
		Enabled:       true,
		MaxSize:       "100KB",
		MaxObjectSize: "1MB",
	}
	if err := cfg.SetDefaultsAndValidate(); err == nil {
		t.Error("expected error when max_object_size > max_size")
	}
}

// TestParseByteSize verifies the parse byte size contract.
// Asserts that parseByteSize():.
func TestParseByteSize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  int64
	}{
		{"1024", 1024},
		{"1KB", 1024},
		{"1kb", 1024},
		{"256MB", 256 * 1024 * 1024},
		{"1GB", 1024 * 1024 * 1024},
		{"100B", 100},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseByteSize(tt.input)
			if err != nil {
				t.Fatalf("parseByteSize(%q): %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("parseByteSize(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

// TestParseByteSize_Invalid verifies that malformed, empty, and out-of-range
// size strings return errors.
func TestParseByteSize_Invalid(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"", "abc", "10XB", "MB", " KB"} {
		t.Run(input, func(t *testing.T) {
			_, err := parseByteSize(input)
			if err == nil {
				t.Errorf("expected error for %q", input)
			}
		})
	}
}

// TestParseByteSize_Overflow verifies that values exceeding int64 return an
// error instead of silently wrapping to a negative number.
func TestParseByteSize_Overflow(t *testing.T) {
	t.Parallel()
	cases := []string{
		"9999999999999999999GB",
		"9223372036854775808", // math.MaxInt64 + 1
		"8589934592GB",        // 8GB * 1Gi overflows
	}
	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			_, err := parseByteSize(input)
			if err == nil {
				t.Errorf("expected overflow error for %q", input)
			}
		})
	}
}

// TestParseByteSize_Negative verifies the parse byte size negative behaviour described by the test name.
func TestParseByteSize_Negative(t *testing.T) {
	t.Parallel()
	_, err := parseByteSize("-1GB")
	if err == nil {
		t.Error("expected error for negative size")
	}
}

// TestLifecycleConfig_EmptyPrefixRejected verifies that a rule filtering on
// nothing at all is refused. An empty prefix is only acceptable when tags
// narrow the rule instead.
func TestLifecycleConfig_EmptyPrefixRejected(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Lifecycle = LifecycleConfig{
		Rules: []LifecycleRule{
			{Prefix: "", ExpirationDays: 30},
		},
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Fatal("expected validation error for a rule with no filter")
	}
	if !errors.Is(err, ErrLifecycleFilterRequired) {
		t.Errorf("error = %q, want ErrLifecycleFilterRequired", err)
	}
}

// TestLifecycleConfig_NegativeExpirationRejected verifies the lifecycle config negative expiration rejected path by exercising cfg.SetDefaultsAndValidate.
func TestLifecycleConfig_NegativeExpirationRejected(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Lifecycle = LifecycleConfig{
		Rules: []LifecycleRule{
			{Prefix: "tmp/", ExpirationDays: -1},
		},
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Fatal("expected validation error for negative expiration_days")
	}
}

// TestLifecycleConfig_ValidRulePasses verifies the lifecycle config valid rule passes contract.
// Asserts that valid lifecycle config should pass:.
func TestLifecycleConfig_ValidRulePasses(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Lifecycle = LifecycleConfig{
		Rules: []LifecycleRule{
			{Prefix: "tmp/", ExpirationDays: 30},
		},
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("valid lifecycle config should pass: %v", err)
	}
	if cfg.Lifecycle.BatchSize != 100 {
		t.Errorf("BatchSize = %d, want 100 (default)", cfg.Lifecycle.BatchSize)
	}
}

// TestDatabaseConfig_MinConnsExceedsMaxConns verifies the database config min conns exceeds max conns contract.
// Asserts that error should mention min_conns:.
func TestDatabaseConfig_MinConnsExceedsMaxConns(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Database.MinConns = 100
	cfg.Database.MaxConns = 10

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Fatal("min_conns > max_conns should fail validation")
	}
	if !strings.Contains(err.Error(), "min_conns") {
		t.Errorf("error should mention min_conns: %v", err)
	}
}

// TestRedisConfig_NegativeFailureThreshold verifies the redis config negative failure threshold contract.
// Asserts that negative failure_threshold should produce error, got:.
func TestRedisConfig_NegativeFailureThreshold(t *testing.T) {
	t.Parallel()
	r := &RedisConfig{
		Address:          "localhost:6379",
		FailureThreshold: -1,
	}
	errs := r.setDefaultsAndValidate()
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "failure_threshold") {
			found = true
		}
	}
	if !found {
		t.Errorf("negative failure_threshold should produce error, got: %v", errs)
	}
}

// TestRedisConfig_NegativeOpenTimeout verifies the redis config negative open timeout contract.
// Asserts that negative open_timeout should produce error, got:.
func TestRedisConfig_NegativeOpenTimeout(t *testing.T) {
	t.Parallel()
	r := &RedisConfig{
		Address:     "localhost:6379",
		OpenTimeout: -5 * time.Second,
	}
	errs := r.setDefaultsAndValidate()
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "open_timeout") {
			found = true
		}
	}
	if !found {
		t.Errorf("negative open_timeout should produce error, got: %v", errs)
	}
}

// TestRateLimitConfig_CIDRValidatedWhenDisabled verifies the rate limit config cidrvalidated when disabled contract.
// Asserts that invalid CIDR should be caught even when disabled, got:.
func TestRateLimitConfig_CIDRValidatedWhenDisabled(t *testing.T) {
	t.Parallel()
	r := &RateLimitConfig{
		Enabled:        false,
		TrustedProxies: []string{"not-a-cidr"},
	}
	errs := r.setDefaultsAndValidate()
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "invalid CIDR") {
			found = true
		}
	}
	if !found {
		t.Errorf("invalid CIDR should be caught even when disabled, got: %v", errs)
	}
}

// TestConfigValidation_DuplicateTokensAcrossBuckets verifies a proxy token
// claimed by two buckets fails validation. The token selects the bucket a
// request is authorized against, so allowing it would let a credential issued
// for one namespace resolve to another.
func TestConfigValidation_DuplicateTokensAcrossBuckets(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Buckets = []BucketConfig{
		{Name: "b1", Credentials: []CredentialConfig{{Token: "SAME"}}},
		{Name: "b2", Credentials: []CredentialConfig{{Token: "SAME"}}},
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Fatal("duplicate proxy tokens across buckets should fail validation")
	}
	if !strings.Contains(err.Error(), "duplicate proxy token") {
		t.Errorf("error should mention duplicate proxy token, got: %v", err)
	}
	if strings.Contains(err.Error(), "SAME") {
		t.Error("the token is a secret and must not appear in the error")
	}
}

// TestConfigValidation_DuplicateTokensWithinBucket verifies the check spans
// credentials inside one bucket too, not just across bucket boundaries.
func TestConfigValidation_DuplicateTokensWithinBucket(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Buckets = []BucketConfig{
		{Name: "b1", Credentials: []CredentialConfig{{Token: "SAME"}, {Token: "SAME"}}},
	}

	if err := cfg.SetDefaultsAndValidate(); err == nil {
		t.Error("duplicate proxy tokens within a bucket should fail validation")
	}
}

// TestConfigValidation_DistinctTokensAccepted verifies unique tokens still
// validate, so the new check does not reject working configurations.
func TestConfigValidation_DistinctTokensAccepted(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Buckets = []BucketConfig{
		{Name: "b1", Credentials: []CredentialConfig{{Token: "one"}}},
		{Name: "b2", Credentials: []CredentialConfig{{Token: "two"}}},
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("distinct tokens must validate, got: %v", err)
	}
}

// TestConfigValidation_EmptyTokensAccepted verifies buckets authenticating by
// SigV4 alone are unaffected: an absent token is not a duplicate of another
// absent token.
func TestConfigValidation_EmptyTokensAccepted(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Buckets = []BucketConfig{
		{Name: "b1", Credentials: []CredentialConfig{{AccessKeyID: "A1", SecretAccessKey: "s1"}}},
		{Name: "b2", Credentials: []CredentialConfig{{AccessKeyID: "A2", SecretAccessKey: "s2"}}},
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("credentials without tokens must validate, got: %v", err)
	}
}

// TestSpillDir_RejectedWhenNotAUsableDirectory pins the startup check. Without
// it a typo'd spill directory is discovered on the first object too large to
// hold in memory, which is the worst moment for writes to start failing.
func TestSpillDir_RejectedWhenNotAUsableDirectory(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	for name, dir := range map[string]string{
		"missing":   filepath.Join(t.TempDir(), "no-such-directory"),
		"not-a-dir": file,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := validBaseConfig()
			cfg.Server.SpillDir = dir
			err := cfg.SetDefaultsAndValidate()
			if !errors.Is(err, ErrSpillDirUnusable) {
				t.Errorf("err = %v, want ErrSpillDirUnusable", err)
			}
		})
	}
}

// TestSpillDir_EmptyIsAccepted holds that the knob stays optional: a config
// that never mentions it keeps the OS temp directory rather than failing.
func TestSpillDir_EmptyIsAccepted(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("config without spill_dir should validate, got %v", err)
	}
}

// TestBackendHTTP_DefaultsPreserveTheOldFixedValues holds the compatibility
// promise: a config that never mentions the http block is dialled exactly as it
// was before the block existed.
func TestBackendHTTP_DefaultsPreserveTheOldFixedValues(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("SetDefaultsAndValidate: %v", err)
	}

	got := cfg.Backends[0].HTTP
	if got.MaxIdleConns != DefaultMaxIdleConns ||
		got.MaxIdleConnsPerHost != DefaultMaxIdleConnsPerHost ||
		got.MaxConnsPerHost != DefaultMaxConnsPerHost ||
		got.ResponseHeaderTimeout != DefaultResponseHeaderTimeout {
		t.Errorf("http = %+v, want the documented defaults", got)
	}
	if !got.HTTP2Enabled() {
		t.Error("HTTP/2 should be attempted when force_http2 is unset")
	}
}

// TestBackendHTTP_ExplicitValuesSurviveDefaulting asserts a configured value is
// not overwritten by the default, which cmp.Or would do for anything the
// operator deliberately set to a smaller number.
func TestBackendHTTP_ExplicitValuesSurviveDefaulting(t *testing.T) {
	t.Parallel()
	off := false
	cfg := validBaseConfig()
	cfg.Backends[0].HTTP = BackendHTTPConfig{
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   2,
		MaxConnsPerHost:       8,
		ResponseHeaderTimeout: time.Second,
		ForceHTTP2:            &off,
	}
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("SetDefaultsAndValidate: %v", err)
	}

	got := cfg.Backends[0].HTTP
	if got.MaxIdleConns != 4 || got.MaxIdleConnsPerHost != 2 || got.MaxConnsPerHost != 8 {
		t.Errorf("pool sizes = %d/%d/%d, want 4/2/8",
			got.MaxIdleConns, got.MaxIdleConnsPerHost, got.MaxConnsPerHost)
	}
	if got.ResponseHeaderTimeout != time.Second {
		t.Errorf("ResponseHeaderTimeout = %s, want 1s", got.ResponseHeaderTimeout)
	}
	if got.HTTP2Enabled() {
		t.Error("an explicit force_http2: false must survive defaulting")
	}
}

// TestBackendHTTP_RejectsNegativeValues covers the validation the acceptance
// criteria call for. Zero is legal and means "use the default"; negative is
// not, and no transport field accepts it.
func TestBackendHTTP_RejectsNegativeValues(t *testing.T) {
	t.Parallel()
	for name, http := range map[string]BackendHTTPConfig{
		"max_idle_conns":          {MaxIdleConns: -1},
		"max_idle_conns_per_host": {MaxIdleConnsPerHost: -1},
		"max_conns_per_host":      {MaxConnsPerHost: -1},
		"response_header_timeout": {ResponseHeaderTimeout: -time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := validBaseConfig()
			cfg.Backends[0].HTTP = http
			if err := cfg.SetDefaultsAndValidate(); !errors.Is(err, ErrNegativeHTTPSetting) {
				t.Errorf("err = %v, want ErrNegativeHTTPSetting", err)
			}
		})
	}
}
