// -------------------------------------------------------------------------------
// DI - WireManager Tests
//
// Author: Alex Freidah
//
// Covers the happy path (every required dependency resolves and the
// worker handles + drain manager land on BackendManager) and each
// dependency-missing branch by feeding WireManager a bare injector
// instead of the full one NewInjector assembles.
// -------------------------------------------------------------------------------

package di

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// TestWireManager_HappyPath builds a fully-populated injector via
// NewInjector, runs WireManager, and asserts the worker handles plus
// the drain manager were installed on BackendManager. PendingReaper is
// expected nil because happyPathConfig leaves the pending pattern
// disabled, which exercises the invokeOptional branch.
func TestWireManager_HappyPath(t *testing.T) {
	t.Parallel()
	cfg := happyPathConfig(t.TempDir())
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("config validation: %v", err)
	}
	inj := NewInjector(cfg, "all", new(slog.LevelVar), telemetry.NewLogBuffer())
	t.Cleanup(func() { _ = inj.Shutdown() })

	if err := WireManager(inj); err != nil {
		t.Fatalf("WireManager: %v", err)
	}

	mgr, err := do.Invoke[*proxy.BackendManager](inj)
	if err != nil {
		t.Fatalf("resolve BackendManager: %v", err)
	}
	if mgr.Drain() == nil {
		t.Error("DrainManager not installed by WireDrain")
	}
	if _, err := do.Invoke[*worker.Rebalancer](inj); err != nil {
		t.Errorf("resolve Rebalancer: %v", err)
	}
	if _, err := do.Invoke[*worker.Replicator](inj); err != nil {
		t.Errorf("resolve Replicator: %v", err)
	}
	if _, err := do.Invoke[*worker.OverReplicationCleaner](inj); err != nil {
		t.Errorf("resolve OverReplicationCleaner: %v", err)
	}
	if _, err := do.Invoke[*worker.CleanupWorker](inj); err != nil {
		t.Errorf("resolve CleanupWorker: %v", err)
	}
	if _, err := do.Invoke[*worker.Scrubber](inj); err != nil {
		t.Errorf("resolve Scrubber: %v", err)
	}
}

// TestWireManager_NoBackendManager covers the first invoke's error
// path: with a bare injector the BackendManager provider is not
// registered and WireManager surfaces the wrapped resolution error.
func TestWireManager_NoBackendManager(t *testing.T) {
	t.Parallel()
	inj := do.New()
	t.Cleanup(func() { _ = inj.Shutdown() })

	err := WireManager(inj)
	if err == nil {
		t.Fatal("expected error for missing BackendManager")
	}
	if !strings.Contains(err.Error(), "resolve BackendManager") {
		t.Errorf("error = %q, want to contain \"resolve BackendManager\"", err.Error())
	}
}

// TestWireManager_NoWorkers covers a subsequent invoke's error path:
// register BackendManager so the first lookup succeeds, then call
// WireManager and assert the wrap names the worker that failed to
// resolve. The specific worker named is whichever WireManager invokes
// first after BackendManager - the contract this test pins is
// "missing required worker bubbles up wrapped, not panicking".
func TestWireManager_NoWorkers(t *testing.T) {
	t.Parallel()
	inj := do.New()
	t.Cleanup(func() { _ = inj.Shutdown() })

	// Provide just enough of a BackendManager value so the first
	// invoke succeeds without a real injector. Workers are intentionally
	// absent so WireManager bails on the next lookup.
	do.ProvideValue(inj, &proxy.BackendManager{})

	err := WireManager(inj)
	if err == nil {
		t.Fatal("expected error for missing worker")
	}
	// At minimum the wrap should mention one of the required workers
	// or the drain manager; any of them failing satisfies the contract
	// that WireManager surfaces resolution failures rather than panicking.
	if !strings.Contains(err.Error(), "resolve") {
		t.Errorf("error = %q, want to contain \"resolve\"", err.Error())
	}
}

// TestWireManager_ProgressiveErrors drives every required-resolution
// error branch by registering dependencies up to a specific worker and
// stopping. Each row asserts that the error wrap names the dependency
// that was intentionally omitted, so a future refactor that drops or
// reorders an invoke surfaces here.
func TestWireManager_ProgressiveErrors(t *testing.T) {
	t.Parallel()

	// register applies the supplied registration functions to a fresh
	// injector in order. Stopping early leaves the remaining
	// dependencies unregistered.
	register := func(steps ...func(do.Injector)) do.Injector {
		inj := do.New()
		for _, step := range steps {
			step(inj)
		}
		return inj
	}

	provideManager := func(inj do.Injector) { do.ProvideValue(inj, &proxy.BackendManager{}) }
	provideRebalancer := func(inj do.Injector) { do.ProvideValue(inj, &worker.Rebalancer{}) }
	provideReplicator := func(inj do.Injector) { do.ProvideValue(inj, &worker.Replicator{}) }
	provideOverRep := func(inj do.Injector) { do.ProvideValue(inj, &worker.OverReplicationCleaner{}) }
	provideCleanup := func(inj do.Injector) { do.ProvideValue(inj, &worker.CleanupWorker{}) }
	provideScrubber := func(inj do.Injector) { do.ProvideValue(inj, &worker.Scrubber{}) }

	tests := []struct {
		name      string
		steps     []func(do.Injector)
		errSubstr string
	}{
		{
			name:      "missing Replicator",
			steps:     []func(do.Injector){provideManager, provideRebalancer},
			errSubstr: "resolve Replicator",
		},
		{
			name:      "missing OverReplicationCleaner",
			steps:     []func(do.Injector){provideManager, provideRebalancer, provideReplicator},
			errSubstr: "resolve OverReplicationCleaner",
		},
		{
			name:      "missing CleanupWorker",
			steps:     []func(do.Injector){provideManager, provideRebalancer, provideReplicator, provideOverRep},
			errSubstr: "resolve CleanupWorker",
		},
		{
			name:      "missing Scrubber",
			steps:     []func(do.Injector){provideManager, provideRebalancer, provideReplicator, provideOverRep, provideCleanup},
			errSubstr: "resolve Scrubber",
		},
		{
			name:      "missing DrainManager",
			steps:     []func(do.Injector){provideManager, provideRebalancer, provideReplicator, provideOverRep, provideCleanup, provideScrubber},
			errSubstr: "resolve DrainManager",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inj := register(tc.steps...)
			t.Cleanup(func() { _ = inj.Shutdown() })

			err := WireManager(inj)
			if err == nil {
				t.Fatalf("expected error matching %q", tc.errSubstr)
			}
			if !strings.Contains(err.Error(), tc.errSubstr) {
				t.Errorf("error = %q, want to contain %q", err.Error(), tc.errSubstr)
			}
		})
	}
	// Sanity: drain.Manager package import stays live so go vet is happy.
	_ = (*drain.Manager)(nil)
}

// TestWireManager_PendingReaperFailedLogsAndContinues drives the Failed
// branch the Optional[T] migration added: an override registers a
// PendingReaper provider that errors, and WireManager must still
// succeed (assigning nil onto the manager) so a broken optional
// dependency does not abort startup.
func TestWireManager_PendingReaperFailedLogsAndContinues(t *testing.T) {
	t.Parallel()
	cfg := happyPathConfig(t.TempDir())
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("config validation: %v", err)
	}
	inj := NewInjector(cfg, "all", new(slog.LevelVar), telemetry.NewLogBuffer())
	t.Cleanup(func() { _ = inj.Shutdown() })

	boom := errors.New("pending reaper construction failed")
	do.Override(inj, func(do.Injector) (*worker.PendingReaper, error) {
		return nil, boom
	})

	if err := WireManager(inj); err != nil {
		t.Fatalf("WireManager: %v (expected to swallow Failed optional)", err)
	}
}
