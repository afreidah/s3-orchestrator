// -------------------------------------------------------------------------------
// Runtime - Tests
//
// Author: Alex Freidah
//
// Covers the runtime composition root: observability bootstrap state,
// required-service resolution, and full New + Run + ordered shutdown via a
// short-lived in-memory daemon. The end-to-end CLI lifecycle is covered by
// the serve package; tests here pin the per-step contracts the CLI layer
// depends on.
// -------------------------------------------------------------------------------

package runtime

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

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

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

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

// loadCfg parses YAML into *config.Config.
func loadCfg(t *testing.T, yaml string) *config.Config {
	t.Helper()
	cfg, err := config.LoadConfig(writeYAML(t, yaml))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

// TestStartObservability sets the default logger and returns a usable
// shutdownTracer + LogBuffer.
func TestStartObservability(t *testing.T) {
	cfg := loadCfg(t, validTestConfigYAML)
	var logLevel slog.LevelVar
	obs, err := startObservability(cfg, &bytes.Buffer{}, &logLevel)
	if err != nil {
		t.Fatalf("startObservability: %v", err)
	}
	if obs.LogBuffer == nil {
		t.Error("expected LogBuffer to be set")
	}
	if obs.ShutdownTracer == nil {
		t.Error("expected ShutdownTracer to be set")
	}
	_ = obs.ShutdownTracer(context.Background())
}

// TestNew_ErrorsOnNilConfig surfaces the contract on missing config.
func TestNew_ErrorsOnNilConfig(t *testing.T) {
	t.Parallel()
	if _, err := New(Options{}, nil); err == nil {
		t.Fatal("expected error on nil config")
	}
}

// TestRunFullLifecycle starts the daemon, waits for readiness, hits
// /health, then cancels and asserts a clean shutdown. This is the
// regression guard for runtime ordering after the decomposition.
func TestRunFullLifecycle(t *testing.T) {
	port := freePort(t)
	cfg := loadCfg(t, configWithPort(port))

	rt, err := New(Options{
		ConfigPath: writeYAML(t, configWithPort(port)),
		Mode:       "all",
		Stdout:     io.Discard,
	}, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- rt.Run(ctx) }()

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
