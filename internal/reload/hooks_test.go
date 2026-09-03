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

	"go.uber.org/mock/gomock"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/proxy/usage"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
	"github.com/afreidah/s3-orchestrator/internal/transport/ui"
	"github.com/afreidah/s3-orchestrator/internal/worker"
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

// TestWorkerConfigsHook_PushesIntegrityOntoTheReplicator covers the section the
// replicator gained with verify_on_replicate. The replicator reads two config
// sections now, and a reload that refreshed only Replication would leave replica
// verification pinned to whatever it was at startup while the operator watched
// SIGHUP report success.
func TestWorkerConfigsHook_PushesIntegrityOntoTheReplicator(t *testing.T) {
	mock := storetest.NewMockMetadataStore(gomock.NewController(t))
	st := proxytest.New(t, mock, &proxytest.StackOptions{
		Runtime: proxytest.NewRuntime(&proxytest.RuntimeOptions{
			Backends:        map[string]backend.ObjectBackend{},
			Order:           []string{},
			RoutingStrategy: config.RoutingPack,
			Metrics:         mock,
		}),
	})
	workers := proxytest.BuildWorkers(st, mock)

	inj := do.New()
	do.ProvideValue(inj, workers.Replicator)

	newCfg := &config.Config{
		Replication: config.ReplicationConfig{Factor: 3},
		Integrity:   config.IntegrityConfig{Enabled: true, VerifyOnReplicate: true},
	}
	h := &workerConfigsHook{inj: inj}
	status, err := h.Apply(context.Background(), nil, newCfg)
	if status != HookApplied || err != nil {
		t.Fatalf("Apply = (%s, %v), want (applied, nil)", status, err)
	}

	if got := workers.Replicator.Config().Factor; got != 3 {
		t.Errorf("replication factor = %d, want 3", got)
	}
	icfg := workers.Replicator.IntegrityConfig()
	if icfg == nil || !icfg.ShouldVerifyOnReplicate() {
		t.Errorf("integrity config = %+v, want replica verification on", icfg)
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
	failingProvider[*usage.Service](inj)
	h := &runtimeConfigHook{inj: inj}
	status, err := h.Apply(context.Background(), nil, &config.Config{})
	if status != HookFailed || !errors.Is(err, errResolve) {
		t.Fatalf("Apply = (%s, %v), want failed wrapping errResolve", status, err)
	}
}

// TestManagerConfigHook_SkippedWhenDisabled covers the no-provider path.
func TestManagerConfigHook_SkippedWhenDisabled(t *testing.T) {
	h := &runtimeConfigHook{inj: do.New()}
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
		return ui.New(newUIDepsForReloadTest(t)), nil
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
		&tlsCertHook{}:       "tls_certificate",
		&bucketAuthHook{}:    "bucket_credentials",
		&rateLimitHook{}:     "rate_limit",
		&quotaSyncHook{}:     "quota_sync",
		&usageLimitsHook{}:   "usage_limits",
		&logLevelHook{}:      "log_level",
		&workerConfigsHook{}: "worker_configs",
		&runtimeConfigHook{}: "runtime_config",
		&opsHook{}:           "ops",
		&uiHandlerHook{}:     "ui_handler",
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

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// newUIDepsForReloadTest wires the minimum real deps the ui handler
// constructor requires. The reload-hook test only exercises UpdateConfig,
// but the constructor still panics via must.NotNil on missing deps.
func newUIDepsForReloadTest(t *testing.T) *ui.Deps {
	t.Helper()
	mock := storetest.NewMockMetadataStore(gomock.NewController(t))
	st := proxytest.New(t, mock, &proxytest.StackOptions{
		Runtime: proxytest.NewRuntime(&proxytest.RuntimeOptions{
			Backends:        map[string]backend.ObjectBackend{},
			Order:           []string{},
			RoutingStrategy: config.RoutingPack,
			Metrics:         mock,
		}),
	})
	workers := proxytest.BuildWorkers(st, mock)
	svc := ops.New(&ops.Deps{
		Objects:      st.Objects,
		Store:        mock,
		EncStore:     mock,
		Runtime:      st.Runtime,
		Usage:        st.Runtime.Usage(),
		IntegrityCfg: st.IntegrityCfg,
		Replicator:   workers.Replicator,
		OverRep:      workers.OverReplicationCleaner,
		Rebalancer:   workers.Rebalancer,
		Scrubber:     workers.Scrubber,
		Cfg:          &config.Config{},
	})
	return &ui.Deps{
		Objects:     svc.Objects,
		Integrity:   svc.Integrity,
		Replication: svc.Replication,
		Rebalance:   svc.Rebalance,
		Encryption:  svc.Encryption,
		Compression: svc.Compression,
		Cfg:         &config.Config{},
	}
}

// duplicateTokenConfig is a config whose buckets both claim one proxy token.
func duplicateTokenConfig() *config.Config {
	return &config.Config{Buckets: []config.BucketConfig{
		{Name: "backups", Credentials: []config.CredentialConfig{{Token: "SAME"}}},
		{Name: "traces", Credentials: []config.CredentialConfig{{Token: "SAME"}}},
	}}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestBucketAuthHook_CheckRejectsAmbiguousCredential proves an ambiguous
// credential is caught in the Check pass. That is what matters for safety:
// Reload aborts before any hook applies, so the server keeps serving with the
// registry it already had rather than one where a token resolves to whichever
// bucket happened to be written last.
func TestBucketAuthHook_CheckRejectsAmbiguousCredential(t *testing.T) {
	h := &bucketAuthHook{inj: do.New()}
	if err := h.Check(&config.Config{}, duplicateTokenConfig()); err == nil {
		t.Fatal("Check must reject a token claimed by two buckets")
	}
}

// TestBucketAuthHook_CheckAcceptsDistinctCredentials verifies the Check pass
// stays out of the way of a valid reload.
func TestBucketAuthHook_CheckAcceptsDistinctCredentials(t *testing.T) {
	h := &bucketAuthHook{inj: do.New()}
	cfg := &config.Config{Buckets: []config.BucketConfig{
		{Name: "backups", Credentials: []config.CredentialConfig{{Token: "one"}}},
		{Name: "traces", Credentials: []config.CredentialConfig{{Token: "two"}}},
	}}
	if err := h.Check(&config.Config{}, cfg); err != nil {
		t.Fatalf("Check rejected a valid config: %v", err)
	}
}

// TestBucketAuthHook_ApplyRejectsAmbiguousCredential covers the Apply path
// directly, so a caller that skipped Check still cannot install a registry
// built from an ambiguous credential.
func TestBucketAuthHook_ApplyRejectsAmbiguousCredential(t *testing.T) {
	inj := do.New()
	do.ProvideValue(inj, &s3api.Server{})
	h := &bucketAuthHook{inj: inj}
	status, err := h.Apply(context.Background(), nil, duplicateTokenConfig())
	if status != HookFailed || err == nil {
		t.Fatalf("Apply = (%s, %v), want (failed, error)", status, err)
	}
}
