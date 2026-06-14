// -------------------------------------------------------------------------------
// Reload Coordinator - Tests
//
// Author: Alex Freidah
//
// Exercises every reload outcome path: full success, partial Apply failure
// (one hook fails, others still applied, cfgPtr still swapped, generation
// still advances), validation failure (Check error aborts before any
// mutation, generation unchanged), load failure (invalid YAML, generation
// unchanged), and non-reloadable field tracking. Generation monotonicity
// and LastResult snapshot are pinned end-to-end with the default hook set.
// -------------------------------------------------------------------------------

package reload

import (
	"context"
	"errors"
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

// -------------------------------------------------------------------------
// FIXTURES
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

	inj := di.NewInjector(di.InjectorDeps{Config: cfg, Mode: "all", LogLevel: logLevel, LogBuffer: logBuffer})

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

// fakeHook is a test double for the Hook interface. Behaviour is
// controlled per instance so a test can compose any mix of
// applied/skipped/failed/check-failed outcomes.
type fakeHook struct {
	name       string
	checkErr   error
	applyStat  HookStatus
	applyErr   error
	applyCalls int
	checkCalls int
}

func (h *fakeHook) Name() string { return h.name }
func (h *fakeHook) Check(_, _ *config.Config) error {
	h.checkCalls++
	return h.checkErr
}
func (h *fakeHook) Apply(_ context.Context, _, _ *config.Config) (HookStatus, error) {
	h.applyCalls++
	return h.applyStat, h.applyErr
}

// initialCfgPtr stores the loaded YAML into a fresh AtomicConfig and
// returns the cfg pointer so tests can compare swap identity.
func initialCfgPtr(t *testing.T, path string) (*syncutil.AtomicConfig[config.Config], *config.Config) {
	t.Helper()
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	var ptr syncutil.AtomicConfig[config.Config]
	ptr.Store(cfg)
	return &ptr, cfg
}

// -------------------------------------------------------------------------
// LOAD-FAILURE PATH
// -------------------------------------------------------------------------

// TestReload_LoadFailure_KeepsCurrentAndReportsStatus guards the
// atomic-rollback contract and pins the LoadFailed status reporting.
func TestReload_LoadFailure_KeepsCurrentAndReportsStatus(t *testing.T) {
	path := writeYAML(t, validTestConfigYAML)
	cfgPtr, original := initialCfgPtr(t, path)

	coord := New(&Deps{
		ConfigPath: path,
		CfgPtr:     cfgPtr,
		Hooks:      []Hook{}, // no hooks needed; load fails first
	})

	if err := os.WriteFile(path, []byte("server:\n  listen_addr: ':9000'\n"), 0600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}

	r := coord.Reload()
	if r.Status != ReloadLoadFailed {
		t.Errorf("Status = %q, want %q", r.Status, ReloadLoadFailed)
	}
	if r.LoadError == "" {
		t.Error("LoadError should be populated on load failure")
	}
	if got := cfgPtr.Load(); got != original {
		t.Errorf("cfgPtr was mutated: got %p, want %p", got, original)
	}
	if coord.Generation() != 0 {
		t.Errorf("Generation = %d, want 0 (load failure must not advance)", coord.Generation())
	}
	if coord.LastResult() != r {
		t.Error("LastResult did not capture the load-failure result")
	}
}

// -------------------------------------------------------------------------
// VALIDATION-FAILURE PATH
// -------------------------------------------------------------------------

// TestReload_ValidationFailure_AbortsBeforeApply ensures a Check error
// stops the pass before any Apply runs, leaves cfgPtr untouched, and
// keeps the generation unchanged.
func TestReload_ValidationFailure_AbortsBeforeApply(t *testing.T) {
	path := writeYAML(t, validTestConfigYAML)
	cfgPtr, original := initialCfgPtr(t, path)

	good := &fakeHook{name: "good", applyStat: HookApplied}
	bad := &fakeHook{name: "bad", checkErr: errors.New("not reloadable")}
	tail := &fakeHook{name: "tail", applyStat: HookApplied}

	coord := New(&Deps{
		ConfigPath: path,
		CfgPtr:     cfgPtr,
		Hooks:      []Hook{good, bad, tail},
	})

	r := coord.Reload()
	if r.Status != ReloadValidationFailed {
		t.Errorf("Status = %q, want %q", r.Status, ReloadValidationFailed)
	}
	if good.applyCalls != 0 || tail.applyCalls != 0 || bad.applyCalls != 0 {
		t.Errorf("Apply ran during validation failure: good=%d bad=%d tail=%d",
			good.applyCalls, bad.applyCalls, tail.applyCalls)
	}
	if cfgPtr.Load() != original {
		t.Error("cfgPtr was mutated on validation failure")
	}
	if coord.Generation() != 0 {
		t.Errorf("Generation = %d, want 0 (validation failure must not advance)", coord.Generation())
	}
	// The failed hook should be the only outcome recorded.
	if len(r.Outcomes) != 1 || r.Outcomes[0].Name != "bad" || r.Outcomes[0].Status != HookFailed {
		t.Errorf("Outcomes = %+v, want single failed outcome for 'bad'", r.Outcomes)
	}
}

// -------------------------------------------------------------------------
// PARTIAL-APPLY PATH
// -------------------------------------------------------------------------

// TestReload_PartialApply_StillSwapsCfgAndAdvancesGen confirms that a
// single Apply failure does not abort the rest of the pass: every other
// hook still runs, cfgPtr still swaps, and the generation still
// advances. The PartialApplied status surfaces the degradation to
// operators.
func TestReload_PartialApply_StillSwapsCfgAndAdvancesGen(t *testing.T) {
	path := writeYAML(t, validTestConfigYAML)
	cfgPtr, original := initialCfgPtr(t, path)

	a := &fakeHook{name: "a", applyStat: HookApplied}
	b := &fakeHook{name: "b", applyErr: errors.New("backend rejected")}
	c := &fakeHook{name: "c", applyStat: HookApplied}

	coord := New(&Deps{
		ConfigPath: path,
		CfgPtr:     cfgPtr,
		Hooks:      []Hook{a, b, c},
	})

	r := coord.Reload()
	if r.Status != ReloadPartialApplied {
		t.Errorf("Status = %q, want %q", r.Status, ReloadPartialApplied)
	}
	if a.applyCalls != 1 || b.applyCalls != 1 || c.applyCalls != 1 {
		t.Errorf("Apply calls a=%d b=%d c=%d, want each 1", a.applyCalls, b.applyCalls, c.applyCalls)
	}
	if cfgPtr.Load() == original {
		t.Error("cfgPtr was not swapped on partial apply")
	}
	if coord.Generation() != 1 {
		t.Errorf("Generation = %d, want 1 (partial apply advances)", coord.Generation())
	}
	// Exactly one failed outcome, named 'b'.
	failedCount := 0
	for _, o := range r.Outcomes {
		if o.Status == HookFailed {
			failedCount++
			if o.Name != "b" {
				t.Errorf("Failed outcome name = %q, want b", o.Name)
			}
			if o.Error == "" {
				t.Error("Failed outcome Error must be populated")
			}
		}
	}
	if failedCount != 1 {
		t.Errorf("failed outcomes = %d, want 1", failedCount)
	}
}

// -------------------------------------------------------------------------
// FULL-SUCCESS + MONOTONIC GENERATION
// -------------------------------------------------------------------------

// TestReload_FullSuccess_MonotonicGeneration drives two consecutive
// reloads and asserts the generation strictly increases and the
// FullSuccess status holds in both passes.
func TestReload_FullSuccess_MonotonicGeneration(t *testing.T) {
	path := writeYAML(t, validTestConfigYAML)
	cfgPtr, _ := initialCfgPtr(t, path)

	a := &fakeHook{name: "a", applyStat: HookApplied}
	b := &fakeHook{name: "b", applyStat: HookSkipped}

	coord := New(&Deps{
		ConfigPath: path,
		CfgPtr:     cfgPtr,
		Hooks:      []Hook{a, b},
	})

	r1 := coord.Reload()
	if r1.Status != ReloadFullSuccess {
		t.Errorf("r1.Status = %q, want %q", r1.Status, ReloadFullSuccess)
	}
	if r1.Generation != 1 {
		t.Errorf("r1.Generation = %d, want 1", r1.Generation)
	}

	r2 := coord.Reload()
	if r2.Status != ReloadFullSuccess {
		t.Errorf("r2.Status = %q, want %q", r2.Status, ReloadFullSuccess)
	}
	if r2.Generation != 2 {
		t.Errorf("r2.Generation = %d, want 2 (must strictly advance)", r2.Generation)
	}
	if coord.LastResult() != r2 {
		t.Error("LastResult should hold the most recent result")
	}
}

// -------------------------------------------------------------------------
// REQUIRES-RESTART TRACKING
// -------------------------------------------------------------------------

// TestReload_RequiresRestart_Populated changes a non-reloadable field
// across reloads and asserts the field name surfaces in the result.
// The reload itself still proceeds; non-reloadable changes are warnings
// not errors.
func TestReload_RequiresRestart_Populated(t *testing.T) {
	path := writeYAML(t, validTestConfigYAML)
	cfgPtr, _ := initialCfgPtr(t, path)

	coord := New(&Deps{
		ConfigPath: path,
		CfgPtr:     cfgPtr,
		Hooks:      []Hook{}, // no hook noise; we only check RequiresRestart
	})

	// Rewriting the listen_addr is a non-reloadable change.
	updated := strings.Replace(validTestConfigYAML,
		`listen_addr: ":0"`,
		`listen_addr: ":9999"`, 1)
	if err := os.WriteFile(path, []byte(updated), 0600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	r := coord.Reload()
	if len(r.RequiresRestart) == 0 {
		t.Errorf("RequiresRestart should list at least one field, got %v", r.RequiresRestart)
	}
	if r.Status != ReloadFullSuccess {
		t.Errorf("Status = %q, want %q (non-reloadable change is a warning, not an error)",
			r.Status, ReloadFullSuccess)
	}
}

// -------------------------------------------------------------------------
// END-TO-END WITH DEFAULT HOOKS
// -------------------------------------------------------------------------

// TestReload_DefaultHooks_AppliesNewConfig drives the full default hook
// set against a real injector to prove the production assembly still
// works after the Hook refactor: cfgPtr swaps, generation advances, and
// the log_level hook actually mutates the level pointer.
func TestReload_DefaultHooks_AppliesNewConfig(t *testing.T) {
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

	coord := New(&Deps{
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

	r := coord.Reload()
	if r.Status != ReloadFullSuccess {
		t.Errorf("Status = %q, want %q", r.Status, ReloadFullSuccess)
	}
	if r.Generation != 1 {
		t.Errorf("Generation = %d, want 1", r.Generation)
	}
	if cfgPtr.Load() == original {
		t.Fatal("cfgPtr was not swapped after a successful reload")
	}
	if got := cfgPtr.Load(); got.Server.LogLevel != "debug" {
		t.Errorf("reloaded log_level = %q, want debug", got.Server.LogLevel)
	}
}
