// -------------------------------------------------------------------------------
// Dependency Injection Tests
//
// Author: Alex Freidah
//
// Pins the contract that every Provider returns a clean
// do.ErrServiceNotFound (never panics) when its required injector entries
// are absent, and that leaf providers with no upstream dependencies
// construct successfully from an empty injector plus a minimal config.
// -------------------------------------------------------------------------------

package di

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// -------------------------------------------------------------------------
// MISSING-DEPENDENCY SAFETY NET
// -------------------------------------------------------------------------

// TestProviders_MissingConfigReturnsCleanError guarantees that every
// Provider surfaces do.ErrServiceNotFound (not a panic) when called with a
// bare injector. Pins the post-#564 invariant: no provider uses
// do.MustInvoke internally.
func TestProviders_MissingConfigReturnsCleanError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		call func(do.Injector) error
	}{
		{"ConcreteStore", func(i do.Injector) error { _, err := ProvideConcreteStore(i); return err }},
		{"AdminStore", func(i do.Injector) error { _, err := ProvideAdminStore(i); return err }},
		{"DatabaseBreaker", func(i do.Injector) error { _, err := ProvideDatabaseBreaker(i); return err }},
		{"ObjectStore", func(i do.Injector) error { _, err := ProvideObjectStore(i); return err }},
		{"QuotaStore", func(i do.Injector) error { _, err := ProvideQuotaStore(i); return err }},
		{"MultipartStore", func(i do.Injector) error { _, err := ProvideMultipartStore(i); return err }},
		{"ReplicationStore", func(i do.Injector) error { _, err := ProvideReplicationStore(i); return err }},
		{"CleanupStore", func(i do.Injector) error { _, err := ProvideCleanupStore(i); return err }},
		{"IntegrityStore", func(i do.Injector) error { _, err := ProvideIntegrityStore(i); return err }},
		{"LifecycleStore", func(i do.Injector) error { _, err := ProvideLifecycleStore(i); return err }},
		{"BackendLifecycleStore", func(i do.Injector) error { _, err := ProvideBackendLifecycleStore(i); return err }},
		{"DashboardStore", func(i do.Injector) error { _, err := ProvideDashboardStore(i); return err }},
		{"UsageFlusher", func(i do.Injector) error { _, err := ProvideUsageFlusher(i); return err }},
		{"AdvisoryLocker", func(i do.Injector) error { _, err := ProvideAdvisoryLocker(i); return err }},
		{"MetricsDeps", func(i do.Injector) error { _, err := ProvideMetricsDeps(i); return err }},
		{"Backends", func(i do.Injector) error { _, err := ProvideBackends(i); return err }},
		{"Encryptor", func(i do.Injector) error { _, err := ProvideEncryptor(i); return err }},
		{"EncryptionProvider", func(i do.Injector) error { _, err := ProvideEncryptionProvider(i); return err }},
		{"RedisCounterBackend", func(i do.Injector) error { _, err := ProvideRedisCounterBackend(i); return err }},
		{"ObjectCache", func(i do.Injector) error { _, err := ProvideObjectCache(i); return err }},
		{"BackendManager", func(i do.Injector) error { _, err := ProvideBackendManager(i); return err }},
		{"LifecycleManager", func(i do.Injector) error { _, err := ProvideLifecycleManager(i); return err }},
		{"BucketAuth", func(i do.Injector) error { _, err := ProvideBucketAuth(i); return err }},
		{"S3Server", func(i do.Injector) error { _, err := ProvideS3Server(i); return err }},
		{"RateLimiter", func(i do.Injector) error { _, err := ProvideRateLimiter(i); return err }},
		{"UIHandler", func(i do.Injector) error { _, err := ProvideUIHandler(i); return err }},
		{"AdminHandler", func(i do.Injector) error { _, err := ProvideAdminHandler(i); return err }},
		{"Notifier", func(i do.Injector) error { _, err := ProvideNotifier(i); return err }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s panicked with bare injector: %v", tc.name, r)
				}
			}()
			err := tc.call(do.New())
			if err == nil {
				t.Fatalf("%s returned nil error with bare injector", tc.name)
			}
			if !errors.Is(err, do.ErrServiceNotFound) {
				t.Fatalf("%s: expected ErrServiceNotFound, got %v", tc.name, err)
			}
		})
	}
}

// -------------------------------------------------------------------------
// LEAF-PROVIDER HAPPY PATHS
// -------------------------------------------------------------------------

// TestProvideLogBuffer exercises the trivial log-buffer factory.
func TestProvideLogBuffer(t *testing.T) {
	t.Parallel()
	lb, err := ProvideLogBuffer(do.New())
	if err != nil {
		t.Fatalf("ProvideLogBuffer: %v", err)
	}
	if lb == nil {
		t.Fatal("ProvideLogBuffer returned nil buffer")
	}
}

// TestProvideLoginThrottle verifies the login-throttle constructor.
func TestProvideLoginThrottle(t *testing.T) {
	t.Parallel()
	lt, err := ProvideLoginThrottle(do.New())
	if err != nil {
		t.Fatalf("ProvideLoginThrottle: %v", err)
	}
	if lt == nil {
		t.Fatal("ProvideLoginThrottle returned nil")
	}
	lt.Close()
}

// TestProvideBucketAuth verifies the BucketRegistry is built from cfg.Buckets.
func TestProvideBucketAuth(t *testing.T) {
	t.Parallel()
	inj := do.New()
	do.ProvideValue(inj, &config.Config{
		Buckets: []config.BucketConfig{{Name: "test"}},
	})
	reg, err := ProvideBucketAuth(inj)
	if err != nil {
		t.Fatalf("ProvideBucketAuth: %v", err)
	}
	if reg == nil {
		t.Fatal("ProvideBucketAuth returned nil registry")
	}
}

// TestProvideRateLimiter verifies rate-limit config wiring.
func TestProvideRateLimiter(t *testing.T) {
	t.Parallel()
	inj := do.New()
	do.ProvideValue(inj, &config.Config{
		RateLimit: config.RateLimitConfig{
			Enabled:        true,
			RequestsPerSec: 100,
			Burst:          200,
		},
	})
	rl, err := ProvideRateLimiter(inj)
	if err != nil {
		t.Fatalf("ProvideRateLimiter: %v", err)
	}
	if rl == nil {
		t.Fatal("ProvideRateLimiter returned nil")
	}
	rl.Close()
}

// TestProvideDatabaseBreaker verifies the CB factory wires config values.
func TestProvideDatabaseBreaker(t *testing.T) {
	t.Parallel()
	inj := do.New()
	do.ProvideValue(inj, &config.Config{
		CircuitBreaker: config.CircuitBreakerConfig{FailureThreshold: 5, OpenTimeout: 10 * time.Second},
	})
	cb, err := ProvideDatabaseBreaker(inj)
	if err != nil {
		t.Fatalf("ProvideDatabaseBreaker: %v", err)
	}
	if cb == nil {
		t.Fatal("ProvideDatabaseBreaker returned nil")
	}
}

// -------------------------------------------------------------------------
// ERROR PATHS
// -------------------------------------------------------------------------

// TestProvideEncryptor_InvalidKey verifies the encryptor surfaces the
// underlying key-provider error when no key source is configured.
func TestProvideEncryptor_InvalidKey(t *testing.T) {
	t.Parallel()
	inj := do.New()
	do.ProvideValue(inj, &config.Config{
		Encryption: config.EncryptionConfig{
			Enabled:   true,
			ChunkSize: 65536,
		},
	})
	_, err := ProvideEncryptor(inj)
	if err == nil {
		t.Fatal("expected error from ProvideEncryptor with no key source, got nil")
	}
}

// TestProvideEncryptionProvider_InvalidKey mirrors the Encryptor error path
// for admin key rotation's separate KeyProvider registration.
func TestProvideEncryptionProvider_InvalidKey(t *testing.T) {
	t.Parallel()
	inj := do.New()
	do.ProvideValue(inj, &config.Config{
		Encryption: config.EncryptionConfig{Enabled: true},
	})
	_, err := ProvideEncryptionProvider(inj)
	if err == nil {
		t.Fatal("expected error from ProvideEncryptionProvider with no key source, got nil")
	}
}

// TestProvideObjectCache_InvalidSize verifies invalid cache sizing never
// panics — either a clean error or a nil cache is acceptable.
func TestProvideObjectCache_InvalidSize(t *testing.T) {
	t.Parallel()
	inj := do.New()
	do.ProvideValue(inj, &config.Config{
		Cache: config.CacheConfig{
			Enabled:      true,
			MaxSizeBytes: -1,
		},
	})
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ProvideObjectCache panicked: %v", r)
		}
	}()
	_, _ = ProvideObjectCache(inj)
}

// -------------------------------------------------------------------------
// NewInjector REGISTRATION MATRIX
// -------------------------------------------------------------------------

// TestNewInjector_DefaultsRegisterRequiredOnly pins the set of providers
// registered with a minimal config. Required providers must be present;
// gated providers must not leak into the default registration.
func TestNewInjector_DefaultsRegisterRequiredOnly(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	inj := NewInjector(cfg, "all", new(slog.LevelVar), telemetry.NewLogBuffer())
	defer func() { _ = inj.Shutdown() }()

	joined := strings.Join(listServiceNames(inj), ",")

	for _, want := range []string{
		"internal/store.ObjectStore",
		"internal/store.QuotaStore",
		"internal/store.CleanupStore",
		"internal/store.AdminStore",
		"internal/breaker.CircuitBreaker",
		"internal/proxy.BackendManager",
		"internal/transport/s3api.Server",
		"internal/lifecycle.Manager",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("required service %q not registered; got: %s", want, joined)
		}
	}

	for _, unwanted := range []string{
		"internal/encryption.Encryptor",
		"internal/counter.RedisCounterBackend",
		"internal/transport/s3api.RateLimiter",
		"internal/transport/ui.Handler",
		"internal/transport/admin.Handler",
		"internal/notify.Notifier",
	} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("optional service %q should NOT be registered with defaults; got: %s", unwanted, joined)
		}
	}
}

// listServiceNames returns the canonical "pkg.Type" names of every service
// registered on the injector, flattened for substring matching.
func listServiceNames(inj do.Injector) []string {
	var names []string
	for _, s := range inj.ListProvidedServices() {
		names = append(names, s.Service)
	}
	return names
}