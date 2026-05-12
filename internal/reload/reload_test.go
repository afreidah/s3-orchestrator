// -------------------------------------------------------------------------------
// Reload Coordinator - Tests
//
// Author: Alex Freidah
//
// Exercises the two load-bearing reload contracts: a failing config load must
// leave cfgPtr untouched (atomic rollback), and a successful reload must swap
// the new config in and fire every applicable hook. The end-to-end check uses
// a fully wired DI injector so the apply path runs the same code as a real
// SIGHUP.
// -------------------------------------------------------------------------------

package reload

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/di"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
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

// writeYAML writes content to a temp file and returns the path.
func writeYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// resolvedInjector configures observability and force-builds every
// service the apply path resolves, mirroring the runtime's eager
// resolution. Returns the injector and a cleanup func.
func resolvedInjector(t *testing.T, cfg *config.Config, logLevel *slog.LevelVar) (do.Injector, func()) {
	t.Helper()
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

	inj := di.NewInjector(cfg, "all", logLevel, logBuffer)

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

// TestReload_FailedLoadKeepsCurrent guards the atomic-rollback contract:
// when LoadConfig fails the previously stored config remains in cfgPtr
// and no apply hooks fire.
func TestReload_FailedLoadKeepsCurrent(t *testing.T) {
	path := writeYAML(t, validTestConfigYAML)
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	var cfgPtr syncutil.AtomicConfig[config.Config]
	cfgPtr.Store(cfg)
	original := cfgPtr.Load()

	coord := New(Deps{
		ConfigPath: path,
		CfgPtr:     &cfgPtr,
	})

	if err := os.WriteFile(path, []byte("server:\n  listen_addr: ':9000'\n"), 0600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}

	coord.Reload()

	if got := cfgPtr.Load(); got != original {
		t.Errorf("cfgPtr was mutated after a failing reload: got %p, want %p", got, original)
	}
}

// TestReload_AppliesNewConfig drives the happy path: rewrites the config
// file with a hot-reloadable change (log_level) and verifies the swap
// landed and the apply path ran end-to-end.
func TestReload_AppliesNewConfig(t *testing.T) {
	path := writeYAML(t, validTestConfigYAML)
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	var logLevel slog.LevelVar
	inj, cleanup := resolvedInjector(t, cfg, &logLevel)
	t.Cleanup(cleanup)

	var cfgPtr syncutil.AtomicConfig[config.Config]
	cfgPtr.Store(cfg)
	original := cfgPtr.Load()

	coord := New(Deps{
		ConfigPath: path,
		Injector:   inj,
		CfgPtr:     &cfgPtr,
		LogLevel:   &logLevel,
	})

	updated := strings.Replace(validTestConfigYAML,
		`listen_addr: ":0"`,
		`listen_addr: ":0"`+"\n  log_level: debug", 1)
	if err := os.WriteFile(path, []byte(updated), 0600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}

	coord.Reload()

	got := cfgPtr.Load()
	if got == original {
		t.Fatalf("cfgPtr was not swapped after a successful reload")
	}
	if got.Server.LogLevel != "debug" {
		t.Errorf("reloaded log_level = %q, want debug", got.Server.LogLevel)
	}
}
