// -------------------------------------------------------------------------------
// Periodic Background Service Validation Tests
//
// Author: Alex Freidah
//
// Operational invariants that protect against drift as new background
// services are added:
//
//   - every lockedTickerService uses a unique advisory lockID
//   - every interval is non-zero and within sane operational bounds
//   - the lifecycle manager registers the expected service set per run mode
//
// These tests build services via the existing servicesFixture (lockID
// and interval shape) and via NewInjector (mode matrix). They do not
// exercise tick behavior  -  services_test.go and lifecycle_test.go
// own that.
// -------------------------------------------------------------------------------

package di

import (
	"log/slog"
	"testing"
	"time"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle/tickrunner"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/multipart"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// allLockedTickerServices builds every locked-ticker background service
// the lifecycle manager registers in worker/all mode. Used by the
// lockID-uniqueness and interval-sanity assertions below.
func allLockedTickerServices(t *testing.T) []*tickrunner.Service {
	t.Helper()
	f := newServicesFixture(t)
	locker := fakeLocker{}

	runners := []lifecycle.Runner{
		multipart.NewCleanupService(f.mgr.Multipart(), locker, 0),
		worker.NewCleanupQueueService(f.cleanupWorker, locker),
		worker.NewRebalancerService(f.mgr, f.rebalancer, locker),
		NewLifecycleService(f.mgr, locker),
		worker.NewOverReplicationService(f.mgr, f.overRep, locker),
		worker.NewReplicatorService(f.mgr, f.replicator, locker),
		worker.NewReconcileService(worker.NewReconciler(f.mgr, nil), locker, time.Hour),
		worker.NewScrubberService(f.scrubber, locker),
	}
	out := make([]*tickrunner.Service, 0, len(runners))
	for _, r := range runners {
		ts, ok := r.(*tickrunner.Service)
		if !ok {
			t.Fatalf("runner %T is not *tickrunner.Service", r)
		}
		out = append(out, ts)
	}
	return out
}

// TestPeriodicServiceSpecs_LockIDsUnique pins the operational invariant
// that no two background services share an advisory-lock ID. Two
// services on the same lockID would silently serialize their ticks
// across instances, masking either work as "lock held by another
// instance".
func TestPeriodicServiceSpecs_LockIDsUnique(t *testing.T) {
	t.Parallel()
	services := allLockedTickerServices(t)
	seen := make(map[int64]string, len(services))
	for _, s := range services {
		if prev, ok := seen[s.LockID()]; ok {
			t.Errorf("lockID %d shared by %q and %q", s.LockID(), prev, s.Name())
			continue
		}
		seen[s.LockID()] = s.Name()
	}
}

// TestPeriodicServiceSpecs_IntervalsSane catches accidental zero or
// pathologically large intervals. The bounds are deliberately wide
// (1s..24h) so legitimate cron-like cadences pass but a typo dropping
// the unit (e.g. 5 instead of 5*time.Minute) does not.
func TestPeriodicServiceSpecs_IntervalsSane(t *testing.T) {
	t.Parallel()
	const (
		minInterval = 1 * time.Second
		maxInterval = 24 * time.Hour
	)
	for _, s := range allLockedTickerServices(t) {
		switch {
		case s.Interval() < minInterval:
			t.Errorf("%s: interval %s is below %s", s.Name(), s.Interval(), minInterval)
		case s.Interval() > maxInterval:
			t.Errorf("%s: interval %s exceeds %s", s.Name(), s.Interval(), maxInterval)
		}
	}
}

// lifecycleManagerForMode builds the DI injector for a mode and returns
// the resolved lifecycle manager. Hoisted out of the mode-matrix test
// so the test body stays a flat sequence of subtests.
func lifecycleManagerForMode(t *testing.T, mode string) *lifecycle.Manager {
	t.Helper()
	cfg := happyPathConfig(t.TempDir())
	cfg.WritePath.PendingPattern.Enabled = new(true)
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("config validation: %v", err)
	}
	inj := NewInjector(cfg, mode, new(slog.LevelVar), telemetry.NewLogBuffer())
	t.Cleanup(func() { _ = inj.Shutdown() })
	mgr, err := do.Invoke[*lifecycle.Manager](inj)
	if err != nil {
		t.Fatalf("resolve LifecycleManager: %v", err)
	}
	return mgr
}

// assertModeRegistrations checks that every name in mustContain is in
// names and every name in mustNotHave is absent. Hoisted to keep the
// matrix loop body free of nested map/loop branches.
func assertModeRegistrations(t *testing.T, mode string, names, mustContain, mustNotHave []string) {
	t.Helper()
	have := make(map[string]struct{}, len(names))
	for _, n := range names {
		have[n] = struct{}{}
	}
	for _, want := range mustContain {
		if _, ok := have[want]; !ok {
			t.Errorf("mode=%s missing service %q (registered: %v)", mode, want, names)
		}
	}
	for _, banned := range mustNotHave {
		if _, ok := have[banned]; ok {
			t.Errorf("mode=%s registered worker-only service %q (registered: %v)", mode, banned, names)
		}
	}
}

// TestLifecycleManager_ModeMatrix pins the per-mode service set so a
// future change that registers a worker service in proxy mode (or
// drops one from worker mode) surfaces here. The expected sets match
// the registration logic in ProvideLifecycleManager + registerWorkerServices.
func TestLifecycleManager_ModeMatrix(t *testing.T) {
	t.Parallel()

	proxyAlwaysOn := []string{"usage-flush", "cb-watchdog"}
	workerExtras := []string{
		"multipart-cleanup",
		"cleanup-queue",
		"pending-reaper",
		"rebalancer",
		"replicator",
		"over-replication",
		"lifecycle",
		"scrubber",
	}
	allModes := append(append([]string{}, proxyAlwaysOn...), workerExtras...)

	tests := []struct {
		mode        string
		mustContain []string
		mustNotHave []string
	}{
		{mode: "proxy", mustContain: proxyAlwaysOn, mustNotHave: workerExtras},
		{mode: "worker", mustContain: allModes},
		{mode: "all", mustContain: allModes},
	}

	for _, tc := range tests {
		t.Run(tc.mode, func(t *testing.T) {
			t.Parallel()
			mgr := lifecycleManagerForMode(t, tc.mode)
			assertModeRegistrations(t, tc.mode, mgr.Names(), tc.mustContain, tc.mustNotHave)
		})
	}
}
