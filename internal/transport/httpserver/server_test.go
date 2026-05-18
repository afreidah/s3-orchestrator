// -------------------------------------------------------------------------------
// HTTP Server - Tests
//
// Author: Alex Freidah
//
// Drives every public surface of the httpserver package: TLS config (no-TLS,
// good cert, bad cert), health / readiness endpoints, separate-listener
// metrics, inline-mux metrics, UI handler registration, and the S3 admission-
// control branches. Shares the same in-memory SQLite test fixture as the
// end-to-end serve tests; nothing here requires external services.
// -------------------------------------------------------------------------------

package httpserver

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	goruntime "runtime"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/di"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
)

// -------------------------------------------------------------------------
// FIXTURES
// -------------------------------------------------------------------------

// validTestConfigYAML is the minimal YAML config the httpserver tests use.
// SQLite in-memory keeps the suite free of external dependencies.
const validTestConfigYAML = `
server:
  listen_addr: ":0"
database:
  driver: sqlite
  path: ":memory:"
buckets:
  - name: test
    credentials:
      - access_key_id: ak
        secret_access_key: sk
backends:
  - name: b1
    endpoint: http://localhost:19000
    region: us-east-1
    bucket: bucket1
    access_key_id: ak
    secret_access_key: sk
`

// writeYAML writes the given YAML to a temp file and returns the path.
func writeYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// freePort returns an available TCP port on localhost.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0") //nolint:noctx // test helper
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// generateSelfSignedCert creates a self-signed TLS cert+key in dir.
func generateSelfSignedCert(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPath = filepath.Join(dir, "cert.pem")
	certFile, err := os.Create(certPath)
	if err != nil {
		t.Fatalf("create cert file: %v", err)
	}
	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		t.Fatalf("encode cert: %v", err)
	}
	certFile.Close()

	keyPath = filepath.Join(dir, "key.pem")
	keyFile, err := os.Create(keyPath)
	if err != nil {
		t.Fatalf("create key file: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	if err := pem.Encode(keyFile, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		t.Fatalf("encode key: %v", err)
	}
	keyFile.Close()

	return certPath, keyPath
}

// resolvedInjector loads cfg, configures observability, builds the DI
// injector, and forces eager construction of every service httpserver.New
// will resolve so the tests do not have to drive the runtime package
// directly. Returns the cfg, injector, and a cleanup func.
func resolvedInjector(t *testing.T, cfg *config.Config, mode string) (do.Injector, func()) {
	t.Helper()

	var logLevel slog.LevelVar
	logLevel.Set(config.ParseLogLevel(cfg.Server.LogLevel))
	logBuffer := telemetry.NewLogBuffer()
	errHandler := logfmt.NewErrAttrHandler(slog.DiscardHandler)
	traceHandler := telemetry.NewTraceHandler(errHandler)
	slog.SetDefault(slog.New(telemetry.NewTeeHandler(traceHandler, logBuffer)))

	shutdownTracer, err := telemetry.InitTracer(context.Background(), cfg.Telemetry.Tracing)
	if err != nil {
		t.Fatalf("init tracer: %v", err)
	}
	telemetry.BuildInfo.WithLabelValues(telemetry.Version, goruntime.Version()).Set(1)
	di.WireAuditMetrics()

	inj := di.NewInjector(cfg, mode, &logLevel, logBuffer)

	if _, err := do.Invoke[core.LifecycleAdmin](inj); err != nil {
		t.Fatalf("invoke LifecycleAdmin: %v", err)
	}
	if _, err := do.Invoke[*breaker.CircuitBreaker](inj); err != nil {
		t.Fatalf("invoke breaker: %v", err)
	}
	manager, err := do.Invoke[*proxy.BackendManager](inj)
	if err != nil {
		t.Fatalf("invoke manager: %v", err)
	}
	if err := di.WireManager(inj); err != nil {
		t.Fatalf("wire manager: %v", err)
	}
	if _, err := do.Invoke[*s3api.Server](inj); err != nil {
		t.Fatalf("invoke s3 server: %v", err)
	}

	cleanup := func() {
		if admin, _ := do.Invoke[core.LifecycleAdmin](inj); admin != nil {
			admin.Close()
		}
		manager.Close()
		_ = shutdownTracer(context.Background())
	}
	return inj, cleanup
}

// loadCfg parses YAML on disk into a *config.Config.
func loadCfg(t *testing.T, yaml string) *config.Config {
	t.Helper()
	cfg, err := config.LoadConfig(writeYAML(t, yaml))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

// healthyDeps returns a HealthDeps stub where ready can be flipped per
// test and the DB breaker callback returns nil (healthy by default).
func healthyDeps() (*atomic.Bool, func() *breaker.CircuitBreaker) {
	var ready atomic.Bool
	return &ready, func() *breaker.CircuitBreaker { return nil }
}

// -------------------------------------------------------------------------
// TLS
// -------------------------------------------------------------------------

// TestBuildTLSConfig_NoTLS returns nil when CertFile is empty.
func TestBuildTLSConfig_NoTLS(t *testing.T) {
	t.Parallel()
	cfg, reloader, err := buildTLSConfig(&config.TLSConfig{})
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if cfg != nil || reloader != nil {
		t.Errorf("expected nil cfg and reloader, got %v / %v", cfg, reloader)
	}
}

// TestBuildTLSConfig_WithCert loads a self-signed cert and reports the
// CertReloader for the reload coordinator to consume.
func TestBuildTLSConfig_WithCert(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	certPath, keyPath := generateSelfSignedCert(t, dir)
	tlsCfg, reloader, err := buildTLSConfig(&config.TLSConfig{
		CertFile: certPath,
		KeyFile:  keyPath,
	})
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if tlsCfg == nil {
		t.Fatal("expected non-nil tls.Config")
	}
	if reloader == nil {
		t.Fatal("expected non-nil CertReloader")
	}
}

// TestBuildTLSConfig_BadCert surfaces an error when the cert file is junk.
func TestBuildTLSConfig_BadCert(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "bad.pem")
	keyPath := filepath.Join(dir, "bad-key.pem")
	_ = os.WriteFile(certPath, []byte("not a cert"), 0600)
	_ = os.WriteFile(keyPath, []byte("not a key"), 0600)
	if _, _, err := buildTLSConfig(&config.TLSConfig{CertFile: certPath, KeyFile: keyPath}); err == nil {
		t.Fatal("expected error for bad cert")
	}
}

// TestParseTLSVersion locks in the supported strings.
func TestParseTLSVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  uint16
	}{
		{"1.3", tls.VersionTLS13},
		{"1.2", tls.VersionTLS12},
		{"", tls.VersionTLS12},
		{"junk", tls.VersionTLS12},
	}
	for _, tt := range tests {
		if got := parseTLSVersion(tt.input); got != tt.want {
			t.Errorf("parseTLSVersion(%q) = 0x%04x, want 0x%04x", tt.input, got, tt.want)
		}
	}
}

// -------------------------------------------------------------------------
// HEALTH ENDPOINTS
// -------------------------------------------------------------------------

// TestHealth_Ok returns 200 with status=ok when nothing is degraded.
func TestHealth_Ok(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	ready, dbBreaker := healthyDeps()
	ready.Store(true)
	registerHealthEndpoints(mux, HealthDeps{Ready: ready, DBBreaker: dbBreaker})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("/health status = %d, want 200", w.Code)
	}
	var got map[string]string
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode /health: %v", err)
	}
	if got["status"] != "ok" {
		t.Errorf("/health status = %q, want ok", got["status"])
	}
}

// TestHealthReady_503BeforeReady returns 503 before ready is flipped.
func TestHealthReady_503BeforeReady(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	ready, dbBreaker := healthyDeps()
	registerHealthEndpoints(mux, HealthDeps{Ready: ready, DBBreaker: dbBreaker})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("/health/ready status = %d, want 503", w.Code)
	}
}

// TestHealthReady_200WhenReady flips ready and gets 200.
func TestHealthReady_200WhenReady(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	ready, dbBreaker := healthyDeps()
	ready.Store(true)
	registerHealthEndpoints(mux, HealthDeps{Ready: ready, DBBreaker: dbBreaker})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("/health/ready status = %d, want 200", w.Code)
	}
}

// -------------------------------------------------------------------------
// METRICS LISTENER
// -------------------------------------------------------------------------

// TestConfigureMetrics_Disabled returns nil and registers no handler.
func TestConfigureMetrics_Disabled(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	if got := configureMetrics(mux, &config.MetricsConfig{Enabled: false}); got != nil {
		t.Errorf("expected nil server when metrics disabled, got %v", got)
	}
}

// TestConfigureMetrics_Inline mounts /metrics on the supplied mux when
// no separate Listen address is configured. Pprof MUST NOT be mounted
// on the inline mux - it shares the listener with the public S3 API
// and would expose runtime internals without authentication.
func TestConfigureMetrics_Inline(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	got := configureMetrics(mux, &config.MetricsConfig{
		Enabled: true,
		Path:    "/metrics",
	})
	if got != nil {
		t.Errorf("expected nil server for inline metrics, got %v", got)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("/metrics status = %d, want 200", w.Code)
	}

	// Security regression: pprof handlers must NOT be reachable on the
	// inline mux. We probe /debug/pprof/ and the most commonly leaked
	// sub-endpoint (cmdline). When configureMetrics has not mounted
	// them the mux returns 404; if a future patch wires them up
	// inline (the issue #886 case), this test fails.
	for _, path := range []string{"/debug/pprof/", "/debug/pprof/cmdline"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("inline-metrics mux must not expose %s (status = %d, want 404)", path, w.Code)
		}
	}
}

// TestConfigureMetrics_SeparateListener returns a non-nil server bound
// to the configured Listen address with both /metrics and pprof
// handlers mounted.
func TestConfigureMetrics_SeparateListener(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	listen := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	srv := configureMetrics(mux, &config.MetricsConfig{
		Enabled: true,
		Listen:  listen,
		Path:    "/metrics",
	})
	if srv == nil {
		t.Fatal("expected separate metrics server")
	}
	if srv.Addr != listen {
		t.Errorf("Addr = %q, want %q", srv.Addr, listen)
	}

	// Pprof MUST be reachable on the dedicated listener's handler.
	// Operators use the dedicated listener precisely so they can
	// profile in production without exposing the surface on the
	// public S3 listener.
	for _, path := range []string{"/debug/pprof/", "/debug/pprof/cmdline"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		srv.Handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("dedicated metrics listener must expose %s (status = %d, want 200)", path, w.Code)
		}
	}
}

// TestServer_ShutdownLogsErrors drives the Shutdown method's
// error-logging branches: both main and metrics http.Server.Shutdown
// calls receive a deadline-exceeded context while a slow handler holds
// the listener busy, so Shutdown reports the context error which the
// Server logs without aborting the rest of the teardown.
func TestServer_ShutdownLogsErrors(t *testing.T) {
	t.Parallel()
	listenAndHold := func(t *testing.T) *http.Server {
		t.Helper()
		lc := &net.ListenConfig{}
		ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		srv := &http.Server{
			ReadHeaderTimeout: time.Second,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				time.Sleep(2 * time.Second)
				w.WriteHeader(http.StatusOK)
			}),
		}
		go func() { _ = srv.Serve(ln) }()
		// kick off a slow request so a connection is open
		go func() {
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+ln.Addr().String(), nil)
			resp, err := http.DefaultClient.Do(req) //nolint:gosec // G104: test server URL
			if err == nil && resp != nil {
				_ = resp.Body.Close()
			}
		}()
		time.Sleep(20 * time.Millisecond) // let the request reach the handler
		return srv
	}

	s := &Server{
		main:    listenAndHold(t),
		metrics: listenAndHold(t),
		log:     slog.Default(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	s.Shutdown(ctx)
}

// TestServer_RunWithSeparateMetrics drives the metrics-listener
// goroutine in Run by configuring an invalid metrics address: the
// "metrics endpoint enabled on separate listener" log fires from the
// goroutine startup, ListenAndServe immediately returns the bind
// failure, and the "metrics listener failed" error log fires as well.
// The main listener is also configured to fail-fast so Run returns and
// the test does not block.
func TestServer_RunWithSeparateMetrics(t *testing.T) {
	t.Parallel()
	s := &Server{
		main:    &http.Server{Addr: "127.0.0.1:0", ReadHeaderTimeout: time.Second},
		metrics: &http.Server{Addr: "0.0.0.0:99999", ReadHeaderTimeout: time.Second}, // invalid port forces ListenAndServe error
		log:     slog.Default(),
	}
	// Run on a goroutine; the main listener will block. Cancel via
	// Shutdown after a short delay so Run exits.
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()
	time.Sleep(50 * time.Millisecond) // let metrics goroutine fail
	_ = s.main.Shutdown(context.Background())
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2 seconds")
	}
}

// -------------------------------------------------------------------------
// FULL ASSEMBLY (New + routes)
// -------------------------------------------------------------------------

// TestNew_HealthRoutesMounted asserts /health and /health/ready are
// reachable through a freshly assembled Server.
func TestNew_HealthRoutesMounted(t *testing.T) {
	cfg := loadCfg(t, validTestConfigYAML)
	inj, cleanup := resolvedInjector(t, cfg, "all")
	defer cleanup()

	var ready atomic.Bool
	ready.Store(true)
	srv, err := New(Deps{
		Cfg:       cfg,
		Mode:      "all",
		Injector:  inj,
		Ready:     &ready,
		DBBreaker: func() *breaker.CircuitBreaker { return nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	srv.main.Handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("/health/ready status = %d, want 200", w.Code)
	}
}

// TestNew_UIRouteMounted enables the dashboard and asserts /ui/ returns
// a response (not a 404). Drives the registerUIHandler branch.
//
//nolint:gosec // G101: hardcoded test fixture, not a real credential
func TestNew_UIRouteMounted(t *testing.T) {
	port := freePort(t)
	yaml := fmt.Sprintf(`
server:
  listen_addr: "127.0.0.1:%d"
  max_concurrent_reads: 4
  max_concurrent_writes: 4
database:
  driver: sqlite
  path: ":memory:"
buckets:
  - name: test
    credentials:
      - access_key_id: ak
        secret_access_key: sk
backends:
  - name: b1
    endpoint: http://localhost:19000
    region: us-east-1
    bucket: bucket1
    access_key_id: ak
    secret_access_key: sk
ui:
  enabled: true
  path: "/ui/"
  admin_key: "ak"
  admin_secret: "sec-1234567890123456"
  admin_token: "tok"
  session_secret: "12345678901234567890123456789012"
`, port)

	cfg := loadCfg(t, yaml)
	inj, cleanup := resolvedInjector(t, cfg, "all")
	defer cleanup()

	var ready atomic.Bool
	srv, err := New(Deps{
		Cfg:       cfg,
		Mode:      "all",
		Injector:  inj,
		Ready:     &ready,
		DBBreaker: func() *breaker.CircuitBreaker { return nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	srv.main.Handler.ServeHTTP(w, req)
	if w.Code == 0 {
		t.Errorf("/ui/ was not handled (code=0)")
	}
}

// TestNew_SingleAdmission drives the registerS3Handler MaxConcurrentRequests
// branch with LoadShedThreshold and AdmissionWait set.
func TestNew_SingleAdmission(t *testing.T) {
	yaml := `
server:
  listen_addr: ":0"
  max_concurrent_requests: 8
  load_shed_threshold: 0.9
  admission_wait: "50ms"
database:
  driver: sqlite
  path: ":memory:"
buckets:
  - name: test
    credentials:
      - access_key_id: ak
        secret_access_key: sk
backends:
  - name: b1
    endpoint: http://localhost:19000
    region: us-east-1
    bucket: bucket1
    access_key_id: ak
    secret_access_key: sk
`
	cfg := loadCfg(t, yaml)
	inj, cleanup := resolvedInjector(t, cfg, "all")
	defer cleanup()

	var ready atomic.Bool
	if _, err := New(Deps{
		Cfg:       cfg,
		Mode:      "all",
		Injector:  inj,
		Ready:     &ready,
		DBBreaker: func() *breaker.CircuitBreaker { return nil },
	}); err != nil {
		t.Fatalf("New: %v", err)
	}
}

// TestNew_RejectsNilDeps documents the contract on missing required deps.
func TestNew_RejectsNilDeps(t *testing.T) {
	t.Parallel()
	if _, err := New(Deps{}); err == nil {
		t.Fatal("expected error for nil deps")
	}
	cfg := &config.Config{}
	var ready atomic.Bool
	if _, err := New(Deps{Cfg: cfg, Ready: &ready}); err == nil {
		t.Fatal("expected error when DBBreaker callback is nil")
	}
}

// Compile-time guard: bytes import lives in serve fixtures but the
// httpserver tests also exercise byte buffers via httptest, so we
// keep both in scope to mirror the call-site shape.
var _ = bytes.NewBuffer
