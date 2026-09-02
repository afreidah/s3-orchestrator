// -------------------------------------------------------------------------------
// Serve - End-to-end Run() Tests
//
// Author: Alex Freidah
//
// These tests drive the CLI Run entry point end-to-end (load config -> build
// runtime -> serve -> shutdown). The subsystem-level tests for HTTP routing,
// TLS, reload, and lifecycle live in internal/transport/httpserver,
// internal/reload, and internal/runtime respectively.
// -------------------------------------------------------------------------------

package serve

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

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

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestRun_InvalidConfigPath verifies that Run returns an error for a
// nonexistent config file.
func TestRun_InvalidConfigPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Run(ctx, "/nonexistent/config.yaml", "all", &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error for missing config")
	}
	if !strings.Contains(err.Error(), "load config") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestRun_InvalidConfig verifies that Run returns an error for config
// that fails validation (missing required fields).
func TestRun_InvalidConfig(t *testing.T) {
	path := writeTestConfig(t, "server:\n  listen_addr: ':9000'\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, path, "all", &bytes.Buffer{}); err == nil {
		t.Fatal("expected error for invalid config")
	}
}

// TestRun_StartsAndStops verifies the full server lifecycle: Run starts
// the HTTP server, responds to health checks, and shuts down cleanly when
// the context is cancelled. This is the strongest regression guard for
// the runtime decomposition.
func TestRun_StartsAndStops(t *testing.T) {
	port := freePort(t)
	path := writeTestConfig(t, configWithPort(port))

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, path, "all", &bytes.Buffer{})
	}()

	addr := fmt.Sprintf("http://127.0.0.1:%d", port)
	var lastErr error
	for range 50 {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, addr+"/health/ready", nil)
		resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
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

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, addr+"/health", nil)
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		cancel()
		t.Fatalf("GET /health: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("/health status = %d, want 200", resp.StatusCode)
	}

	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not exit within 10 seconds")
	}
}
