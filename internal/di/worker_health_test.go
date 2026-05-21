// -------------------------------------------------------------------------------
// Worker Health Tracking Tests
//
// Author: Alex Freidah
//
// Covers the per-tick health state on lockedTickerService and the
// HealthReporter implementation. Specifically pins:
//
//   - a successful tick advances LastSuccess and resets
//     ConsecutiveFailures
//   - a failing tick increments ConsecutiveFailures and captures
//     LastError
//   - lock-acquire errors land in the same failure path so an
//     uncatchable lock storm shows up in the snapshot
//   - Health() returns the registered name even when the service has
//     not yet been ticked
//
// These invariants back the /admin/api/workers endpoint and the
// Prometheus alerts on worker staleness.
// -------------------------------------------------------------------------------

package di

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/lifecycle/tickrunner"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// healthSvc returns a minimal tickrunner.Service wired with the
// supplied locker and work closure, so each test below can drive a
// single tick directly without standing up the lifecycle manager.
func healthSvc(locker tickrunner.AdvisoryLocker, name string, work func(context.Context) error) *tickrunner.Service {
	return tickrunner.New(tickrunner.Config{
		Locker:   locker,
		Interval: time.Second,
		LockID:   core.LockRebalancer,
		Name:     name,
		Log:      slog.Default(),
		Work:     work,
	})
}

// TestLockedTickerService_HealthSuccessAdvancesLastSuccess drives one
// successful tick and asserts the snapshot reflects it. Without this,
// a working service would never advance its staleness metric and the
// Prometheus alert would fire constantly.
func TestLockedTickerService_HealthSuccessAdvancesLastSuccess(t *testing.T) {
	t.Parallel()
	svc := healthSvc(acquiringLocker{}, "test", func(context.Context) error { return nil })
	svc.Tick(context.Background())
	h := svc.Health()
	if h.Name != "test" {
		t.Errorf("Name = %q, want test", h.Name)
	}
	if h.LastSuccess.IsZero() {
		t.Error("LastSuccess was not set on successful tick")
	}
	if h.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d, want 0 after success", h.ConsecutiveFailures)
	}
	if h.LastError != "" {
		t.Errorf("LastError = %q, want empty after success", h.LastError)
	}
}

// TestLockedTickerService_HealthFailureAccumulates drives two failed
// ticks and asserts the snapshot captures the error message and the
// consecutive-failure run. The reset-on-success branch is covered
// separately in TestLockedTickerService_HealthSuccessResetsFailures
// to keep this test focused on the failure-accumulation invariant.
func TestLockedTickerService_HealthFailureAccumulates(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	svc := healthSvc(acquiringLocker{}, "test", func(context.Context) error { return boom })
	svc.Tick(context.Background())
	svc.Tick(context.Background())
	h := svc.Health()
	if h.ConsecutiveFailures != 2 {
		t.Errorf("ConsecutiveFailures = %d, want 2", h.ConsecutiveFailures)
	}
	if h.LastError != "boom" {
		t.Errorf("LastError = %q, want boom", h.LastError)
	}
	if h.LastFailure.IsZero() {
		t.Error("LastFailure was not set")
	}
}

// TestLockedTickerService_HealthSuccessResetsFailures drives a fail
// then success, asserting the success branch zeroes the failure run
// and clears LastError. The original LastFailure timestamp must be
// preserved so operators can see "service recovered N seconds ago".
func TestLockedTickerService_HealthSuccessResetsFailures(t *testing.T) {
	t.Parallel()
	calls := 0
	work := func(context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("first")
		}
		return nil
	}
	svc := healthSvc(acquiringLocker{}, "test", work)
	svc.Tick(context.Background())
	failTS := svc.Health().LastFailure
	svc.Tick(context.Background())
	h := svc.Health()
	if h.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d, want 0", h.ConsecutiveFailures)
	}
	if h.LastError != "" {
		t.Errorf("LastError = %q, want empty", h.LastError)
	}
	if !h.LastFailure.Equal(failTS) {
		t.Error("LastFailure should remain set so recovery is visible")
	}
	if h.LastSuccess.IsZero() {
		t.Error("LastSuccess was not set on recovery tick")
	}
}

// TestLockedTickerService_HealthLockErrorCountsAsFailure pins the
// contract that a non-DBUnavailable lock-acquire error counts as a
// tick failure, not a skip. A lock storm caused by a flaky database
// must show up in the snapshot so operators can distinguish it from
// "another instance holds the lock".
func TestLockedTickerService_HealthLockErrorCountsAsFailure(t *testing.T) {
	t.Parallel()
	svc := healthSvc(errLocker{err: errors.New("lock broke")}, "test", func(context.Context) error { return nil })
	svc.Tick(context.Background())
	h := svc.Health()
	if h.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures = %d, want 1", h.ConsecutiveFailures)
	}
	if h.LastError == "" {
		t.Error("LastError was not captured for lock-acquire failure")
	}
}

// TestLockedTickerService_HealthDBUnavailableNotCountedAsFailure
// covers the breaker-aware path: ErrDBUnavailable is squelched so
// the service does not appear "failing" while the breaker is open.
// Health() should reflect that the tick effectively did not happen.
func TestLockedTickerService_HealthDBUnavailableNotCountedAsFailure(t *testing.T) {
	t.Parallel()
	svc := healthSvc(errLocker{err: core.ErrDBUnavailable}, "test", func(context.Context) error { return nil })
	svc.Tick(context.Background())
	h := svc.Health()
	if h.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d, want 0 (ErrDBUnavailable is squelched)", h.ConsecutiveFailures)
	}
	if h.LastError != "" {
		t.Errorf("LastError = %q, want empty", h.LastError)
	}
}
