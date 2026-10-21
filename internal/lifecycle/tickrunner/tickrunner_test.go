// -------------------------------------------------------------------------------
// Tick Runner Tests
//
// Author: Alex Freidah
//
// Pins what one tick records for each lock and work outcome, and drives Run on
// a fake clock to pin the startup hook, the jittered first tick, one tick per
// interval, ShouldRun gating and cancellation.
// -------------------------------------------------------------------------------

package tickrunner

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/testutil/testx"
)

// interval is the tick period the Run tests use; the first tick lands within
// interval/2 of jitter after Run starts.
const interval = 10 * time.Second

// newService builds a Service named after the test, so the shared worker
// metrics it bumps are its own.
//
//nolint:gocritic // hugeParam: mirrors New, which takes Config by value.
func newService(t *testing.T, cfg Config) *Service {
	t.Helper()
	cfg.Name = t.Name()
	cfg.Interval = interval
	cfg.LockID = core.LockRebalancer
	cfg.Log = slog.New(slog.DiscardHandler)
	if cfg.Work == nil {
		cfg.Work = func(context.Context) error { return nil }
	}
	return New(cfg)
}

// ticks reads the worker tick counter for one outcome of the test's service.
func ticks(t *testing.T, outcome string) float64 {
	t.Helper()
	return testutil.ToFloat64(telemetry.WorkerTicksTotal.WithLabelValues(t.Name(), outcome))
}

// TestTick_RecordsEachOutcome pins what one tick records: a held lock or an
// unavailable database is a skip, a lock error and a work error are failures.
func TestTick_RecordsEachOutcome(t *testing.T) {
	workErr := errors.New("work broke")
	lockErr := errors.New("lock broke")
	tests := []struct {
		name         string
		locker       testx.Locker
		work         error
		wantRan      bool
		wantFailures int
		wantErr      string
		wantSkipped  float64
	}{
		{"work succeeds", testx.Locker{}, nil, true, 0, "", 0},
		{"work fails", testx.Locker{}, workErr, true, 1, "work broke", 0},
		{"lock held elsewhere", testx.Locker{Busy: true}, nil, false, 0, "", 1},
		{"lock fails", testx.Locker{Err: lockErr}, nil, false, 1, "advisory lock: lock broke", 0},
		{"database unavailable", testx.Locker{Err: core.ErrDBUnavailable}, nil, false, 0, "", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ran := false
			svc := newService(t, Config{
				Locker: tt.locker,
				Work:   func(context.Context) error { ran = true; return tt.work },
			})

			svc.Tick(t.Context())

			h := svc.Health()
			if ran != tt.wantRan {
				t.Errorf("work ran = %v, want %v", ran, tt.wantRan)
			}
			if h.Name != t.Name() || h.ConsecutiveFailures != tt.wantFailures || h.LastError != tt.wantErr {
				t.Errorf("health = %+v, want %d failures and error %q", h, tt.wantFailures, tt.wantErr)
			}
			if got := ticks(t, "skipped"); got != tt.wantSkipped {
				t.Errorf("skipped ticks = %v, want %v", got, tt.wantSkipped)
			}
		})
	}
}

// TestTick_OnErrorReplacesTheLog asserts a lock error goes to OnError, and an
// unavailable database does not.
func TestTick_OnErrorReplacesTheLog(t *testing.T) {
	lockErr := errors.New("lock broke")
	for _, tt := range []struct {
		err  error
		want error
	}{{lockErr, lockErr}, {core.ErrDBUnavailable, nil}} {
		var got error
		svc := newService(t, Config{Locker: testx.Locker{Err: tt.err}, OnError: func(err error) { got = err }})
		svc.Tick(t.Context())
		if !errors.Is(got, tt.want) {
			t.Errorf("lock error %v: OnError got %v, want %v", tt.err, got, tt.want)
		}
	}
}

// TestTick_SuccessResetsFailures asserts failures count up across ticks and a
// success clears them along with the last error, keeping LastFailure so a
// recovery stays visible.
func TestTick_SuccessResetsFailures(t *testing.T) {
	fail := true
	svc := newService(t, Config{
		Locker: testx.Locker{},
		Work: func(context.Context) error {
			if fail {
				return errors.New("work broke")
			}
			return nil
		},
	})
	svc.Tick(t.Context())
	svc.Tick(t.Context())
	failed := svc.Health()
	if failed.ConsecutiveFailures != 2 || failed.LastFailure.IsZero() {
		t.Fatalf("after two failures health = %+v, want 2 failures", failed)
	}

	fail = false
	svc.Tick(t.Context())
	h := svc.Health()
	if h.ConsecutiveFailures != 0 || h.LastError != "" || h.LastSuccess.IsZero() {
		t.Errorf("after a success health = %+v, want failures and error cleared", h)
	}
	if !h.LastFailure.Equal(failed.LastFailure) {
		t.Errorf("LastFailure = %v, want %v kept after recovery", h.LastFailure, failed.LastFailure)
	}
}

// refresher counts quota metric refreshes and fails them with err.
type refresher struct {
	calls int
	err   error
}

func (r *refresher) UpdateQuotaMetrics(context.Context) error {
	r.calls++
	return r.err
}

// TestHandlePassResult pins the pass workers' shared result handling: an
// unavailable database is not a failure, any other error is, and only a pass
// that did work refreshes the quota metrics, whose own failure is not the
// pass's.
func TestHandlePassResult(t *testing.T) {
	boom := errors.New("worker broke")
	tests := []struct {
		name        string
		count       int
		err         error
		refreshErr  error
		wantErr     error
		wantRefresh int
	}{
		{"no work", 0, nil, nil, nil, 0},
		{"work done refreshes metrics", 7, nil, nil, nil, 1},
		{"refresh failure is not the pass's", 7, nil, errors.New("refresh broke"), nil, 1},
		{"database unavailable", 0, core.ErrDBUnavailable, nil, nil, 0},
		{"worker error", 0, boom, nil, boom, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &refresher{err: tt.refreshErr}
			err := HandlePassResult(t.Context(), slog.New(slog.DiscardHandler), r, tt.count, tt.err, "objects")
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
			if r.calls != tt.wantRefresh {
				t.Errorf("quota refreshes = %d, want %d", r.calls, tt.wantRefresh)
			}
		})
	}
}

// TestRun_StartupThenOneTickPerInterval drives Run on a fake clock: the
// startup hook runs once before anything else, the first tick lands after the
// jitter and one interval, and each interval after that adds exactly one.
func TestRun_StartupThenOneTickPerInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var startups, works atomic.Int32
		svc := newService(t, Config{
			Locker:  testx.Locker{},
			Startup: func(context.Context) error { startups.Add(1); return nil },
			Work:    func(context.Context) error { works.Add(1); return nil },
		})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error)
		go func() { done <- svc.Run(ctx) }()

		synctest.Wait()
		if startups.Load() != 1 || works.Load() != 0 {
			t.Fatalf("before the first tick: startups=%d works=%d, want 1 and 0", startups.Load(), works.Load())
		}

		// The jitter is under interval/2, so three intervals past its upper
		// bound hold exactly three ticks.
		synctest.Sleep(interval/2 + 3*interval)
		if got := works.Load(); got != 3 {
			t.Errorf("ticks after three intervals = %d, want 3", got)
		}

		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run returned %v on cancel, want nil", err)
		}
		if startups.Load() != 1 {
			t.Errorf("startup ran %d times, want once", startups.Load())
		}
	})
}

// TestRun_ShouldRunFalseSkipsTheTick asserts a tick ShouldRun declines runs no
// work and is counted as skipped rather than vanishing.
func TestRun_ShouldRunFalseSkipsTheTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var works atomic.Int32
		svc := newService(t, Config{
			Locker:    testx.Locker{},
			ShouldRun: func() bool { return false },
			Work:      func(context.Context) error { works.Add(1); return nil },
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = svc.Run(ctx) }()

		synctest.Sleep(interval/2 + 2*interval)
		if works.Load() != 0 {
			t.Errorf("work ran %d times while ShouldRun declined", works.Load())
		}
		if got := ticks(t, "skipped"); got != 2 {
			t.Errorf("skipped ticks = %v, want 2", got)
		}
	})
}

// TestRun_CancelDuringJitterRunsNoTick asserts a service stopped before its
// first tick returns cleanly without running the work.
func TestRun_CancelDuringJitterRunsNoTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ran := false
		svc := newService(t, Config{Locker: testx.Locker{}, Work: func(context.Context) error { ran = true; return nil }})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := svc.Run(ctx); err != nil || ran {
			t.Errorf("Run = %v, work ran = %v; want nil and no tick", err, ran)
		}
	})
}
