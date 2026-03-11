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
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigValidation_MinimalValid(t *testing.T) {
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

func TestConfigValidation_MissingRequired(t *testing.T) {
	cfg := Config{}
	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("empty config should fail validation")
	}
}

func TestConfigValidation_DuplicateBackendNames(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Backends = []BackendConfig{
		{Name: "dup", Endpoint: "e", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s", QuotaBytes: 1},
		{Name: "dup", Endpoint: "e", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s", QuotaBytes: 1},
	}

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("duplicate backend names should fail validation")
	}
}

func TestConfigValidation_NegativeQuota(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Backends[0].QuotaBytes = -1

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("negative quota should fail validation")
	}
}

func TestConfigValidation_NegativeMaxConcurrentRequests(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Server.MaxConcurrentRequests = -1

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("negative max_concurrent_requests should fail validation")
	}
}

func TestConfigValidation_NegativeMaxConcurrentReads(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Server.MaxConcurrentReads = -1

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("negative max_concurrent_reads should fail validation")
	}
}

func TestConfigValidation_NegativeMaxConcurrentWrites(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Server.MaxConcurrentWrites = -1

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("negative max_concurrent_writes should fail validation")
	}
}

func TestConfigValidation_InvalidLoadShedThreshold(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Server.LoadShedThreshold = 1.5

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("load_shed_threshold >= 1.0 should fail validation")
	}
}

func TestConfigValidation_NegativeLoadShedThreshold(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Server.LoadShedThreshold = -0.5

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("negative load_shed_threshold should fail validation")
	}
}

func TestConfigValidation_ValidLoadShedThreshold(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Server.LoadShedThreshold = 0.8

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("valid load_shed_threshold 0.8 should pass: %v", err)
	}
}

func TestConfigValidation_NegativeAdmissionWait(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Server.AdmissionWait = -1 * time.Second

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("negative admission_wait should fail validation")
	}
}

func TestConfigValidation_ZeroMaxConcurrentRequestsMeansUnlimited(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Server.MaxConcurrentRequests = 0

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("zero max_concurrent_requests (unlimited) should pass validation: %v", err)
	}
}

func TestConfigValidation_ZeroQuotaMeansUnlimited(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Backends[0].QuotaBytes = 0

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("zero quota (unlimited) should pass validation: %v", err)
	}
}

func TestConfigValidation_OmittedQuotaMeansUnlimited(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Backends[0].QuotaBytes = 0

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("omitted quota (unlimited) should pass validation: %v", err)
	}
}

func TestConnectionString(t *testing.T) {
	db := DatabaseConfig{
		Host:     "localhost",
		Port:     5433,
		Database: "s3proxy",
		User:     "s3proxy",
		Password: "secret",
		SSLMode:  "require",
	}

	got := db.ConnectionString()
	want := "postgres://s3proxy:secret@localhost:5433/s3proxy?sslmode=require"
	if got != want {
		t.Errorf("ConnectionString() = %q, want %q", got, want)
	}
}

func TestConnectionString_SpecialChars(t *testing.T) {
	db := DatabaseConfig{
		Host:     "db.example.com",
		Port:     5432,
		Database: "mydb",
		User:     "admin",
		Password: "p@ss=w ord&special",
		SSLMode:  "disable",
	}

	got := db.ConnectionString()
	// url.UserPassword percent-encodes @ but preserves = and &
	want := "postgres://admin:p%40ss=w%20ord&special@db.example.com:5432/mydb?sslmode=disable"
	if got != want {
		t.Errorf("ConnectionString() = %q, want %q", got, want)
	}
}

func TestRebalanceConfig_Defaults(t *testing.T) {
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

func TestRebalanceConfig_InvalidStrategy(t *testing.T) {
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

func TestRebalanceConfig_DisabledSkipsValidation(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Rebalance = RebalanceConfig{
		Enabled:  false,
		Strategy: "garbage",
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("disabled rebalance should skip validation: %v", err)
	}
}

func TestRebalanceConfig_InvalidThreshold(t *testing.T) {
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

func TestReplicationConfig_DefaultsWhenDisabled(t *testing.T) {
	cfg := validBaseConfig()
	// factor=0 should default to 1 (disabled)

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("disabled replication should pass: %v", err)
	}

	if cfg.Replication.Factor != 1 {
		t.Errorf("factor default = %d, want 1", cfg.Replication.Factor)
	}
}

func TestReplicationConfig_DefaultsWhenEnabled(t *testing.T) {
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

func TestReplicationConfig_FactorExceedsBackends(t *testing.T) {
	cfg := validBaseConfig() // 1 backend
	cfg.Replication = ReplicationConfig{Factor: 2}

	if err := cfg.SetDefaultsAndValidate(); err == nil {
		t.Error("factor > backends should fail validation")
	}
}

func TestReplicationConfig_FactorNegative(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Replication = ReplicationConfig{Factor: -1}

	if err := cfg.SetDefaultsAndValidate(); err == nil {
		t.Error("negative factor should fail validation")
	}
}

func TestReplicationConfig_DisabledSkipsValidation(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Replication = ReplicationConfig{Factor: 1, WorkerInterval: -1}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("factor=1 should skip interval validation: %v", err)
	}
}

func TestCircuitBreakerDefaults(t *testing.T) {
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

func TestCircuitBreakerConfig_ParallelBroadcastSet(t *testing.T) {
	cfg := validBaseConfig()
	cfg.CircuitBreaker.ParallelBroadcast = true

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("parallel_broadcast=true should pass: %v", err)
	}
	if !cfg.CircuitBreaker.ParallelBroadcast {
		t.Error("parallel_broadcast should be true when set")
	}
}

func TestServerTimeoutDefaults(t *testing.T) {
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

func TestServerTimeoutCustomValues(t *testing.T) {
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

func TestServerTimeoutCrossValidation(t *testing.T) {
	tests := []struct {
		name              string
		readHeaderTimeout time.Duration
		readTimeout       time.Duration
		backendTimeout    time.Duration
		writeTimeout      time.Duration
		wantErr           string
	}{
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
		t.Run(tt.name, func(t *testing.T) {
			cfg := validBaseConfig()
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

			err := cfg.SetDefaultsAndValidate()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want substring %q", err.Error(), tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestNonReloadableFieldsChanged_ServerTimeouts(t *testing.T) {
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

func TestShutdownDelayDefault(t *testing.T) {
	cfg := validBaseConfig()

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("valid config should pass: %v", err)
	}

	if cfg.Server.ShutdownDelay != 0 {
		t.Errorf("shutdown_delay default = %v, want 0", cfg.Server.ShutdownDelay)
	}
}

func TestShutdownDelayCustomValue(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Server.ShutdownDelay = 5 * time.Second

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("custom shutdown_delay should pass: %v", err)
	}

	if cfg.Server.ShutdownDelay != 5*time.Second {
		t.Errorf("shutdown_delay = %v, want 5s", cfg.Server.ShutdownDelay)
	}
}

func TestNonReloadableFieldsChanged_ShutdownDelay(t *testing.T) {
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

func TestNonReloadableFieldsChanged_CircuitBreaker(t *testing.T) {
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

func TestConfigValidation_MixedQuotaAndUnlimited(t *testing.T) {
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

func TestConfigValidation_MultipleUnlimitedWithoutReplication(t *testing.T) {
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

func TestConfigValidation_MultipleUnlimitedWithReplication(t *testing.T) {
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

func TestConfigValidation_QuotaBackendsWithReplication(t *testing.T) {
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

func TestConfigValidation_NegativeAPIRequestLimit(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Backends[0].APIRequestLimit = -1

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("negative api_request_limit should fail validation")
	}
}

func TestConfigValidation_NegativeEgressByteLimit(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Backends[0].EgressByteLimit = -1

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("negative egress_byte_limit should fail validation")
	}
}

func TestConfigValidation_NegativeIngressByteLimit(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Backends[0].IngressByteLimit = -1

	err := cfg.SetDefaultsAndValidate()
	if err == nil {
		t.Error("negative ingress_byte_limit should fail validation")
	}
}

func TestConfigValidation_ZeroUsageLimitsMeansUnlimited(t *testing.T) {
	cfg := validBaseConfig()
	// All zero — should pass (unlimited)
	cfg.Backends[0].APIRequestLimit = 0
	cfg.Backends[0].EgressByteLimit = 0
	cfg.Backends[0].IngressByteLimit = 0

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("zero usage limits (unlimited) should pass validation: %v", err)
	}
}

// -------------------------------------------------------------------------
// BUCKET VALIDATION TESTS
// -------------------------------------------------------------------------

func TestConfigValidation_NoBuckets(t *testing.T) {
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

func TestConfigValidation_DuplicateBucketNames(t *testing.T) {
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

func TestConfigValidation_DuplicateAccessKeysAcrossBuckets(t *testing.T) {
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

func TestConfigValidation_BucketMissingCredentials(t *testing.T) {
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

func TestConfigValidation_CredentialWithNoAuthMethod(t *testing.T) {
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

func TestConfigValidation_BucketNameWithSlash(t *testing.T) {
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

func TestConfigValidation_MultipleCredentialsOnSameBucket(t *testing.T) {
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

func TestConfigValidation_TokenCredential(t *testing.T) {
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

func TestConfigValidation_BucketMissingName(t *testing.T) {
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

// -------------------------------------------------------------------------
// NON-RELOADABLE FIELDS CHANGED TESTS
// -------------------------------------------------------------------------

func TestNonReloadableFieldsChanged_IdenticalConfigs(t *testing.T) {
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	if len(changed) != 0 {
		t.Errorf("identical configs should return empty slice, got %v", changed)
	}
}

func TestNonReloadableFieldsChanged_ListenAddr(t *testing.T) {
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

func TestNonReloadableFieldsChanged_MaxConcurrentRequests(t *testing.T) {
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

func TestNonReloadableFieldsChanged_MaxConcurrentReads(t *testing.T) {
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	b.Server.MaxConcurrentReads = 50
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	if len(changed) != 1 || changed[0] != "server.max_concurrent_reads" {
		t.Errorf("expected [server.max_concurrent_reads], got %v", changed)
	}
}

func TestNonReloadableFieldsChanged_MaxConcurrentWrites(t *testing.T) {
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	b.Server.MaxConcurrentWrites = 25
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	if len(changed) != 1 || changed[0] != "server.max_concurrent_writes" {
		t.Errorf("expected [server.max_concurrent_writes], got %v", changed)
	}
}

func TestNonReloadableFieldsChanged_LoadShedThreshold(t *testing.T) {
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

func TestNonReloadableFieldsChanged_AdmissionWait(t *testing.T) {
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

func TestNonReloadableFieldsChanged_Database(t *testing.T) {
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

func TestNonReloadableFieldsChanged_BackendStructuralFields(t *testing.T) {
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

func TestNonReloadableFieldsChanged_BackendCredentials(t *testing.T) {
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

func TestNonReloadableFieldsChanged_BackendCountChanged(t *testing.T) {
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

func TestNonReloadableFieldsChanged_ReloadableOnlyChanges(t *testing.T) {
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	_ = b.SetDefaultsAndValidate()

	// These are reloadable fields — should NOT appear in the result
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

func TestNonReloadableFieldsChanged_UnsignedPayloadChanged(t *testing.T) {
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

func TestNonReloadableFieldsChanged_UnsignedPayloadBothNil(t *testing.T) {
	a := validBaseConfig()
	b := validBaseConfig()
	// Both nil — should be treated as identical (both default to true)
	_ = a.SetDefaultsAndValidate()
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	if len(changed) != 0 {
		t.Errorf("both nil unsigned_payload should be identical, got %v", changed)
	}
}

func TestNonReloadableFieldsChanged_UnsignedPayloadExplicitTrue(t *testing.T) {
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

func TestNonReloadableFieldsChanged_DisableChecksumChanged(t *testing.T) {
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

func TestNonReloadableFieldsChanged_DisableChecksumBothTrue(t *testing.T) {
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

func TestNonReloadableFieldsChanged_DisableChecksumBothFalse(t *testing.T) {
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	if len(changed) != 0 {
		t.Errorf("both disable_checksum=false (default) should be identical, got %v", changed)
	}
}

func TestNonReloadableFieldsChanged_StripSDKHeadersChanged(t *testing.T) {
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

func TestNonReloadableFieldsChanged_StripSDKHeadersBothTrue(t *testing.T) {
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

func TestNonReloadableFieldsChanged_StripSDKHeadersBothFalse(t *testing.T) {
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	if len(changed) != 0 {
		t.Errorf("both strip_sdk_headers=false (default) should be identical, got %v", changed)
	}
}

func TestBoolDefault(t *testing.T) {
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

func TestConfigValidation_TLS_CertWithoutKey(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Server.TLS.CertFile = "/etc/cert.pem"
	err := cfg.SetDefaultsAndValidate()
	if err == nil || !strings.Contains(err.Error(), "both cert_file and key_file") {
		t.Errorf("expected cert+key pair error, got %v", err)
	}
}

func TestConfigValidation_TLS_KeyWithoutCert(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Server.TLS.KeyFile = "/etc/key.pem"
	err := cfg.SetDefaultsAndValidate()
	if err == nil || !strings.Contains(err.Error(), "both cert_file and key_file") {
		t.Errorf("expected cert+key pair error, got %v", err)
	}
}

func TestConfigValidation_TLS_ValidPair(t *testing.T) {
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

func TestConfigValidation_TLS_InvalidMinVersion(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Server.TLS.CertFile = "/etc/cert.pem"
	cfg.Server.TLS.KeyFile = "/etc/key.pem"
	cfg.Server.TLS.MinVersion = "1.1"
	err := cfg.SetDefaultsAndValidate()
	if err == nil || !strings.Contains(err.Error(), "min_version") {
		t.Errorf("expected min_version error, got %v", err)
	}
}

func TestConfigValidation_TLS_MinVersion13(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Server.TLS.CertFile = "/etc/cert.pem"
	cfg.Server.TLS.KeyFile = "/etc/key.pem"
	cfg.Server.TLS.MinVersion = "1.3"
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("TLS 1.3 should be valid: %v", err)
	}
}

func TestConfigValidation_TLS_NoTLSIsValid(t *testing.T) {
	cfg := validBaseConfig()
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("no TLS config should pass: %v", err)
	}
}

func TestNonReloadableFieldsChanged_TLS(t *testing.T) {
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

func TestNonReloadableFieldsChanged_MultipleChanges(t *testing.T) {
	a := validBaseConfig()
	b := validBaseConfig()
	_ = a.SetDefaultsAndValidate()

	b.Server.ListenAddr = ":8080"
	b.Database.Host = "newhost"
	b.RoutingStrategy = "spread"
	_ = b.SetDefaultsAndValidate()

	changed := NonReloadableFieldsChanged(&a, &b)
	if len(changed) < 3 {
		t.Errorf("expected at least 3 changed fields, got %v", changed)
	}
}

// -------------------------------------------------------------------------
// USAGE FLUSH CONFIG TESTS
// -------------------------------------------------------------------------

func TestUsageFlushConfig_Defaults(t *testing.T) {
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

func TestUsageFlushConfig_CustomValues(t *testing.T) {
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

func TestUsageFlushConfig_FastIntervalExceedsInterval(t *testing.T) {
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

func TestUsageFlushConfig_InvalidThreshold(t *testing.T) {
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

func TestLifecycleConfig_EmptyRulesValid(t *testing.T) {
	cfg := validBaseConfig()
	// No lifecycle rules — should be valid (disabled)
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("empty lifecycle rules should pass: %v", err)
	}
}

func TestLifecycleConfig_ValidRules(t *testing.T) {
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

func TestLifecycleConfig_MissingPrefix(t *testing.T) {
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
	if !strings.Contains(err.Error(), "prefix is required") {
		t.Errorf("error should mention prefix, got: %v", err)
	}
}

func TestLifecycleConfig_ZeroExpirationDays(t *testing.T) {
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

func TestLifecycleConfig_NegativeExpirationDays(t *testing.T) {
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

func TestLifecycleConfig_DuplicatePrefix(t *testing.T) {
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
	if !strings.Contains(err.Error(), "duplicate prefix") {
		t.Errorf("error should mention duplicate prefix, got: %v", err)
	}
}

// -------------------------------------------------------------------------
// RATE LIMIT CONFIG TESTS
// -------------------------------------------------------------------------

func TestRateLimitConfig_Defaults(t *testing.T) {
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

func TestRateLimitConfig_DisabledSkipsValidation(t *testing.T) {
	cfg := validBaseConfig()
	cfg.RateLimit = RateLimitConfig{Enabled: false, RequestsPerSec: -1, Burst: -1}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("disabled rate limit should skip validation: %v", err)
	}
}

// -------------------------------------------------------------------------
// ROUTING STRATEGY TESTS
// -------------------------------------------------------------------------

func TestRoutingStrategy_DefaultsPack(t *testing.T) {
	cfg := validBaseConfig()

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("valid config should pass: %v", err)
	}

	if cfg.RoutingStrategy != "pack" {
		t.Errorf("routing_strategy default = %q, want \"pack\"", cfg.RoutingStrategy)
	}
}

func TestRoutingStrategy_InvalidValue(t *testing.T) {
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

func TestTracingConfig_EnabledWithoutEndpoint(t *testing.T) {
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

func TestTracingConfig_EnabledWithEndpoint(t *testing.T) {
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

func TestTracingConfig_DisabledSkipsEndpointValidation(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Telemetry.Tracing = TracingConfig{
		Enabled: false,
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("disabled tracing should skip endpoint validation: %v", err)
	}
}

func TestMetricsConfig_DefaultPath(t *testing.T) {
	cfg := validBaseConfig()

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("valid config should pass: %v", err)
	}

	if cfg.Telemetry.Metrics.Path != "/metrics" {
		t.Errorf("metrics path default = %q, want \"/metrics\"", cfg.Telemetry.Metrics.Path)
	}
}

// -------------------------------------------------------------------------
// LOADCONFIG TESTS
// -------------------------------------------------------------------------

func TestLoadConfig_ValidFile(t *testing.T) {
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

func TestLoadConfig_DisableChecksum(t *testing.T) {
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

func TestLoadConfig_DisableChecksumDefaultFalse(t *testing.T) {
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

func TestLoadConfig_StripSDKHeaders(t *testing.T) {
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

func TestLoadConfig_StripSDKHeadersDefaultFalse(t *testing.T) {
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

func TestLoadConfig_NonexistentFile(t *testing.T) {
	_, err := LoadConfig("/tmp/nonexistent-config-file-abc123.yaml")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
	if !strings.Contains(err.Error(), "read config file") {
		t.Errorf("error should mention reading file, got: %v", err)
	}
}

func TestLoadConfig_InvalidYAML(t *testing.T) {
	path := writeTempConfig(t, "{{invalid yaml")

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
	if !strings.Contains(err.Error(), "parse config") {
		t.Errorf("error should mention parsing, got: %v", err)
	}
}

func TestLoadConfig_ValidationFailure(t *testing.T) {
	// Valid YAML but fails validation (missing required fields)
	path := writeTempConfig(t, "server:\n  listen_addr: \"\"\n")

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "invalid config") {
		t.Errorf("error should mention invalid config, got: %v", err)
	}
}

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

func TestUIConfig_EnabledMissingCredentials(t *testing.T) {
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

func TestUIConfig_EnabledWithCredentials(t *testing.T) {
	cfg := validBaseConfig()
	cfg.UI = UIConfig{Enabled: true, AdminKey: "key", AdminSecret: "secret"}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("valid UI config should pass: %v", err)
	}
	if cfg.UI.Path != "/ui" {
		t.Errorf("UI.Path = %q, want /ui (default)", cfg.UI.Path)
	}
}

func TestUIConfig_DisabledSkipsValidation(t *testing.T) {
	cfg := validBaseConfig()
	cfg.UI = UIConfig{Enabled: false}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("disabled UI should skip credential validation: %v", err)
	}
}

func TestUIConfig_SessionSecret(t *testing.T) {
	cfg := validBaseConfig()
	cfg.UI = UIConfig{
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

func TestLogLevel_DefaultsToInfo(t *testing.T) {
	cfg := validBaseConfig()
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Server.LogLevel != "info" {
		t.Errorf("log_level default = %q, want %q", cfg.Server.LogLevel, "info")
	}
}

func TestLogLevel_CustomValue(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error"} {
		cfg := validBaseConfig()
		cfg.Server.LogLevel = level
		if err := cfg.SetDefaultsAndValidate(); err != nil {
			t.Errorf("log_level %q should be valid: %v", level, err)
		}
	}
}

func TestLogLevel_InvalidValue(t *testing.T) {
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

func TestBackendCircuitBreakerDefaults(t *testing.T) {
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

func TestBackendCircuitBreakerDefaults_Disabled(t *testing.T) {
	cfg := validBaseConfig()
	// Disabled (default) — defaults should NOT be applied
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BackendCircuitBreaker.FailureThreshold != 0 {
		t.Errorf("FailureThreshold should stay 0 when disabled, got %d", cfg.BackendCircuitBreaker.FailureThreshold)
	}
}

func TestBackendCircuitBreakerDefaults_CustomValues(t *testing.T) {
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

func TestNonReloadableFieldsChanged_BackendCircuitBreaker(t *testing.T) {
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

func TestRedisConfig_MissingAddress(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Redis = &RedisConfig{}

	err := cfg.SetDefaultsAndValidate()
	if err == nil || !strings.Contains(err.Error(), "redis.address is required") {
		t.Errorf("missing redis address should fail, got: %v", err)
	}
}

func TestRedisConfig_Defaults(t *testing.T) {
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

func TestRedisConfig_CustomValues(t *testing.T) {
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

func TestNonReloadableFieldsChanged_RedisAdded(t *testing.T) {
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

func TestNonReloadableFieldsChanged_RedisRemoved(t *testing.T) {
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

func TestNonReloadableFieldsChanged_RedisModified(t *testing.T) {
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

func TestNonReloadableFieldsChanged_RedisBothNil(t *testing.T) {
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

func TestNonReloadableFieldsChanged_RedisIdentical(t *testing.T) {
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

func TestEncryptionConfig_ValidMasterKey(t *testing.T) {
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

func TestEncryptionConfig_CustomChunkSize(t *testing.T) {
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

func TestEncryptionConfig_ChunkSizeTooSmall(t *testing.T) {
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

func TestEncryptionConfig_ChunkSizeTooLarge(t *testing.T) {
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

func TestEncryptionConfig_ChunkSizeNotPowerOf2(t *testing.T) {
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

func TestEncryptionConfig_NoKeySource(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Encryption = EncryptionConfig{Enabled: true}

	err := cfg.SetDefaultsAndValidate()
	if err == nil || !strings.Contains(err.Error(), "exactly one of master_key") {
		t.Errorf("missing key source should fail, got: %v", err)
	}
}

func TestEncryptionConfig_MultipleKeySources(t *testing.T) {
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

func TestEncryptionConfig_InvalidBase64MasterKey(t *testing.T) {
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

func TestEncryptionConfig_WrongKeyLength(t *testing.T) {
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

func TestEncryptionConfig_InvalidPreviousKey(t *testing.T) {
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

func TestEncryptionConfig_PreviousKeyWrongLength(t *testing.T) {
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

func TestEncryptionConfig_VaultMissingFields(t *testing.T) {
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
	for _, want := range []string{"vault.address", "vault.token", "vault.key_name"} {
		if !strings.Contains(errStr, want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

func TestEncryptionConfig_VaultDefaultMountPath(t *testing.T) {
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

func TestEncryptionConfig_DisabledSkipsValidation(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Encryption = EncryptionConfig{
		Enabled:   false,
		MasterKey: "garbage",
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("disabled encryption should skip validation: %v", err)
	}
}

func TestParseLogLevel(t *testing.T) {
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
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
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
