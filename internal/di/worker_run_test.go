// -------------------------------------------------------------------------------
// Worker Run-on-Request Tests
//
// Author: Alex Freidah
//
// Covers tickrunner.Service.RunNow, the tick an operator asks for: it runs
// through the same lock and health recording as a scheduled tick, reports
// the work's error, and reports a disabled worker or one another instance
// holds without running it.
// -------------------------------------------------------------------------------

package di

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle/tickrunner"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestRunNow_RunsAndRecordsTheTick verifies a run on request does the work
// and advances the worker's health like a scheduled tick.
func TestRunNow_RunsAndRecordsTheTick(t *testing.T) {
	t.Parallel()
	ran := false
	svc := healthSvc(acquiringLocker{}, "test", func(context.Context) error { ran = true; return nil })
	if err := svc.RunNow(context.Background()); err != nil {
		t.Fatalf("RunNow = %v, want nil", err)
	}
	if !ran || svc.Health().LastSuccess.IsZero() {
		t.Errorf("ran=%v lastSuccess=%v, want the work run and recorded", ran, svc.Health().LastSuccess)
	}
}

// TestRunNow_ReportsWhyItDidNotSucceed verifies each way a run on request can
// end other than success reaches the caller.
func TestRunNow_ReportsWhyItDidNotSucceed(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	cases := map[string]struct {
		locker tickrunner.AdvisoryLocker
		work   error
		want   error
	}{
		"work fails":           {locker: acquiringLocker{}, work: boom, want: boom},
		"lock held elsewhere":  {locker: fakeLocker{}, want: lifecycle.ErrWorkerBusy},
		"database unavailable": {locker: errLocker{err: core.ErrDBUnavailable}, want: core.ErrDBUnavailable},
		"lock errors":          {locker: errLocker{err: errors.New("lock broke")}, want: errors.New("lock broke")},
	}
	for name, tc := range cases {
		svc := healthSvc(tc.locker, "test", func(context.Context) error { return tc.work })
		err := svc.RunNow(context.Background())
		if err == nil || (!errors.Is(err, tc.want) && err.Error() != "advisory lock: "+tc.want.Error()) {
			t.Errorf("%s: RunNow = %v, want %v", name, err, tc.want)
		}
	}
}

// TestRunNow_RefusesADisabledWorker verifies a worker whose ShouldRun says no
// is reported as disabled and its work never runs.
func TestRunNow_RefusesADisabledWorker(t *testing.T) {
	t.Parallel()
	ran := false
	svc := tickrunner.New(tickrunner.Config{
		Locker: acquiringLocker{}, Interval: time.Second, LockID: core.LockRebalancer, Name: "test", Log: slog.Default(),
		ShouldRun: func() bool { return false },
		Work:      func(context.Context) error { ran = true; return nil },
	})
	if err := svc.RunNow(context.Background()); !errors.Is(err, lifecycle.ErrWorkerDisabled) {
		t.Errorf("RunNow = %v, want ErrWorkerDisabled", err)
	}
	if ran {
		t.Error("a disabled worker's work ran")
	}
}

// TestManagerRunNow_FindsTheWorkerByItsHealthName verifies the manager runs
// a worker by the name its health is reported under, not the name it was
// registered with, and reports a name nothing answers to.
func TestManagerRunNow_FindsTheWorkerByItsHealthName(t *testing.T) {
	t.Parallel()
	ran := false
	m := lifecycle.NewManager()
	m.Register("replicator", healthSvc(acquiringLocker{}, "replication", func(context.Context) error { ran = true; return nil }))

	if err := m.RunNow(context.Background(), "replication"); err != nil || !ran {
		t.Errorf("RunNow(replication) = %v ran=%v, want the worker run", err, ran)
	}
	if err := m.RunNow(context.Background(), "replicator"); !errors.Is(err, lifecycle.ErrUnknownWorker) {
		t.Errorf("RunNow(registration name) = %v, want ErrUnknownWorker", err)
	}
}
