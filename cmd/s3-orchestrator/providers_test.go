// -------------------------------------------------------------------------------
// DI Provider Tests - Injector Smoke Tests
//
// Author: Alex Freidah
//
// Smoke tests that verify the DI injector resolves services correctly with
// various config combinations. These catch registration errors and dependency
// cycles at test time rather than runtime. Infrastructure services (PostgreSQL,
// S3 backends) are provided as eager values to avoid real connections.
// -------------------------------------------------------------------------------

package main

import (
	"testing"
	"time"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
	"github.com/afreidah/s3-orchestrator/internal/store"
)

// testConfig returns a minimal valid config for DI testing.
func testConfig() *config.Config {
	return &config.Config{
		Server: config.ServerConfig{
			ListenAddr:     ":0",
			BackendTimeout: 30 * time.Second,
		},
		Database: config.DatabaseConfig{
			Host:     "localhost",
			Database: "test",
			User:     "test",
		},
		Backends: []config.BackendConfig{
			{
				Name:           "b1",
				Endpoint:       "http://localhost:9000",
				Region:         "us-east-1",
				Bucket:         "test",
				AccessKeyID:    "AK",
				SecretAccessKey: "SK",
			},
		},
		Buckets: []config.BucketConfig{
			{
				Name: "test-bucket",
				Credentials: []config.CredentialConfig{
					{AccessKeyID: "AK", SecretAccessKey: "SK"},
				},
			},
		},
		RoutingStrategy: config.RoutingPack,
		CircuitBreaker:  config.CircuitBreakerConfig{FailureThreshold: 3, OpenTimeout: 15 * time.Second, CacheTTL: 60 * time.Second},
		UI: config.UIConfig{
			Enabled:       true,
			AdminKey:      "admin",
			AdminSecret:   "secret",
			SessionSecret: "session",
		},
		RateLimit: config.RateLimitConfig{
			Enabled:        true,
			RequestsPerSec: 100,
			Burst:          200,
		},
	}
}

func TestInjector_ResolvesBackends(t *testing.T) {
	cfg := testConfig()
	inj := do.New()
	do.ProvideValue(inj, cfg)
	do.Provide(inj, ProvideBackends)

	br, err := do.Invoke[*backendsResult](inj)
	if err != nil {
		t.Fatalf("ProvideBackends: %v", err)
	}
	if len(br.Backends) != 1 {
		t.Errorf("expected 1 backend, got %d", len(br.Backends))
	}
	if br.Order[0] != "b1" {
		t.Errorf("order[0] = %q, want b1", br.Order[0])
	}
}

func TestInjector_ResolvesBucketAuth(t *testing.T) {
	cfg := testConfig()
	inj := do.New()
	do.ProvideValue(inj, cfg)
	do.Provide(inj, ProvideBucketAuth)

	br, err := do.Invoke[*auth.BucketRegistry](inj)
	if err != nil {
		t.Fatalf("ProvideBucketAuth: %v", err)
	}
	if br == nil {
		t.Fatal("BucketRegistry is nil")
	}
}

func TestInjector_ResolvesRateLimiter(t *testing.T) {
	cfg := testConfig()
	inj := do.New()
	do.ProvideValue(inj, cfg)
	do.Provide(inj, ProvideRateLimiter)

	rl, err := do.Invoke[*s3api.RateLimiter](inj)
	if err != nil {
		t.Fatalf("ProvideRateLimiter: %v", err)
	}
	if rl == nil {
		t.Fatal("RateLimiter is nil")
	}
}

func TestInjector_ResolvesLoginThrottle(t *testing.T) {
	inj := do.New()
	do.Provide(inj, ProvideLoginThrottle)

	lt, err := do.Invoke[*httputil.LoginThrottle](inj)
	if err != nil {
		t.Fatalf("ProvideLoginThrottle: %v", err)
	}
	if lt == nil {
		t.Fatal("LoginThrottle is nil")
	}
	lt.Close()
}

func TestInjector_OptionalServiceAbsent(t *testing.T) {
	// When encryption is not registered, Invoke returns an error
	inj := do.New()
	do.ProvideValue(inj, testConfig())

	_, err := do.Invoke[*encryption.Encryptor](inj)
	if err == nil {
		t.Fatal("expected error when encryption not registered")
	}
}

func TestInjector_BackendManagerWithOptionalDeps(t *testing.T) {
	cfg := testConfig()
	inj := do.New()
	do.ProvideValue(inj, cfg)
	do.Provide(inj, ProvideBackends)

	// Provide a mock CB store — BackendManager needs it
	mockStore := &mockMetadataStore{}
	cbStore := store.NewCircuitBreakerStore(mockStore, cfg.CircuitBreaker)
	do.ProvideValue(inj, cbStore)

	do.Provide(inj, ProvideBackendManager)

	// No encryption, no Redis, no cache registered — should still resolve
	mgr, err := do.Invoke[*proxy.BackendManager](inj)
	if err != nil {
		t.Fatalf("ProvideBackendManager: %v", err)
	}
	if mgr == nil {
		t.Fatal("BackendManager is nil")
	}
	mgr.Close()
}

func TestInjector_LifecycleManagerRegistersServices(t *testing.T) {
	cfg := testConfig()
	inj := do.New()
	do.ProvideValue(inj, cfg)
	do.ProvideValue(inj, "all")
	do.Provide(inj, ProvideBackends)

	mockStore := &mockMetadataStore{}
	cbStore := store.NewCircuitBreakerStore(mockStore, cfg.CircuitBreaker)
	do.ProvideValue(inj, cbStore)

	do.Provide(inj, ProvideBackendManager)
	do.Provide(inj, ProvideLifecycleManager)

	sm, err := do.Invoke[*lifecycle.Manager](inj)
	if err != nil {
		t.Fatalf("ProvideLifecycleManager: %v", err)
	}
	if sm == nil {
		t.Fatal("lifecycle.Manager is nil")
	}
}

// mockMetadataStore is a minimal mock for DI smoke tests. Methods panic
// if called — these tests verify wiring, not behavior.
type mockMetadataStore struct{ store.MetadataStore }
