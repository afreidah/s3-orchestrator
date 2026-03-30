// -------------------------------------------------------------------------------
// Server Lifecycle Tests - run(), server struct, and lifecycle methods
//
// Author: Alex Freidah
//
// Tests for the refactored server lifecycle: config loading, logging init, DI
// wiring, HTTP server construction, TLS configuration, health endpoints, and
// graceful shutdown. Uses in-memory SQLite and ephemeral ports to avoid
// external dependencies.
// -------------------------------------------------------------------------------

package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// -------------------------------------------------------------------------
// TEST HELPERS
// -------------------------------------------------------------------------

// validTestConfigYAML is a minimal YAML config that passes validation and
// uses SQLite in-memory so no external services are needed.
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

// writeTestConfig writes YAML content to a temp file and returns the path.
func writeTestConfig(t *testing.T, content string) string {
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
	l, err := net.Listen("tcp", "127.0.0.1:0") //nolint:noctx // test helper, no cancellation needed
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// configWithPort returns a valid YAML config using the given listen port.
func configWithPort(port int) string {
	return fmt.Sprintf(`
server:
  listen_addr: "127.0.0.1:%d"
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
`, port)
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

// -------------------------------------------------------------------------
// loadConfig
// -------------------------------------------------------------------------

// TestLoadConfig_ValidFile verifies that a valid YAML config loads without error.
func TestLoadConfig_ValidFile(t *testing.T) {
	path := writeTestConfig(t, validTestConfigYAML)
	s := &server{configPath: path}
	if err := s.loadConfig(); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if s.cfg == nil {
		t.Fatal("expected cfg to be set")
	}
	if len(s.cfg.Backends) != 1 {
		t.Errorf("backends = %d, want 1", len(s.cfg.Backends))
	}
}

// TestLoadConfig_MissingFile verifies that a missing config returns an error.
func TestLoadConfig_MissingFile(t *testing.T) {
	s := &server{configPath: "/nonexistent/config.yaml"}
	if err := s.loadConfig(); err == nil {
		t.Fatal("expected error for missing config")
	}
}

// TestLoadConfig_InvalidYAML verifies that invalid config content returns an error.
func TestLoadConfig_InvalidYAML(t *testing.T) {
	path := writeTestConfig(t, "server:\n  listen_addr: ':9000'\n")
	s := &server{configPath: path}
	if err := s.loadConfig(); err == nil {
		t.Fatal("expected error for invalid config (missing required fields)")
	}
}

// -------------------------------------------------------------------------
// initLogging
// -------------------------------------------------------------------------

// TestInitLogging_SetsDefaultLogger verifies that initLogging configures the
// global slog default and initializes the tracer shutdown function.
func TestInitLogging_SetsDefaultLogger(t *testing.T) {
	path := writeTestConfig(t, validTestConfigYAML)
	s := &server{configPath: path, stdout: &bytes.Buffer{}}
	if err := s.loadConfig(); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if err := s.initLogging(); err != nil {
		t.Fatalf("initLogging: %v", err)
	}
	if s.shutdownTracer == nil {
		t.Error("expected shutdownTracer to be set")
	}
	if s.logBuffer == nil {
		t.Error("expected logBuffer to be set")
	}
	// Clean up tracer
	_ = s.shutdownTracer(context.Background())
}

// -------------------------------------------------------------------------
// initDI
// -------------------------------------------------------------------------

// TestInitDI_RegistersProviders verifies that initDI populates the injector
// without error for a minimal config.
func TestInitDI_RegistersProviders(t *testing.T) {
	path := writeTestConfig(t, validTestConfigYAML)
	s := &server{configPath: path, mode: "all", stdout: &bytes.Buffer{}}
	if err := s.loadConfig(); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if err := s.initLogging(); err != nil {
		t.Fatalf("initLogging: %v", err)
	}
	s.initDI()
	if s.inj == nil {
		t.Fatal("expected injector to be set")
	}
	_ = s.shutdownTracer(context.Background())
}

// -------------------------------------------------------------------------
// configureTLS
// -------------------------------------------------------------------------

// TestConfigureTLS_NoTLS verifies that configureTLS is a no-op when no cert
// file is configured.
func TestConfigureTLS_NoTLS(t *testing.T) {
	path := writeTestConfig(t, validTestConfigYAML)
	s := &server{configPath: path, stdout: &bytes.Buffer{}}
	if err := s.loadConfig(); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	s.httpServer = &http.Server{ReadHeaderTimeout: time.Second}
	if err := s.configureTLS(); err != nil {
		t.Fatalf("configureTLS: %v", err)
	}
	if s.httpServer.TLSConfig != nil {
		t.Error("expected no TLS config when cert_file is empty")
	}
}

// TestConfigureTLS_WithCert verifies that configureTLS loads a certificate
// and sets TLSConfig on the server.
func TestConfigureTLS_WithCert(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := generateSelfSignedCert(t, dir)

	yaml := fmt.Sprintf(`
server:
  listen_addr: ":0"
  tls:
    cert_file: %s
    key_file: %s
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
`, certPath, keyPath)

	path := writeTestConfig(t, yaml)
	s := &server{configPath: path, stdout: &bytes.Buffer{}}
	if err := s.loadConfig(); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	s.httpServer = &http.Server{ReadHeaderTimeout: time.Second}
	if err := s.configureTLS(); err != nil {
		t.Fatalf("configureTLS: %v", err)
	}
	if s.httpServer.TLSConfig == nil {
		t.Fatal("expected TLS config to be set")
	}
	if s.certReloader == nil {
		t.Error("expected certReloader to be set")
	}
}

// TestConfigureTLS_BadCert verifies that configureTLS returns an error when
// the cert file is invalid.
func TestConfigureTLS_BadCert(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(certPath, []byte("not a cert"), 0600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	keyPath := filepath.Join(dir, "bad-key.pem")
	if err := os.WriteFile(keyPath, []byte("not a key"), 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	yaml := fmt.Sprintf(`
server:
  listen_addr: ":0"
  tls:
    cert_file: %s
    key_file: %s
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
`, certPath, keyPath)

	path := writeTestConfig(t, yaml)
	s := &server{configPath: path, stdout: &bytes.Buffer{}}
	if err := s.loadConfig(); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	s.httpServer = &http.Server{ReadHeaderTimeout: time.Second}
	if err := s.configureTLS(); err == nil {
		t.Fatal("expected error for bad cert")
	}
}

// -------------------------------------------------------------------------
// Health Endpoints
// -------------------------------------------------------------------------

// TestHealthEndpoints_Healthy verifies /health returns 200 with "ok" status
// and /health/ready returns 200 when the server is ready.
func TestHealthEndpoints_Healthy(t *testing.T) {
	port := freePort(t)
	path := writeTestConfig(t, configWithPort(port))
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
	defer s.db.Close()
	defer s.manager.Close()
	defer func() { _ = s.shutdownTracer(context.Background()) }()

	s.buildHTTPServer()
	s.ready.Store(true)

	// /health
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "/health", nil)
	w := &testResponseWriter{header: http.Header{}}
	s.httpServer.Handler.ServeHTTP(w, req)
	if w.code != http.StatusOK {
		t.Errorf("/health status = %d, want 200", w.code)
	}
	var healthResp map[string]string
	if err := json.Unmarshal(w.body.Bytes(), &healthResp); err != nil {
		t.Fatalf("unmarshal /health response: %v", err)
	}
	if healthResp["status"] != "ok" {
		t.Errorf("/health status = %q, want ok", healthResp["status"])
	}

	// /health/ready
	req2, _ := http.NewRequestWithContext(context.Background(), "GET", "/health/ready", nil)
	w2 := &testResponseWriter{header: http.Header{}}
	s.httpServer.Handler.ServeHTTP(w2, req2)
	if w2.code != http.StatusOK {
		t.Errorf("/health/ready status = %d, want 200", w2.code)
	}
}

// TestHealthReady_NotReady verifies /health/ready returns 503 before the
// server is marked ready.
func TestHealthReady_NotReady(t *testing.T) {
	port := freePort(t)
	path := writeTestConfig(t, configWithPort(port))
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
	defer s.db.Close()
	defer s.manager.Close()
	defer func() { _ = s.shutdownTracer(context.Background()) }()

	s.buildHTTPServer()
	// ready is false by default

	req, _ := http.NewRequestWithContext(context.Background(), "GET", "/health/ready", nil)
	w := &testResponseWriter{header: http.Header{}}
	s.httpServer.Handler.ServeHTTP(w, req)
	if w.code != http.StatusServiceUnavailable {
		t.Errorf("/health/ready status = %d, want 503", w.code)
	}
}

// -------------------------------------------------------------------------
// run() integration
// -------------------------------------------------------------------------

// TestRun_InvalidConfigPath verifies that run() returns an error for a
// nonexistent config file.
func TestRun_InvalidConfigPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := run(ctx, "/nonexistent/config.yaml", "all", &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error for missing config")
	}
	if !strings.Contains(err.Error(), "load config") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestRun_StartsAndStops verifies the full server lifecycle: run() starts
// the HTTP server, responds to health checks, and shuts down cleanly when
// the context is cancelled.
func TestRun_StartsAndStops(t *testing.T) {
	port := freePort(t)
	path := writeTestConfig(t, configWithPort(port))

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, path, "all", &bytes.Buffer{})
	}()

	// Wait for the server to be ready
	addr := fmt.Sprintf("http://127.0.0.1:%d", port)
	var lastErr error
	for range 50 {
		getReq, _ := http.NewRequestWithContext(context.Background(), "GET", addr + "/health/ready", nil)
		resp, err := http.DefaultClient.Do(getReq) //nolint:gosec // G704: test server URL
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				lastErr = nil
				break
			}
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr != nil {
		cancel()
		t.Fatalf("server never became ready: %v", lastErr)
	}

	// Verify /health returns 200
	getReq, _ := http.NewRequestWithContext(context.Background(), "GET", addr + "/health", nil)
	resp, err := http.DefaultClient.Do(getReq) //nolint:gosec // G704: test server URL
	if err != nil {
		cancel()
		t.Fatalf("GET /health: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("/health status = %d, want 200", resp.StatusCode)
	}

	// Cancel context to trigger shutdown
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run() returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run() did not exit within 10 seconds")
	}
}

// TestRun_InvalidConfig verifies that run() returns an error for config
// that fails validation (missing required fields).
func TestRun_InvalidConfig(t *testing.T) {
	path := writeTestConfig(t, "server:\n  listen_addr: ':9000'\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := run(ctx, path, "all", &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error for invalid config")
	}
}

// -------------------------------------------------------------------------
// parseTLSVersion
// -------------------------------------------------------------------------

// TestParseTLSVersion verifies TLS version string parsing.
func TestParseTLSVersion(t *testing.T) {
	tests := []struct {
		input string
		want  uint16
	}{
		{"1.3", 0x0304}, // tls.VersionTLS13
		{"1.2", 0x0303}, // tls.VersionTLS12
		{"", 0x0303},    // default to TLS 1.2
		{"junk", 0x0303},
	}
	for _, tt := range tests {
		got := parseTLSVersion(tt.input)
		if got != tt.want {
			t.Errorf("parseTLSVersion(%q) = 0x%04x, want 0x%04x", tt.input, got, tt.want)
		}
	}
}

// -------------------------------------------------------------------------
// logStartup
// -------------------------------------------------------------------------

// TestLogStartup_DoesNotPanic verifies that logStartup runs without error
// when the server has a valid config.
func TestLogStartup_DoesNotPanic(t *testing.T) {
	path := writeTestConfig(t, validTestConfigYAML)
	s := &server{configPath: path, mode: "all", stdout: &bytes.Buffer{}}
	if err := s.loadConfig(); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	// Should not panic
	s.logStartup()
}

// -------------------------------------------------------------------------
// RESPONSE WRITER HELPER
// -------------------------------------------------------------------------

// testResponseWriter is a minimal http.ResponseWriter for handler tests.
type testResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	code   int
}

func (w *testResponseWriter) Header() http.Header       { return w.header }
func (w *testResponseWriter) WriteHeader(code int)       { w.code = code }
func (w *testResponseWriter) Write(b []byte) (int, error) { return w.body.Write(b) }

var _ http.ResponseWriter = (*testResponseWriter)(nil)
