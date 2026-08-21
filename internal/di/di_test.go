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
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/samber/do/v2"
	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/notify"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
	"github.com/afreidah/s3-orchestrator/internal/transport/ui"
	"github.com/afreidah/s3-orchestrator/internal/worker"
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
		{"DatabaseBreaker", func(i do.Injector) error { _, err := ProvideDatabaseBreaker(i); return err }},
		{"MetadataStore", func(i do.Injector) error { _, err := ProvideMetadataStore(i); return err }},
		{"Backends", func(i do.Injector) error { _, err := ProvideBackends(i); return err }},
		{"Encryptor", func(i do.Injector) error { _, err := ProvideEncryptor(i); return err }},
		{"EncryptionProvider", func(i do.Injector) error { _, err := ProvideEncryptionProvider(i); return err }},
		{"RedisCounterBackend", func(i do.Injector) error { _, err := ProvideRedisCounterBackend(i); return err }},
		{"ObjectCache", func(i do.Injector) error { _, err := ProvideObjectCache(i); return err }},
		{"BackendManager", func(i do.Injector) error { _, err := ProvideBackendManager(i); return err }},
		{"LifecycleManager", func(i do.Injector) error { _, err := ProvideLifecycleManager(i); return err }},
		{"Rebalancer", func(i do.Injector) error { _, err := ProvideRebalancer(i); return err }},
		{"Replicator", func(i do.Injector) error { _, err := ProvideReplicator(i); return err }},
		{"OverReplicationCleaner", func(i do.Injector) error { _, err := ProvideOverReplicationCleaner(i); return err }},
		{"CleanupWorker", func(i do.Injector) error { _, err := ProvideCleanupWorker(i); return err }},
		{"PendingReaper", func(i do.Injector) error { _, err := ProvidePendingReaper(i); return err }},
		{"Scrubber", func(i do.Injector) error { _, err := ProvideScrubber(i); return err }},
		{"DrainManager", func(i do.Injector) error { _, err := ProvideDrainManager(i); return err }},
		{"Reconciler", func(i do.Injector) error { _, err := ProvideReconciler(i); return err }},
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

// TestOpenStore_InvalidDriver covers the default branch of openStore's
// driver-dispatch switch.
func TestOpenStore_InvalidDriver(t *testing.T) {
	t.Parallel()
	_, err := openStore(context.Background(), &config.DatabaseConfig{Driver: "bogus"}, nil)
	if err == nil {
		t.Fatal("expected error for unsupported driver, got nil")
	}
}

// TestOpenStore_SQLiteInMemory covers the sqlite branch of openStore with
// an in-memory database  -  verifies the handle is non-nil and satisfies the
// LifecycleAdmin role embedded in concreteStore.
func TestOpenStore_SQLiteInMemory(t *testing.T) {
	t.Parallel()
	cs, err := openStore(context.Background(), &config.DatabaseConfig{
		Driver: "sqlite",
		Path:   ":memory:",
	}, nil)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	if cs == nil {
		t.Fatal("expected non-nil concreteStore")
	}
	cs.Close()
}

// TestWireAuditMetrics covers the audit->Prometheus wiring side-effect
// and drives the registered callback by emitting an audit event so the
// inner closure (which increments the Prometheus counter) actually runs.
// Restores the previous callback state on cleanup so other tests aren't
// affected by the package-global SetOnEvent registration.
func TestWireAuditMetrics(t *testing.T) {
	defer func() {
		audit.SetOnEvent(nil)
		if r := recover(); r != nil {
			t.Fatalf("WireAuditMetrics panicked: %v", r)
		}
	}()
	WireAuditMetrics()
	// Fire an audit event so the registered callback runs.
	audit.Log(context.Background(), "test.coverage")
}

// TestProvideMetadataStore_SQLiteInMemory drives ProvideMetadataStore's
// happy path against an in-memory sqlite, covering migrations + schema
// verification + quota sync without needing Postgres.
func TestProvideMetadataStore_SQLiteInMemory(t *testing.T) {
	t.Parallel()
	inj := do.New()
	do.ProvideValue(inj, &config.Config{
		Database: config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"},
		Backends: []config.BackendConfig{{Name: "b1", QuotaBytes: 1024}},
	})
	do.ProvideValue[*breaker.CircuitBreaker](inj, nil)
	cs, err := ProvideMetadataStore(inj)
	if err != nil {
		t.Fatalf("ProvideMetadataStore: %v", err)
	}
	if cs == nil {
		t.Fatal("expected non-nil MetadataStore")
	}
	cs.Close()
}

// TestOpenStore_PostgresInvalidConfig covers the postgres branch of the
// driver switch. We can't open a real Postgres in a unit test, so we use
// a config that fails fast  -  the function still exercises the
// store.NewStore call site, which is the uncovered branch.
func TestOpenStore_PostgresInvalidConfig(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := openStore(ctx, &config.DatabaseConfig{
		Driver:   "postgres",
		Host:     "127.0.0.1",
		Port:     1, // unreachable port  -  connect fails fast
		Database: "nope",
		User:     "nope",
		Password: "nope",
		SSLMode:  "disable",
	}, nil)
	if err == nil {
		t.Fatal("expected error connecting to unreachable postgres, got nil")
	}
}

// TestRegisterInfrastructure_StoreAliases verifies registerInfrastructure
// exposes the wide metadata store under each narrow role interface, so a
// deleted or mistyped do.MustAs is caught here rather than at boot. The real
// store provider is overridden with a mock so no database is opened.
func TestRegisterInfrastructure_StoreAliases(t *testing.T) {
	t.Parallel()
	inj := do.New()
	registerInfrastructure(inj)
	do.OverrideValue[core.MetadataStore](inj, storetest.NewMockMetadataStore(gomock.NewController(t)))

	if _, err := do.Invoke[core.LifecycleAdmin](inj); err != nil {
		t.Errorf("LifecycleAdmin alias: %v", err)
	}
	if _, err := do.Invoke[core.EncryptionAdmin](inj); err != nil {
		t.Errorf("EncryptionAdmin alias: %v", err)
	}
	if _, err := do.Invoke[core.NotificationOutbox](inj); err != nil {
		t.Errorf("NotificationOutbox alias: %v", err)
	}
	if _, err := do.Invoke[metrics.Deps](inj); err != nil {
		t.Errorf("metrics.Deps alias: %v", err)
	}
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

// TestProvideEncryptor_HappyPath covers the success branch where a valid
// static master key produces an encryptor. The startup log line that
// reports chunk size and key ID also fires, which is what new-code
// coverage tracks for this PR.
func TestProvideEncryptor_HappyPath(t *testing.T) {
	t.Parallel()
	inj := do.New()
	// 32-byte base64 key (all zeros is valid for the format check).
	key := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	do.ProvideValue(inj, &config.Config{
		Encryption: config.EncryptionConfig{
			Enabled:   true,
			ChunkSize: 65536,
			MasterKey: key,
		},
	})
	enc, err := ProvideEncryptor(inj)
	if err != nil {
		t.Fatalf("ProvideEncryptor: %v", err)
	}
	if enc == nil {
		t.Fatal("ProvideEncryptor returned nil encryptor")
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

// TestProvideObjectCache_HappyPath drives ProvideObjectCache through
// the success branch with a valid cache configuration; the startup
// "object data cache enabled" log fires here, which is the new-code
// coverage target for this PR.
func TestProvideObjectCache_HappyPath(t *testing.T) {
	t.Parallel()
	inj := do.New()
	do.ProvideValue(inj, &config.Config{
		Cache: config.CacheConfig{
			Enabled:            true,
			MaxSize:            "16MB",
			MaxSizeBytes:       16 << 20,
			MaxObjectSize:      "1MB",
			MaxObjectSizeBytes: 1 << 20,
			TTL:                time.Minute,
		},
	})
	cache, err := ProvideObjectCache(inj)
	if err != nil {
		t.Fatalf("ProvideObjectCache: %v", err)
	}
	if cache == nil {
		t.Fatal("ProvideObjectCache returned nil cache")
	}
}

// TestProvideObjectCache_InvalidSize verifies invalid cache sizing never
// panics  -  either a clean error or a nil cache is acceptable.
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

// TestResolveOptionalEncryptor_Disabled returns nil/nil when encryption
// is off  -  covers the early-return branch.
func TestResolveOptionalEncryptor_Disabled(t *testing.T) {
	t.Parallel()
	inj := do.New()
	enc, err := resolveOptionalEncryptor(inj, false)
	if err != nil {
		t.Fatalf("resolveOptionalEncryptor(false): %v", err)
	}
	if enc != nil {
		t.Errorf("expected nil encryptor when disabled, got %v", enc)
	}
}

// TestResolveOptionalEncryptor_EnabledMissing wraps the do.Invoke error
// when encryption is enabled but no encryptor provider is registered.
func TestResolveOptionalEncryptor_EnabledMissing(t *testing.T) {
	t.Parallel()
	inj := do.New()
	_, err := resolveOptionalEncryptor(inj, true)
	if err == nil {
		t.Fatal("expected error when encryption enabled but no provider, got nil")
	}
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
	inj := NewInjector(InjectorDeps{Config: cfg, Mode: "all", LogLevel: new(slog.LevelVar), LogBuffer: telemetry.NewLogBuffer()})
	defer func() { _ = inj.Shutdown() }()

	joined := strings.Join(listServiceNames(inj), ",")

	for _, want := range []string{
		"internal/store/core.MetadataStore",
		"internal/store/core.LifecycleAdmin",
		"internal/store/core.EncryptionAdmin",
		"internal/store/core.NotificationOutbox",
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

// -------------------------------------------------------------------------
// FULL-INJECTOR HAPPY PATH
//
// Drives the big composite providers (Backends, BackendManager, S3Server,
// LifecycleManager, UIHandler, AdminHandler, Notifier) end-to-end against
// an in-memory SQLite store and a fake S3 backend endpoint. The storage
// calls never fire during construction  -  NewS3Backend only parses config  -
// so no live network is required.
// -------------------------------------------------------------------------

// happyPathConfig returns a Config that every optional provider accepts
// and that resolves through the full NewInjector wiring.
func happyPathConfig(tmpDir string) *config.Config {
	return &config.Config{
		Server: config.ServerConfig{
			ListenAddr:            "127.0.0.1:0",
			BackendTimeout:        30 * time.Second,
			MaxObjectSize:         1 << 20,
			MaxConcurrentRequests: 4,
		},
		Database: config.DatabaseConfig{
			Driver: "sqlite",
			Path:   tmpDir + "/test.db",
		},
		CircuitBreaker: config.CircuitBreakerConfig{
			FailureThreshold: 3,
			OpenTimeout:      time.Second,
			CacheTTL:         time.Minute,
		},
		Backends: []config.BackendConfig{{
			Name:            "b1",
			Endpoint:        "http://localhost:9999",
			Region:          "us-east-1",
			Bucket:          "bucket",
			AccessKeyID:     "AK",
			SecretAccessKey: "SK",
			ForcePathStyle:  true,
			QuotaBytes:      1024,
		}},
		Buckets: []config.BucketConfig{{
			Name:        "test-bucket",
			Credentials: []config.CredentialConfig{{AccessKeyID: "AK", SecretAccessKey: "SK"}},
		}},
		RoutingStrategy: config.RoutingPack,
		Cache: config.CacheConfig{
			Enabled:      true,
			MaxSize:      "64MB",
			MaxSizeBytes: 64 << 20,
		},
		RateLimit: config.RateLimitConfig{
			Enabled:        true,
			RequestsPerSec: 10,
			Burst:          10,
		},
		UI: config.UIConfig{
			Enabled:       true,
			AdminKey:      "admin-key",
			AdminSecret:   "admin-secret",
			AdminToken:    "secret-token",
			SessionSecret: "0123456789abcdef0123456789abcdef",
		},
	}
}

// TestNewInjector_HappyPathResolvesEverything walks every provider
// registered by the default-plus-ui config. Each do.Invoke exercises the
// full body of its provider (not just the missing-dep early return),
// which is what Sonar counts as new-code coverage.
func TestNewInjector_HappyPathResolvesEverything(t *testing.T) {
	t.Parallel()
	cfg := happyPathConfig(t.TempDir())
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("config validation: %v", err)
	}
	inj := NewInjector(InjectorDeps{Config: cfg, Mode: "all", LogLevel: new(slog.LevelVar), LogBuffer: telemetry.NewLogBuffer()})
	t.Cleanup(func() { _ = inj.Shutdown() })

	if _, err := do.Invoke[*BackendsResult](inj); err != nil {
		t.Errorf("BackendsResult: %v", err)
	}
	if _, err := do.Invoke[core.LifecycleAdmin](inj); err != nil {
		t.Errorf("LifecycleAdmin: %v", err)
	}
	if _, err := do.Invoke[core.EncryptionAdmin](inj); err != nil {
		t.Errorf("EncryptionAdmin: %v", err)
	}
	if _, err := do.Invoke[core.NotificationOutbox](inj); err != nil {
		t.Errorf("NotificationOutbox: %v", err)
	}
	if _, err := do.Invoke[*breaker.CircuitBreaker](inj); err != nil {
		t.Errorf("CircuitBreaker: %v", err)
	}
	if _, err := do.Invoke[*proxy.BackendManager](inj); err != nil {
		t.Errorf("BackendManager: %v", err)
	}
	if _, err := do.Invoke[*s3api.Server](inj); err != nil {
		t.Errorf("S3Server: %v", err)
	}
	if _, err := do.Invoke[*ui.Handler](inj); err != nil {
		t.Errorf("UIHandler: %v", err)
	}
	if _, err := do.Invoke[*admin.Handler](inj); err != nil {
		t.Errorf("AdminHandler: %v", err)
	}
	if _, err := do.Invoke[*s3api.RateLimiter](inj); err != nil {
		t.Errorf("RateLimiter: %v", err)
	}
	if _, err := do.Invoke[*httputil.LoginThrottle](inj); err != nil {
		t.Errorf("LoginThrottle: %v", err)
	}
	if _, err := do.Invoke[*lifecycle.Manager](inj); err != nil {
		t.Errorf("LifecycleManager: %v", err)
	}
}

// TestNewInjector_WorkerModeResolvesLifecycle covers the mode == "worker"
// / "all" branch of ProvideLifecycleManager (which registers the full
// set of background services instead of the minimal pair). Also drives
// each per-worker DI provider through its happy path so the worker-
// construction branches added in #676 B run end-to-end.
func TestNewInjector_WorkerModeResolvesLifecycle(t *testing.T) {
	t.Parallel()
	cfg := happyPathConfig(t.TempDir())
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("config validation: %v", err)
	}
	inj := NewInjector(InjectorDeps{Config: cfg, Mode: "worker", LogLevel: new(slog.LevelVar), LogBuffer: telemetry.NewLogBuffer()})
	t.Cleanup(func() { _ = inj.Shutdown() })

	if _, err := do.Invoke[*lifecycle.Manager](inj); err != nil {
		t.Fatalf("LifecycleManager: %v", err)
	}
	if _, err := do.Invoke[*worker.Rebalancer](inj); err != nil {
		t.Errorf("Rebalancer: %v", err)
	}
	if _, err := do.Invoke[*worker.Replicator](inj); err != nil {
		t.Errorf("Replicator: %v", err)
	}
	if _, err := do.Invoke[*worker.OverReplicationCleaner](inj); err != nil {
		t.Errorf("OverReplicationCleaner: %v", err)
	}
	if _, err := do.Invoke[*worker.CleanupWorker](inj); err != nil {
		t.Errorf("CleanupWorker: %v", err)
	}
	if _, err := do.Invoke[*worker.Scrubber](inj); err != nil {
		t.Errorf("Scrubber: %v", err)
	}
	if _, err := do.Invoke[*drain.Manager](inj); err != nil {
		t.Errorf("DrainManager: %v", err)
	}
	// PendingReaper is conditionally registered (#830). happyPathConfig
	// leaves PendingPattern at the default (enabled), so the provider
	// is registered and resolves cleanly.
	if !IsRegistered[*worker.PendingReaper](inj) {
		t.Error("PendingReaper not registered with default config")
	}
	if _, err := do.Invoke[*worker.PendingReaper](inj); err != nil {
		t.Errorf("PendingReaper: %v", err)
	}
}

// TestNewInjector_RootsResolveInEveryMode builds the injector in each run
// mode and resolves the always-registered roots. BackendManager and
// lifecycle.Manager are registered unconditionally and transitively pull
// in the bulk of the graph, so a registered-but-unresolvable provider
// surfaces here in CI rather than as a production startup panic after a
// mode change. Complements the "all" (every provider) and "worker"
// (every worker) resolution tests above by covering api mode explicitly.
func TestNewInjector_RootsResolveInEveryMode(t *testing.T) {
	t.Parallel()
	for _, mode := range []config.Mode{"api", "worker", "all"} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			cfg := happyPathConfig(t.TempDir())
			if err := cfg.SetDefaultsAndValidate(); err != nil {
				t.Fatalf("config validation: %v", err)
			}
			inj := NewInjector(InjectorDeps{Config: cfg, Mode: mode, LogLevel: new(slog.LevelVar), LogBuffer: telemetry.NewLogBuffer()})
			t.Cleanup(func() { _ = inj.Shutdown() })

			if _, err := do.Invoke[*proxy.BackendManager](inj); err != nil {
				t.Fatalf("BackendManager: %v", err)
			}
			if _, err := do.Invoke[*lifecycle.Manager](inj); err != nil {
				t.Fatalf("lifecycle.Manager: %v", err)
			}
		})
	}
}

// TestNewInjector_PendingReaperDisabled covers the conditional
// registration (#830): when the pending pattern is off in config,
// no PendingReaper provider is registered and Optional reports
// Disabled (not Failed), so callers can distinguish "feature off"
// from "feature on but broken."
func TestNewInjector_PendingReaperDisabled(t *testing.T) {
	t.Parallel()
	cfg := happyPathConfig(t.TempDir())
	disabled := false
	cfg.WritePath.PendingPattern.Enabled = &disabled
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("config validation: %v", err)
	}
	inj := NewInjector(InjectorDeps{Config: cfg, Mode: "all", LogLevel: new(slog.LevelVar), LogBuffer: telemetry.NewLogBuffer()})
	t.Cleanup(func() { _ = inj.Shutdown() })

	if IsRegistered[*worker.PendingReaper](inj) {
		t.Error("PendingReaper should NOT be registered when feature disabled")
	}
	res := Optional[*worker.PendingReaper](inj)
	if !res.Disabled() {
		t.Errorf("Optional[*worker.PendingReaper].Resolution = %s, want disabled", res.Resolution)
	}
	if res.Failed() {
		t.Errorf("Optional reported Failed for a disabled feature: %v", res.Err)
	}

	// WireManager must still succeed with the feature off.
	if err := WireManager(inj); err != nil {
		t.Errorf("WireManager with PendingReaper disabled: %v", err)
	}
}

// TestNewInjector_NotifierResolvesWhenEndpointsConfigured covers the
// optional notifier provider via the full injector path.
func TestNewInjector_NotifierResolvesWhenEndpointsConfigured(t *testing.T) {
	t.Parallel()
	cfg := happyPathConfig(t.TempDir())
	cfg.Notifications.Endpoints = []config.NotificationEndpoint{{
		URL:    "http://example.invalid/webhook",
		Events: []string{"*"},
	}}
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("config validation: %v", err)
	}
	inj := NewInjector(InjectorDeps{Config: cfg, Mode: "worker", LogLevel: new(slog.LevelVar), LogBuffer: telemetry.NewLogBuffer()})
	t.Cleanup(func() { _ = inj.Shutdown() })

	if _, err := do.Invoke[*notify.Notifier](inj); err != nil {
		t.Fatalf("Notifier: %v", err)
	}
}

// TestProvideReconciler_HappyPath drives the reconciler factory end-to-end.
func TestProvideReconciler_HappyPath(t *testing.T) {
	t.Parallel()
	cfg := happyPathConfig(t.TempDir())
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("config validation: %v", err)
	}
	inj := NewInjector(InjectorDeps{Config: cfg, Mode: "all", LogLevel: new(slog.LevelVar), LogBuffer: telemetry.NewLogBuffer()})
	t.Cleanup(func() { _ = inj.Shutdown() })
	rec, err := do.Invoke[*worker.Reconciler](inj)
	if err != nil {
		t.Fatalf("Reconciler: %v", err)
	}
	if rec == nil {
		t.Fatal("Reconciler resolved to nil")
	}
}

// TestNewInjector_ReconcilerNotRegisteredInAPIMode pins the mode-conditional
// registration: api-mode binaries do not need the worker-side reconciler.
func TestNewInjector_ReconcilerNotRegisteredInAPIMode(t *testing.T) {
	t.Parallel()
	cfg := happyPathConfig(t.TempDir())
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("config validation: %v", err)
	}
	inj := NewInjector(InjectorDeps{Config: cfg, Mode: "api", LogLevel: new(slog.LevelVar), LogBuffer: telemetry.NewLogBuffer()})
	t.Cleanup(func() { _ = inj.Shutdown() })
	if _, err := do.Invoke[*worker.Reconciler](inj); !errors.Is(err, do.ErrServiceNotFound) {
		t.Fatalf("expected ErrServiceNotFound in api mode, got %v", err)
	}
}

// TestResolveAdminHandlerRequiredDeps_PartialDeps walks the dependency
// list so every intermediate missing-dep return path in the helper is
// exercised, not just the bare-injector first-invoke branch.
func TestResolveAdminHandlerRequiredDeps_PartialDeps(t *testing.T) {
	t.Parallel()
	cfg := happyPathConfig(t.TempDir())
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("config validation: %v", err)
	}
	full := NewInjector(InjectorDeps{Config: cfg, Mode: "all", LogLevel: new(slog.LevelVar), LogBuffer: telemetry.NewLogBuffer()})
	t.Cleanup(func() { _ = full.Shutdown() })

	mgr, err := do.Invoke[*proxy.BackendManager](full)
	if err != nil {
		t.Fatalf("BackendManager: %v", err)
	}
	cb, err := do.Invoke[*breaker.CircuitBreaker](full)
	if err != nil {
		t.Fatalf("DatabaseBreaker: %v", err)
	}
	stores, err := do.Invoke[core.MetadataStore](full)
	if err != nil {
		t.Fatalf("MetadataStore: %v", err)
	}
	repl, err := do.Invoke[*worker.Replicator](full)
	if err != nil {
		t.Fatalf("Replicator: %v", err)
	}
	overRep, err := do.Invoke[*worker.OverReplicationCleaner](full)
	if err != nil {
		t.Fatalf("OverReplicationCleaner: %v", err)
	}
	scrubber, err := do.Invoke[*worker.Scrubber](full)
	if err != nil {
		t.Fatalf("Scrubber: %v", err)
	}

	// Each step seeds one more dep than the previous; the helper should
	// fail on the first unseeded dep at each stage.
	steps := []struct {
		name string
		seed func(do.Injector)
	}{
		{"only-cfg", func(i do.Injector) {
			do.ProvideValue(i, cfg)
		}},
		{"+manager", func(i do.Injector) {
			do.ProvideValue(i, cfg)
			do.ProvideValue(i, mgr)
		}},
		{"+cb", func(i do.Injector) {
			do.ProvideValue(i, cfg)
			do.ProvideValue(i, mgr)
			do.ProvideValue(i, cb)
		}},
		{"+encAdmin", func(i do.Injector) {
			do.ProvideValue(i, cfg)
			do.ProvideValue(i, mgr)
			do.ProvideValue(i, cb)
			do.ProvideValue[core.EncryptionAdmin](i, stores)
		}},
		{"+logLevel", func(i do.Injector) {
			do.ProvideValue(i, cfg)
			do.ProvideValue(i, mgr)
			do.ProvideValue(i, cb)
			do.ProvideValue[core.EncryptionAdmin](i, stores)
			do.ProvideValue(i, new(slog.LevelVar))
		}},
		{"+stores", func(i do.Injector) {
			do.ProvideValue(i, cfg)
			do.ProvideValue(i, mgr)
			do.ProvideValue(i, cb)
			do.ProvideValue[core.EncryptionAdmin](i, stores)
			do.ProvideValue(i, new(slog.LevelVar))
			do.ProvideValue(i, stores)
		}},
		{"+replicator", func(i do.Injector) {
			do.ProvideValue(i, cfg)
			do.ProvideValue(i, mgr)
			do.ProvideValue(i, cb)
			do.ProvideValue[core.EncryptionAdmin](i, stores)
			do.ProvideValue(i, new(slog.LevelVar))
			do.ProvideValue(i, stores)
			do.ProvideValue(i, repl)
		}},
		{"+overRep", func(i do.Injector) {
			do.ProvideValue(i, cfg)
			do.ProvideValue(i, mgr)
			do.ProvideValue(i, cb)
			do.ProvideValue[core.EncryptionAdmin](i, stores)
			do.ProvideValue(i, new(slog.LevelVar))
			do.ProvideValue(i, stores)
			do.ProvideValue(i, repl)
			do.ProvideValue(i, overRep)
		}},
		{"+scrubber", func(i do.Injector) {
			do.ProvideValue(i, cfg)
			do.ProvideValue(i, mgr)
			do.ProvideValue(i, cb)
			do.ProvideValue[core.EncryptionAdmin](i, stores)
			do.ProvideValue(i, new(slog.LevelVar))
			do.ProvideValue(i, stores)
			do.ProvideValue(i, repl)
			do.ProvideValue(i, overRep)
			do.ProvideValue(i, scrubber)
		}},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			t.Parallel()
			inj := do.New()
			step.seed(inj)
			if _, err := resolveAdminHandlerRequiredDeps(inj); err == nil {
				t.Fatal("expected error from partial deps")
			}
		})
	}
}

// TestProvideReconciler_PartialDeps walks the resolve sequence so each
// missing-dep return path is exercised.
func TestProvideReconciler_PartialDeps(t *testing.T) {
	t.Parallel()
	// Bare injector: config missing.
	if _, err := ProvideReconciler(do.New()); err == nil {
		t.Error("expected error with no deps")
	}
	// Add only config: BackendManager missing.
	step1 := do.New()
	do.ProvideValue(step1, &config.Config{Buckets: []config.BucketConfig{{Name: "b1"}}})
	if _, err := ProvideReconciler(step1); err == nil {
		t.Error("expected error after seeding only config")
	}
}

// TestProvideAdminHandler_ReconcilerFailedLogsAndContinues drives the
// Failed branch added by the Optional[T] migration: overriding the
// Reconciler provider with one that errors must not abort admin handler
// construction; the handler is wired with a nil reconciler and the
// failure is logged at startup.
func TestProvideAdminHandler_ReconcilerFailedLogsAndContinues(t *testing.T) {
	t.Parallel()
	cfg := happyPathConfig(t.TempDir())
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("config validation: %v", err)
	}
	inj := NewInjector(InjectorDeps{Config: cfg, Mode: "all", LogLevel: new(slog.LevelVar), LogBuffer: telemetry.NewLogBuffer()})
	t.Cleanup(func() { _ = inj.Shutdown() })

	if err := WireManager(inj); err != nil {
		t.Fatalf("WireManager: %v", err)
	}

	boom := errors.New("reconciler construction failed")
	do.Override(inj, func(do.Injector) (*worker.Reconciler, error) {
		return nil, boom
	})

	h, err := ProvideAdminHandler(inj)
	if err != nil {
		t.Fatalf("ProvideAdminHandler: %v (expected to swallow Failed optional)", err)
	}
	if h == nil {
		t.Fatal("ProvideAdminHandler returned nil")
	}
}

// TestProvideCodec_RejectsUnknownLevel covers the branch where a config that
// passed validation still cannot build a codec, which is why the provider is
// resolved strictly rather than treated as optional.
func TestProvideCodec_RejectsUnknownLevel(t *testing.T) {
	inj := do.New()
	do.ProvideValue(inj, &config.Config{
		Compression: config.CompressionConfig{Enabled: true, Level: "turbo", ChunkSize: 1 << 20},
	})
	if _, err := ProvideCodec(inj); err == nil {
		t.Fatal("expected an error for an unknown compression level, got nil")
	}
}

// TestProvideCodec_DefaultsWhenBlockOmitted checks that a config with no
// compression block still yields a codec, since objects already stored
// compressed have to stay readable whether or not the feature is on.
func TestProvideCodec_DefaultsWhenBlockOmitted(t *testing.T) {
	inj := do.New()
	do.ProvideValue(inj, &config.Config{})
	c, err := ProvideCodec(inj)
	if err != nil {
		t.Fatalf("ProvideCodec: %v", err)
	}
	defer c.Close()
	if c.ChunkSize() != config.DefaultCompressionChunkSize {
		t.Errorf("ChunkSize = %d, want the %d default", c.ChunkSize(), config.DefaultCompressionChunkSize)
	}
}
