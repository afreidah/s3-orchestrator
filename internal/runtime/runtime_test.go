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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/reload"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

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

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

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

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

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

// TestRunFullLifecycle_WithShutdownDelay drives the load-balancer drain
// branch of the shutdown sequence by setting Server.ShutdownDelay; the
// log line that announces the wait period only fires when the delay is
// positive.
func TestRunFullLifecycle_WithShutdownDelay(t *testing.T) {
	port := freePort(t)
	yaml := fmt.Sprintf(`
server:
  listen_addr: "127.0.0.1:%d"
  shutdown_delay: 10ms
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
	cfg := loadCfg(t, yaml)

	rt, err := New(Options{
		ConfigPath: writeYAML(t, yaml),
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
	for range 50 {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, addr+"/health/ready", nil)
		resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	select {
	case <-errCh:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not exit within 10 seconds")
	}
}

// TestRunFullLifecycle starts the daemon, waits for readiness, hits
// /health, then cancels and asserts a clean shutdown. This is the
// regression guard for runtime ordering after the decomposition.
func TestRunFullLifecycle(t *testing.T) {
	port := freePort(t)
	cfg := loadCfg(t, configWithPort(port))

	var stdout bytes.Buffer
	rt, err := New(Options{
		ConfigPath: writeYAML(t, configWithPort(port)),
		Mode:       "all",
		Stdout:     &stdout,
	}, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cause := errors.New("test-shutdown-cause")
	ctx, cancel := context.WithCancelCause(context.Background())
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
		cancel(cause)
		t.Fatalf("server never became ready: %v", lastErr)
	}

	cancel(cause)
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not exit within 10 seconds")
	}

	// New shutdown-cause log line (Go 1.26 signal.NotifyContext compatibility).
	if !strings.Contains(stdout.String(), `"msg":"shutdown initiated"`) || !strings.Contains(stdout.String(), cause.Error()) {
		t.Errorf("expected shutdown log line with cause %q, got logs:\n%s", cause.Error(), stdout.String())
	}
}

// TestToAdminReloadStatus_NilBeforeFirstReload asserts the converter reports
// nil before any reload has run, which is the signal the admin handler turns
// into its not-yet placeholder.
func TestToAdminReloadStatus_NilBeforeFirstReload(t *testing.T) {
	t.Parallel()
	if got := toAdminReloadStatus(nil); got != nil {
		t.Errorf("toAdminReloadStatus(nil) = %+v, want nil", got)
	}
}

// TestToAdminReloadStatus_CopiesResult pins the conversion that keeps
// internal/reload types off the admin API: every field maps across, hook
// outcomes included, with generation carried as a pointer so a zero survives.
func TestToAdminReloadStatus_CopiesResult(t *testing.T) {
	t.Parallel()
	started := time.Unix(1700000000, 0).UTC()
	ended := started.Add(2 * time.Second)
	res := &reload.Result{
		Generation: 4,
		Status:     reload.ReloadPartialApplied,
		Outcomes: []reload.HookOutcome{
			{Name: "log-level", Status: reload.HookApplied},
			{Name: "backends", Status: reload.HookFailed, Error: "boom"},
		},
		RequiresRestart: []string{"server.listen_addr"},
		LoadError:       "",
		StartedAt:       started,
		EndedAt:         ended,
	}

	got := toAdminReloadStatus(res)
	if got == nil {
		t.Fatal("toAdminReloadStatus returned nil for a populated result")
	}
	if got.Status != string(reload.ReloadPartialApplied) {
		t.Errorf("status = %q, want %q", got.Status, reload.ReloadPartialApplied)
	}
	if got.Generation == nil || *got.Generation != 4 {
		t.Errorf("generation = %v, want 4", got.Generation)
	}
	if len(got.Outcomes) != 2 {
		t.Fatalf("outcomes = %d, want 2", len(got.Outcomes))
	}
	if got.Outcomes[1].Name != "backends" || got.Outcomes[1].Status != string(reload.HookFailed) ||
		got.Outcomes[1].Error != "boom" {
		t.Errorf("failed outcome = %+v, want backends/failed/boom", got.Outcomes[1])
	}
	if len(got.RequiresRestart) != 1 || got.RequiresRestart[0] != "server.listen_addr" {
		t.Errorf("requires_restart = %v, want [server.listen_addr]", got.RequiresRestart)
	}
	if got.StartedAt == nil || !got.StartedAt.Equal(started) || got.EndedAt == nil || !got.EndedAt.Equal(ended) {
		t.Errorf("timestamps = %v/%v, want %v/%v", got.StartedAt, got.EndedAt, started, ended)
	}
}

// TestToAdminReloadStatus_ZeroGenerationSurvives guards the pointer field: a
// validation-failed pass before any successful apply legitimately has
// generation 0, and must still report the field rather than looking like the
// not-yet placeholder.
func TestToAdminReloadStatus_ZeroGenerationSurvives(t *testing.T) {
	t.Parallel()
	got := toAdminReloadStatus(&reload.Result{
		Generation: 0,
		Status:     reload.ReloadValidationFailed,
	})
	if got == nil || got.Generation == nil {
		t.Fatalf("generation dropped for a zero-generation result: %+v", got)
	}
	if *got.Generation != 0 {
		t.Errorf("generation = %d, want 0", *got.Generation)
	}

	body, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"generation":0`) {
		t.Errorf("body = %s, want an explicit generation:0", body)
	}
}
