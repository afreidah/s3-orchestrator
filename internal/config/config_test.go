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
// HELPERS
// -------------------------------------------------------------------------

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
