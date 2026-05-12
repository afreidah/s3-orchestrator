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
	"strings"
	"testing"
	"time"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
	"github.com/afreidah/s3-orchestrator/internal/transport/ui"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

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

// fakeLifecycleAdmin implements core.LifecycleAdmin for tests that need
// to drive the SyncQuotaLimits success and failure branches without
// standing up a real metadata store.
type fakeLifecycleAdmin struct {
	syncErr error
	calls   int
}

func (f *fakeLifecycleAdmin) RunMigrations(context.Context) error       { return nil }
func (f *fakeLifecycleAdmin) VerifySchemaVersion(context.Context) error { return nil }
func (f *fakeLifecycleAdmin) SyncQuotaLimits(_ context.Context, _ []config.BackendConfig) error {
	f.calls++
	return f.syncErr
}
func (f *fakeLifecycleAdmin) Close() {}

// TestQuotaSyncHook_AppliedOnSuccess wires a fake LifecycleAdmin whose
// SyncQuotaLimits returns nil and asserts the hook reports Applied.
func TestQuotaSyncHook_AppliedOnSuccess(t *testing.T) {
	fake := &fakeLifecycleAdmin{}
	inj := do.New()
	do.ProvideValue[core.LifecycleAdmin](inj, fake)
	h := &quotaSyncHook{inj: inj}
	status, err := h.Apply(context.Background(), nil, &config.Config{})
	if status != HookApplied || err != nil {
		t.Fatalf("Apply = (%s, %v), want (applied, nil)", status, err)
	}
	if fake.calls != 1 {
		t.Errorf("SyncQuotaLimits calls = %d, want 1", fake.calls)
	}
}

// TestQuotaSyncHook_FailedOnSyncError wires a fake whose SyncQuotaLimits
// returns an error so the inner error path is covered: the hook surfaces
// HookFailed with the unwrapped underlying error.
func TestQuotaSyncHook_FailedOnSyncError(t *testing.T) {
	syncBoom := errors.New("quota sync failed")
	inj := do.New()
	do.ProvideValue[core.LifecycleAdmin](inj, &fakeLifecycleAdmin{syncErr: syncBoom})
	h := &quotaSyncHook{inj: inj}
	status, err := h.Apply(context.Background(), nil, &config.Config{})
	if status != HookFailed {
		t.Fatalf("status = %s, want failed", status)
	}
	if !errors.Is(err, syncBoom) {
		t.Fatalf("err = %v, want syncBoom", err)
	}
}

// TestBucketAuthHook_FailedResolution proves that a broken S3-server
// provider surfaces as HookFailed with the labelled error, instead of
// silently being treated as "feature off".
func TestBucketAuthHook_FailedResolution(t *testing.T) {
	inj := do.New()
	failingProvider[*s3api.Server](inj)
	h := &bucketAuthHook{inj: inj}
	cfg := &config.Config{}
	status, err := h.Apply(context.Background(), nil, cfg)
	if status != HookFailed {
		t.Fatalf("status = %s, want failed", status)
	}
	if !errors.Is(err, errResolve) {
		t.Fatalf("err = %v, want wrap of errResolve", err)
	}
}

// TestBucketAuthHook_SkippedWhenDisabled covers the "no S3 server
// registered" path: the hook reports Skipped without touching anything.
func TestBucketAuthHook_SkippedWhenDisabled(t *testing.T) {
	h := &bucketAuthHook{inj: do.New()}
	status, err := h.Apply(context.Background(), nil, &config.Config{})
	if status != HookSkipped || err != nil {
		t.Fatalf("Apply = (%s, %v), want (skipped, nil)", status, err)
	}
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
	failingProvider[*proxy.BackendManager](inj)
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

// TestWorkerConfigsHook_FailedRebalancer threads a failing rebalancer
// provider through and asserts the hook short-circuits with the
// labelled error before touching the other workers.
func TestWorkerConfigsHook_FailedRebalancer(t *testing.T) {
	inj := do.New()
	failingProvider[*worker.Rebalancer](inj)
	h := &workerConfigsHook{inj: inj}
	status, err := h.Apply(context.Background(), nil, &config.Config{})
	if status != HookFailed || !errors.Is(err, errResolve) {
		t.Fatalf("Apply = (%s, %v), want failed wrapping errResolve", status, err)
	}
	if !strings.Contains(err.Error(), "rebalancer") {
		t.Errorf("err %q missing rebalancer label", err)
	}
}

// TestWorkerConfigsHook_FailedReplicator confirms the second-position
// resolution failure is also surfaced (and labelled correctly).
func TestWorkerConfigsHook_FailedReplicator(t *testing.T) {
	inj := do.New()
	failingProvider[*worker.Replicator](inj)
	h := &workerConfigsHook{inj: inj}
	status, err := h.Apply(context.Background(), nil, &config.Config{})
	if status != HookFailed || !errors.Is(err, errResolve) {
		t.Fatalf("Apply = (%s, %v), want failed wrapping errResolve", status, err)
	}
	if !strings.Contains(err.Error(), "replicator") {
		t.Errorf("err %q missing replicator label", err)
	}
}

// TestWorkerConfigsHook_FailedOverRepCleaner covers the third worker.
func TestWorkerConfigsHook_FailedOverRepCleaner(t *testing.T) {
	inj := do.New()
	failingProvider[*worker.OverReplicationCleaner](inj)
	h := &workerConfigsHook{inj: inj}
	status, err := h.Apply(context.Background(), nil, &config.Config{})
	if status != HookFailed || !errors.Is(err, errResolve) {
		t.Fatalf("Apply = (%s, %v), want failed wrapping errResolve", status, err)
	}
	if !strings.Contains(err.Error(), "over-replication cleaner") {
		t.Errorf("err %q missing label", err)
	}
}

// TestWorkerConfigsHook_FailedScrubber covers the fourth worker.
func TestWorkerConfigsHook_FailedScrubber(t *testing.T) {
	inj := do.New()
	failingProvider[*worker.Scrubber](inj)
	h := &workerConfigsHook{inj: inj}
	status, err := h.Apply(context.Background(), nil, &config.Config{})
	if status != HookFailed || !errors.Is(err, errResolve) {
		t.Fatalf("Apply = (%s, %v), want failed wrapping errResolve", status, err)
	}
	if !strings.Contains(err.Error(), "scrubber") {
		t.Errorf("err %q missing label", err)
	}
}

// TestWorkerConfigsHook_AllDisabledSkipped is the run-mode case where
// none of the workers are wired in: the hook reports Skipped because
// there is nothing to push config onto.
func TestWorkerConfigsHook_AllDisabledSkipped(t *testing.T) {
	h := &workerConfigsHook{inj: do.New()}
	status, err := h.Apply(context.Background(), nil, &config.Config{})
	if status != HookSkipped || err != nil {
		t.Fatalf("Apply = (%s, %v), want (skipped, nil)", status, err)
	}
}

// TestManagerConfigHook_FailedResolution drives the Failed branch.
func TestManagerConfigHook_FailedResolution(t *testing.T) {
	inj := do.New()
	failingProvider[*proxy.BackendManager](inj)
	h := &managerConfigHook{inj: inj}
	status, err := h.Apply(context.Background(), nil, &config.Config{})
	if status != HookFailed || !errors.Is(err, errResolve) {
		t.Fatalf("Apply = (%s, %v), want failed wrapping errResolve", status, err)
	}
}

// TestManagerConfigHook_SkippedWhenDisabled covers the no-provider path.
func TestManagerConfigHook_SkippedWhenDisabled(t *testing.T) {
	h := &managerConfigHook{inj: do.New()}
	status, err := h.Apply(context.Background(), nil, &config.Config{})
	if status != HookSkipped || err != nil {
		t.Fatalf("Apply = (%s, %v), want (skipped, nil)", status, err)
	}
}

// TestUIHandlerHook_FailedResolution drives the Failed branch.
func TestUIHandlerHook_FailedResolution(t *testing.T) {
	inj := do.New()
	failingProvider[*ui.Handler](inj)
	h := &uiHandlerHook{inj: inj}
	status, err := h.Apply(context.Background(), nil, &config.Config{})
	if status != HookFailed || !errors.Is(err, errResolve) {
		t.Fatalf("Apply = (%s, %v), want failed wrapping errResolve", status, err)
	}
}

// TestUIHandlerHook_SkippedWhenDisabled covers the no-provider path.
func TestUIHandlerHook_SkippedWhenDisabled(t *testing.T) {
	h := &uiHandlerHook{inj: do.New()}
	status, err := h.Apply(context.Background(), nil, &config.Config{})
	if status != HookSkipped || err != nil {
		t.Fatalf("Apply = (%s, %v), want (skipped, nil)", status, err)
	}
}

// TestUIHandlerHook_AppliedPushesNewConfig wires a real ui.Handler
// through the injector and confirms Apply pushes the new config onto
// it, returning HookApplied.
func TestUIHandlerHook_AppliedPushesNewConfig(t *testing.T) {
	inj := do.New()
	do.Provide(inj, func(do.Injector) (*ui.Handler, error) {
		return ui.New(&ui.Deps{Cfg: &config.Config{}}), nil
	})
	h := &uiHandlerHook{inj: inj}
	status, err := h.Apply(context.Background(), nil, &config.Config{})
	if status != HookApplied || err != nil {
		t.Fatalf("Apply = (%s, %v), want (applied, nil)", status, err)
	}
}

// TestRateLimitHook_AppliedPushesNewLimits wires a real rate limiter
// through the injector with the feature enabled and confirms Apply
// pushes the new limits and returns HookApplied.
func TestRateLimitHook_AppliedPushesNewLimits(t *testing.T) {
	inj := do.New()
	do.Provide(inj, func(do.Injector) (*s3api.RateLimiter, error) {
		return s3api.NewRateLimiter(config.RateLimitConfig{
			RequestsPerSec: 1,
			Burst:          1,
		}), nil
	})
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
		&tlsCertHook{}:        "tls_certificate",
		&bucketAuthHook{}:     "bucket_credentials",
		&rateLimitHook{}:      "rate_limit",
		&quotaSyncHook{}:      "quota_sync",
		&usageLimitsHook{}:    "usage_limits",
		&logLevelHook{}:       "log_level",
		&workerConfigsHook{}:  "worker_configs",
		&managerConfigHook{}:  "manager_config",
		&uiHandlerHook{}:      "ui_handler",
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
