// -------------------------------------------------------------------------------
// Reload Coordinator - Hook Branch Tests
//
// Author: Alex Freidah
//
// Drives each defaultHooks entry through its Skipped / Failed / Applied
// branches directly so the Optional[T] error-classification work has
// real coverage. The Failed branches matter most: a broken-but-
// configured optional dependency used to look identical to an
// intentionally absent one, and the hooks now surface that distinction
// as HookFailed. Each test builds the minimum injector the hook reads,
// usually with a failing constructor, and asserts on the returned
// status and error wrap.
// -------------------------------------------------------------------------------

package reload

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/provisioning"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
	"github.com/afreidah/s3-orchestrator/internal/transport/cors"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// errResolve is the sentinel every "provider registered but errors"
// case threads through so tests can assert the wrapped error survives.
var errResolve = errors.New("dependency unavailable")

// failingProvider registers a constructor for T that always returns
// errResolve, modelling a broken-but-configured optional dependency.
func failingProvider[T any](inj do.Injector) {
	do.Provide(inj, func(do.Injector) (T, error) {
		var zero T
		return zero, errResolve
	})
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestResolutionError_WrapsCause confirms the helper's rendered form
// includes the subsystem label and unwraps to the original error so
// callers can errors.Is against it.
func TestResolutionError_WrapsCause(t *testing.T) {
	err := resolutionError("widget", errResolve)
	if err == nil {
		t.Fatal("resolutionError returned nil")
	}
	if !strings.Contains(err.Error(), "widget") {
		t.Errorf("error %q missing subsystem label", err)
	}
	if !errors.Is(err, errResolve) {
		t.Errorf("error chain does not unwrap to errResolve")
	}
}

// TestTlsCertHook_NilReloaderSkipped covers the "TLS not configured"
// path where the coordinator was constructed without a CertReloader.
func TestTlsCertHook_NilReloaderSkipped(t *testing.T) {
	h := &tlsCertHook{}
	status, err := h.Apply(context.Background(), nil, nil)
	if status != HookSkipped || err != nil {
		t.Fatalf("Apply = (%s, %v), want (skipped, nil)", status, err)
	}
	if h.Name() != "tls_certificate" {
		t.Errorf("Name = %q", h.Name())
	}
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// writeSelfSignedCert generates a self-signed ECDSA cert/key pair at
// the given paths so the tlsCertHook tests can drive a real
// CertReloader without an external CA.
func writeSelfSignedCert(t *testing.T, certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "reload-test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestTlsCertHook_AppliedOnSuccessfulReload wires a real CertReloader
// over a valid cert pair, then rewrites the cert files with a fresh
// pair and asserts Apply reports HookApplied.
func TestTlsCertHook_AppliedOnSuccessfulReload(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	writeSelfSignedCert(t, certPath, keyPath)

	cr, err := httputil.NewCertReloader(certPath, keyPath)
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}
	writeSelfSignedCert(t, certPath, keyPath)

	h := &tlsCertHook{reloader: cr}
	status, err := h.Apply(context.Background(), nil, nil)
	if status != HookApplied || err != nil {
		t.Fatalf("Apply = (%s, %v), want (applied, nil)", status, err)
	}
}

// TestTlsCertHook_FailedOnReloadError starts with a valid cert pair,
// then corrupts the cert file on disk before reload so
// tls.LoadX509KeyPair fails. The hook must surface HookFailed with the
// underlying error.
func TestTlsCertHook_FailedOnReloadError(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	writeSelfSignedCert(t, certPath, keyPath)

	cr, err := httputil.NewCertReloader(certPath, keyPath)
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}
	if err := os.WriteFile(certPath, []byte("not a pem block"), 0600); err != nil {
		t.Fatalf("corrupt cert: %v", err)
	}

	h := &tlsCertHook{reloader: cr}
	status, err := h.Apply(context.Background(), nil, nil)
	if status != HookFailed {
		t.Fatalf("status = %s, want failed", status)
	}
	if err == nil {
		t.Fatal("expected reload error, got nil")
	}
}

// TestQuotaSyncHook_AppliedOnSuccess wires a fake LifecycleAdmin whose
// SyncQuotaLimits returns nil and asserts the hook reports Applied.
func TestQuotaSyncHook_AppliedOnSuccess(t *testing.T) {
	fake := storetest.NewMockLifecycleAdmin(gomock.NewController(t))
	// Exactly one sync is the assertion: the hook must not retry or skip.
	fake.EXPECT().SyncQuotaLimits(gomock.Any(), gomock.Any()).Return(nil).Times(1)
	inj := do.New()
	do.ProvideValue[core.LifecycleAdmin](inj, fake)
	h := &quotaSyncHook{inj: inj}
	status, err := h.Apply(context.Background(), nil, &config.Config{})
	if status != HookApplied || err != nil {
		t.Fatalf("Apply = (%s, %v), want (applied, nil)", status, err)
	}
}

// TestQuotaSyncHook_FailedOnSyncError wires a fake whose SyncQuotaLimits
// returns an error so the inner error path is covered: the hook surfaces
// HookFailed with the unwrapped underlying error.
func TestQuotaSyncHook_FailedOnSyncError(t *testing.T) {
	syncBoom := errors.New("quota sync failed")
	inj := do.New()
	failing := storetest.NewMockLifecycleAdmin(gomock.NewController(t))
	failing.EXPECT().SyncQuotaLimits(gomock.Any(), gomock.Any()).Return(syncBoom).AnyTimes()
	do.ProvideValue[core.LifecycleAdmin](inj, failing)
	h := &quotaSyncHook{inj: inj}
	status, err := h.Apply(context.Background(), nil, &config.Config{})
	if status != HookFailed {
		t.Fatalf("status = %s, want failed", status)
	}
	if !errors.Is(err, syncBoom) {
		t.Fatalf("err = %v, want syncBoom", err)
	}
}

// TestProvisioningHook_ApplyInstalls proves a reload publishes the new bucket
// set, credentials and CORS rules, which is what makes adding a bucket or a
// browser origin a reload rather than a restart.
func TestProvisioningHook_ApplyInstalls(t *testing.T) {
	inj, declared, srv := provisioningInjector(t)
	h := &provisioningHook{inj: inj}

	status, err := h.Apply(context.Background(), nil, corsBucketConfig("https://app.example.com"))
	if status != HookApplied || err != nil {
		t.Fatalf("Apply = (%s, %v), want (applied, nil)", status, err)
	}
	if !slices.Contains(declared.Names(), "photos") {
		t.Errorf("declared = %v, want photos published", declared.Names())
	}
	if srv.GetBucketAuth() == nil {
		t.Error("credential registry not installed")
	}
}

// TestProvisioningHook_ApplyFailsOnBrokenServer proves a broken S3-server
// provider surfaces as HookFailed rather than being read as "feature off".
func TestProvisioningHook_ApplyFailsOnBrokenServer(t *testing.T) {
	inj := do.New()
	emptyProvisioningStore(t, inj)
	do.ProvideValue(inj, provisioning.NewDeclared())
	do.ProvideValue(inj, config.ModeAPI)
	failingProvider[*s3api.Server](inj)
	do.ProvideValue(inj, cors.New(s3api.BucketFromPath, s3api.WriteS3Error))

	status, err := (&provisioningHook{inj: inj}).Apply(context.Background(), nil, &config.Config{})
	if status != HookFailed || !errors.Is(err, errResolve) {
		t.Fatalf("Apply = (%s, %v), want failed wrapping errResolve", status, err)
	}
}

// corsBucketConfig is a config declaring one bucket, photos, with a CORS rule
// allowing origin.
func corsBucketConfig(origin string) *config.Config {
	return &config.Config{Buckets: []config.BucketConfig{{
		Name: "photos",
		CORS: []config.CORSRule{{AllowedOrigins: []string{origin}, AllowedMethods: []string{"GET"}}},
	}}}
}

// TestRateLimitHook_FeatureDisabled covers the early-return when the
// new config has rate limiting turned off; no DI lookup happens.
func TestRateLimitHook_FeatureDisabled(t *testing.T) {
	h := &rateLimitHook{inj: do.New()}
	cfg := &config.Config{}
	cfg.RateLimit.Enabled = false
	status, err := h.Apply(context.Background(), nil, cfg)
	if status != HookSkipped || err != nil {
		t.Fatalf("Apply = (%s, %v), want (skipped, nil)", status, err)
	}
}

// TestRateLimitHook_FailedResolution drives the Failed branch by
// registering a constructor that errors with the feature enabled.
func TestRateLimitHook_FailedResolution(t *testing.T) {
	inj := do.New()
	failingProvider[*s3api.RateLimiter](inj)
	h := &rateLimitHook{inj: inj}
	cfg := &config.Config{}
	cfg.RateLimit.Enabled = true
	status, err := h.Apply(context.Background(), nil, cfg)
	if status != HookFailed || !errors.Is(err, errResolve) {
		t.Fatalf("Apply = (%s, %v), want failed wrapping errResolve", status, err)
	}
}

// TestRateLimitHook_SkippedWhenDisabledProvider covers the Skipped
// outcome when the feature is on in config but no provider is wired
// (e.g. a worker-only run mode).
func TestRateLimitHook_SkippedWhenDisabledProvider(t *testing.T) {
	h := &rateLimitHook{inj: do.New()}
	cfg := &config.Config{}
	cfg.RateLimit.Enabled = true
	status, err := h.Apply(context.Background(), nil, cfg)
	if status != HookSkipped || err != nil {
		t.Fatalf("Apply = (%s, %v), want (skipped, nil)", status, err)
	}
}

// TestQuotaSyncHook_FailedResolution drives the Failed branch.
func TestQuotaSyncHook_FailedResolution(t *testing.T) {
	inj := do.New()
	failingProvider[core.LifecycleAdmin](inj)
	h := &quotaSyncHook{inj: inj}
	status, err := h.Apply(context.Background(), nil, &config.Config{})
	if status != HookFailed || !errors.Is(err, errResolve) {
		t.Fatalf("Apply = (%s, %v), want failed wrapping errResolve", status, err)
	}
}

// TestQuotaSyncHook_SkippedWhenDisabled covers the no-provider path.
func TestQuotaSyncHook_SkippedWhenDisabled(t *testing.T) {
	h := &quotaSyncHook{inj: do.New()}
	status, err := h.Apply(context.Background(), nil, &config.Config{})
	if status != HookSkipped || err != nil {
		t.Fatalf("Apply = (%s, %v), want (skipped, nil)", status, err)
	}
}

// TestUsageLimitsHook_FailedResolution drives the Failed branch.
func TestUsageLimitsHook_FailedResolution(t *testing.T) {
	inj := do.New()
	failingProvider[*infra.BackendRuntime](inj)
	h := &usageLimitsHook{inj: inj}
	status, err := h.Apply(context.Background(), nil, &config.Config{})
	if status != HookFailed || !errors.Is(err, errResolve) {
		t.Fatalf("Apply = (%s, %v), want failed wrapping errResolve", status, err)
	}
}

// TestUsageLimitsHook_SkippedWhenDisabled covers the no-provider path.
func TestUsageLimitsHook_SkippedWhenDisabled(t *testing.T) {
	h := &usageLimitsHook{inj: do.New()}
	status, err := h.Apply(context.Background(), nil, &config.Config{})
	if status != HookSkipped || err != nil {
		t.Fatalf("Apply = (%s, %v), want (skipped, nil)", status, err)
	}
}

// TestLogLevelHook_NilLevelSkipped covers the no-level-pointer path.
func TestLogLevelHook_NilLevelSkipped(t *testing.T) {
	h := &logLevelHook{}
	status, err := h.Apply(context.Background(), nil, &config.Config{})
	if status != HookSkipped || err != nil {
		t.Fatalf("Apply = (%s, %v), want (skipped, nil)", status, err)
	}
}

// TestLogLevelHook_Applied confirms the new level is pushed onto the
// passed pointer.
func TestLogLevelHook_Applied(t *testing.T) {
	var lv slog.LevelVar
	lv.Set(slog.LevelInfo)
	cfg := &config.Config{}
	cfg.Server.LogLevel = "debug"
	h := &logLevelHook{level: &lv}
	status, err := h.Apply(context.Background(), nil, cfg)
	if status != HookApplied || err != nil {
		t.Fatalf("Apply = (%s, %v), want (applied, nil)", status, err)
	}
	if lv.Level() != slog.LevelDebug {
		t.Errorf("level = %s, want debug", lv.Level())
	}
}

// TestQuotaMetricsHook_FailedResolution drives the Failed branch.
func TestQuotaMetricsHook_FailedResolution(t *testing.T) {
	inj := do.New()
	failingProvider[*infra.BackendRuntime](inj)
	h := &quotaMetricsHook{inj: inj}
	status, err := h.Apply(context.Background(), nil, &config.Config{})
	if status != HookFailed || !errors.Is(err, errResolve) {
		t.Fatalf("Apply = (%s, %v), want failed wrapping errResolve", status, err)
	}
}

// TestRateLimitHook_AppliedPushesNewLimits wires a real rate limiter
// through the injector with the feature enabled and confirms Apply
// pushes the new limits and returns HookApplied.
func TestRateLimitHook_AppliedPushesNewLimits(t *testing.T) {
	inj := do.New()
	rl := s3api.NewRateLimiter(config.RateLimitConfig{
		RequestsPerSec: 1,
		Burst:          1,
	})
	t.Cleanup(rl.Close)
	do.Provide(inj, func(do.Injector) (*s3api.RateLimiter, error) { return rl, nil })
	h := &rateLimitHook{inj: inj}
	cfg := &config.Config{}
	cfg.RateLimit.Enabled = true
	cfg.RateLimit.RequestsPerSec = 10
	cfg.RateLimit.Burst = 20
	status, err := h.Apply(context.Background(), nil, cfg)
	if status != HookApplied || err != nil {
		t.Fatalf("Apply = (%s, %v), want (applied, nil)", status, err)
	}
}

// TestHookNamesStable pins the Name() strings every hook reports.
// Operators read these from admin status output; renaming one is a
// breaking change that should be deliberate.
func TestHookNamesStable(t *testing.T) {
	cases := map[Hook]string{
		&tlsCertHook{}:      "tls_certificate",
		&provisioningHook{}: "provisioning",
		&rateLimitHook{}:    "rate_limit",
		&quotaSyncHook{}:    "quota_sync",
		&usageLimitsHook{}:  "usage_limits",
		&logLevelHook{}:     "log_level",
		&quotaMetricsHook{}: "quota_metrics",
	}
	for h, want := range cases {
		if got := h.Name(); got != want {
			t.Errorf("Name = %q, want %q", got, want)
		}
		if err := h.Check(nil, nil); err != nil {
			t.Errorf("%s Check returned %v, want nil", want, err)
		}
	}
}

// duplicateKeyConfig is a config whose buckets both claim one access key.
func duplicateKeyConfig() *config.Config {
	return &config.Config{Buckets: []config.BucketConfig{
		{Name: "backups", Credentials: []config.CredentialConfig{
			{AccessKeyID: "SAME", SecretAccessKey: "backups-secret"},
		}},
		{Name: "traces", Credentials: []config.CredentialConfig{
			{AccessKeyID: "SAME", SecretAccessKey: "traces-secret"},
		}},
	}}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// emptyProvisioningStore registers a store answering every listing empty, which
// the hook reads because a reload loads the provisioning view from config
// merged with the store. Without it a hook fails on the missing dependency
// rather than on the config it was handed, and a test asserting rejection
// would pass for the wrong reason.
func emptyProvisioningStore(t *testing.T, inj do.Injector) {
	t.Helper()
	s := storetest.NewMockProvisioningStore(gomock.NewController(t))
	a := gomock.Any()
	s.EXPECT().ListBuckets(a).Return(nil, nil).AnyTimes()
	s.EXPECT().ListUsers(a).Return(nil, nil).AnyTimes()
	s.EXPECT().ListCredentials(a).Return(nil, nil).AnyTimes()
	s.EXPECT().ListGrants(a).Return(nil, nil).AnyTimes()
	do.ProvideValue[core.ProvisioningStore](inj, s)
}

// provisioningInjector registers everything the provisioning hook loads from
// and installs into, in a mode serving S3, and returns the views a test reads.
func provisioningInjector(t *testing.T) (do.Injector, *provisioning.Declared, *s3api.Server) {
	t.Helper()
	inj := do.New()
	emptyProvisioningStore(t, inj)
	declared, srv := provisioning.NewDeclared(), &s3api.Server{}
	do.ProvideValue(inj, declared)
	do.ProvideValue(inj, srv)
	do.ProvideValue(inj, config.ModeAPI)
	do.ProvideValue(inj, cors.New(s3api.BucketFromPath, s3api.WriteS3Error))
	return inj, declared, srv
}

// TestProvisioningHook_CheckChangesNothing proves the Check pass publishes
// nothing, whether it accepts the config or refuses it: an ambiguous credential
// or an unreadable CORS rule aborts the reload with every running view as it
// was, and a valid config waits for Apply.
func TestProvisioningHook_CheckChangesNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg     *config.Config
		wantErr bool
	}{
		"valid":                {corsBucketConfig("https://app.example.com"), false},
		"ambiguous credential": {duplicateKeyConfig(), true},
		"unreadable CORS rule": {corsBucketConfig("https://*.*.example.com"), true},
	} {
		t.Run(name, func(t *testing.T) {
			inj, declared, srv := provisioningInjector(t)
			err := (&provisioningHook{inj: inj}).Check(&config.Config{}, tc.cfg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Check = %v, want error %v", err, tc.wantErr)
			}
			if len(declared.Names()) != 0 || srv.GetBucketAuth() != nil {
				t.Errorf("Check published: declared %v, registry %v", declared.Names(), srv.GetBucketAuth())
			}
		})
	}
}

// TestProvisioningHook_ApplyRejectsAmbiguousCredential covers the Apply path
// directly, so a caller that skipped Check still cannot install a registry
// built from an ambiguous credential.
func TestProvisioningHook_ApplyRejectsAmbiguousCredential(t *testing.T) {
	inj, _, srv := provisioningInjector(t)
	status, err := (&provisioningHook{inj: inj}).Apply(context.Background(), nil, duplicateKeyConfig())
	if status != HookFailed || err == nil || srv.GetBucketAuth() != nil {
		t.Fatalf("Apply = (%s, %v), registry %v; want failed and nothing installed", status, err, srv.GetBucketAuth())
	}
}
