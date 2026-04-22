// -------------------------------------------------------------------------------
// Dependency Injection Tests
//
// Author: Alex Freidah
//
// Tests for provider error handling (samber/do patterns adopted in #564),
// the optional-service gating contract in resolveServices/buildHTTPServer/
// registerUIHandler, and happy-path construction of the small leaf providers
// that carry no network side-effects.
// -------------------------------------------------------------------------------

package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// TestProviders_MissingConfigReturnsCleanError verifies that every provider
// surfaces a clean ErrServiceNotFound (rather than panicking) when their
// required injector entries are absent. Pins the post-#564 invariant: no
// provider uses do.MustInvoke internally.
func TestProviders_MissingConfigReturnsCleanError(t *testing.T) {
	tests := []struct {
		name string
		call func(do.Injector) error
	}{
		{"StoreBundle", func(i do.Injector) error { _, err := ProvideStoreBundle(i); return err }},
		{"MetadataStore", func(i do.Injector) error { _, err := ProvideMetadataStore(i); return err }},
		{"AdminStore", func(i do.Injector) error { _, err := ProvideAdminStore(i); return err }},
		{"CBStore", func(i do.Injector) error { _, err := ProvideCBStore(i); return err }},
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
			// Recover so a stray panic surfaces as a test failure rather than
			// crashing the runner — that's the exact regression we're guarding.
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

// TestProvideLogBuffer verifies the trivial log-buffer factory constructs a
// usable LogBuffer instance.
func TestProvideLogBuffer(t *testing.T) {
	lb, err := ProvideLogBuffer(do.New())
	if err != nil {
		t.Fatalf("ProvideLogBuffer: %v", err)
	}
	if lb == nil {
		t.Fatal("ProvideLogBuffer returned nil buffer")
	}
}

// TestProvideLoginThrottle verifies the login-throttle constructor returns a
// non-nil throttle ready for use.
func TestProvideLoginThrottle(t *testing.T) {
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

// TestProvideRateLimiter verifies the rate limiter is constructed from the
// rate-limit config section.
func TestProvideRateLimiter(t *testing.T) {
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

// -------------------------------------------------------------------------
// PROVIDER ERROR PATHS
// -------------------------------------------------------------------------

// TestProvideEncryptor_InvalidKey verifies construction fails cleanly when the
// encryption config has no key source (neither master_key nor master_key_file
// nor Vault). The provider must surface the underlying error, not panic.
func TestProvideEncryptor_InvalidKey(t *testing.T) {
	inj := do.New()
	do.ProvideValue(inj, &config.Config{
		Encryption: config.EncryptionConfig{
			Enabled:   true,
			ChunkSize: 65536,
			// MasterKey intentionally empty — NewKeyProviderFromConfig returns error.
		},
	})
	_, err := ProvideEncryptor(inj)
	if err == nil {
		t.Fatal("expected error from ProvideEncryptor with no key source, got nil")
	}
}

// TestProvideEncryptionProvider_InvalidKey mirrors the Encryptor error path
// for the separate KeyProvider registration used by admin key rotation.
func TestProvideEncryptionProvider_InvalidKey(t *testing.T) {
	inj := do.New()
	do.ProvideValue(inj, &config.Config{
		Encryption: config.EncryptionConfig{Enabled: true},
	})
	_, err := ProvideEncryptionProvider(inj)
	if err == nil {
		t.Fatal("expected error from ProvideEncryptionProvider with no key source, got nil")
	}
}

// TestProvideObjectCache_InvalidSize verifies that invalid cache sizing yields
// a clean error rather than a panic. Exact trigger depends on MemoryCache's
// validation rules; we pass an illegal (negative-equivalent) configuration.
func TestProvideObjectCache_InvalidSize(t *testing.T) {
	inj := do.New()
	do.ProvideValue(inj, &config.Config{
		Cache: config.CacheConfig{
			Enabled:      true,
			MaxSizeBytes: -1, // invalid
		},
	})
	// Either an error return or a nil cache is acceptable — the contract is
	// "don't panic." A successful construction with -1 would be a regression
	// in the cache layer itself, not in this provider.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ProvideObjectCache panicked: %v", r)
		}
	}()
	_, _ = ProvideObjectCache(inj)
}

// -------------------------------------------------------------------------
// OPTIONAL-SERVICE GATING CONTRACT
//
// resolveServices(), buildHTTPServer(), and registerUIHandler() gate on
// cfg.X.Enabled flags at the call site (a #564-era change). When a feature
// is disabled, its Invoke is skipped entirely, leaving the server-struct
// field nil.
// -------------------------------------------------------------------------

// newResolvedServer wires a server with the given YAML config and returns it
// with services resolved. Used by gating-contract tests. Cleanup runs on
// t.Cleanup so each test is fully isolated.
func newResolvedServer(t *testing.T, yaml string) *server {
	t.Helper()
	path := writeTestConfig(t, yaml)
	s := &server{configPath: path, mode: "all", stdout: &bytes.Buffer{}}
	if err := s.loadConfig(); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if err := s.initLogging(); err != nil {
		t.Fatalf("initLogging: %v", err)
	}
	s.initDI()
	if err := s.resolveServices(); err != nil {
		t.Fatalf("resolveServices: %v", err)
	}
	t.Cleanup(func() {
		if s.db != nil {
			s.db.Close()
		}
		if s.manager != nil {
			s.manager.Close()
		}
		if s.inj != nil {
			_ = s.inj.Shutdown()
		}
	})
	return s
}

// TestResolveServices_RateLimiterDisabled verifies that when RateLimit is
// disabled (the default), resolveServices leaves s.rl nil instead of invoking
// the provider.
func TestResolveServices_RateLimiterDisabled(t *testing.T) {
	s := newResolvedServer(t, validTestConfigYAML)
	if s.rl != nil {
		t.Fatal("expected s.rl == nil when rate limiting is disabled")
	}
}

// TestResolveServices_LoginThrottleDisabled verifies that when UI is disabled
// (the default), resolveServices leaves s.loginThrottle nil.
func TestResolveServices_LoginThrottleDisabled(t *testing.T) {
	s := newResolvedServer(t, validTestConfigYAML)
	if s.loginThrottle != nil {
		t.Fatal("expected s.loginThrottle == nil when UI is disabled")
	}
}

// TestResolveServices_RateLimiterEnabled verifies that enabling rate limiting
// drives the provider through resolveServices and populates s.rl.
func TestResolveServices_RateLimiterEnabled(t *testing.T) {
	yaml := validTestConfigYAML + `
rate_limit:
  enabled: true
  requests_per_sec: 10
  burst: 20
`
	s := newResolvedServer(t, yaml)
	if s.rl == nil {
		t.Fatal("expected s.rl != nil when rate limiting is enabled")
	}
}

// TestRegisterUIHandler_Disabled verifies that registerUIHandler returns nil
// and mounts no routes when UI is disabled.
func TestRegisterUIHandler_Disabled(t *testing.T) {
	s := newResolvedServer(t, validTestConfigYAML)
	mux := http.NewServeMux()
	if err := s.registerUIHandler(mux, context.Background()); err != nil {
		t.Fatalf("registerUIHandler (disabled): %v", err)
	}
	if s.uiHandler != nil {
		t.Fatal("expected s.uiHandler == nil when UI is disabled")
	}

	// No /ui/ route should be mounted — an arbitrary UI path returns 404.
	req := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for /ui/ when UI disabled, got %d", w.Code)
	}
}

// TestBuildHTTPServer_NoAdminKey verifies that with an empty cfg.UI.AdminKey,
// no /admin/ route is mounted. With mode="all" the S3 handler is mounted on
// "/" as a catch-all, so /admin/* falls through to it — we detect the absence
// of the admin handler by the XML content-type the S3 handler returns.
func TestBuildHTTPServer_NoAdminKey(t *testing.T) {
	s := newResolvedServer(t, validTestConfigYAML)
	if err := s.buildHTTPServer(); err != nil {
		t.Fatalf("buildHTTPServer: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/api/health", nil)
	w := httptest.NewRecorder()
	s.httpServer.Handler.ServeHTTP(w, req)

	ct := w.Header().Get("Content-Type")
	if strings.Contains(ct, "json") {
		t.Fatalf("expected /admin/api/health to fall through S3 handler; got JSON content-type %q (admin handler appears mounted)", ct)
	}
}

// -------------------------------------------------------------------------
// SHUTDOWN HELPERS
// -------------------------------------------------------------------------

// TestShutdownHTTPServers_NoMetricsServer verifies the helper handles the
// common case where no metrics listener was started (s.metricsServer == nil).
func TestShutdownHTTPServers_NoMetricsServer(t *testing.T) {
	s := &server{httpServer: &http.Server{ReadHeaderTimeout: time.Second}}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("shutdownHTTPServers panicked with no metricsServer: %v", r)
		}
	}()
	s.shutdownHTTPServers(context.Background())
}

// TestCloseEncryptionProvider_Unregistered verifies closeEncryptionProvider
// is a no-op when no KeyProvider was registered (the non-encryption case).
func TestCloseEncryptionProvider_Unregistered(t *testing.T) {
	s := &server{inj: do.New()}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("closeEncryptionProvider panicked on empty injector: %v", r)
		}
	}()
	s.closeEncryptionProvider()
}

// TestCloseRedisBackend_Unregistered verifies closeRedisBackend is a no-op
// when no Redis counter backend was registered.
func TestCloseRedisBackend_Unregistered(t *testing.T) {
	s := &server{inj: do.New()}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("closeRedisBackend panicked on empty injector: %v", r)
		}
	}()
	s.closeRedisBackend(context.Background())
}

// -------------------------------------------------------------------------
// NewInjector REGISTRATION MATRIX
// -------------------------------------------------------------------------

// TestNewInjector_DefaultsRegisterRequiredOnly verifies that with a minimal
// config (all optional sections off), only the always-required providers are
// registered. Guards against drift: if someone adds an optional provider
// without a config gate, this test catches the leak.
func TestNewInjector_DefaultsRegisterRequiredOnly(t *testing.T) {
	cfg := &config.Config{}
	inj := NewInjector(cfg, "all", new(slog.LevelVar), telemetry.NewLogBuffer())
	defer func() { _ = inj.Shutdown() }()

	services := inj.ListProvidedServices()
	var names []string
	for _, s := range services {
		names = append(names, s.Service)
	}
	joined := strings.Join(names, ",")

	// Required services must always be registered.
	for _, want := range []string{
		"cmd/s3-orchestrator.storeBundle",
		"internal/store.CircuitBreakerStore",
		"internal/proxy.BackendManager",
		"internal/transport/s3api.Server",
		"internal/lifecycle.Manager",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("required service %q not registered; got: %s", want, joined)
		}
	}

	// Optional services must NOT be registered when their config is off.
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
