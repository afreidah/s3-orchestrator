// -------------------------------------------------------------------------------
// PressureTracker - Tests
//
// Author: Alex Freidah
//
// Drives the tracker with calls at chosen times and judges it at chosen times,
// covering each reason, the minimum-calls lapse, the release at half the
// threshold, the baseline frozen while under pressure, the switch, and the
// service that judges on a tick.
// -------------------------------------------------------------------------------

package counter

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// The call times the tests use: a backend's normal call, one over three times
// it, and the backend timeout.
const (
	fastCall    = 10 * time.Millisecond
	slowCall    = 100 * time.Millisecond
	timeoutCall = 30 * time.Second
)

// pressureClock hands out call times 100ms apart.
type pressureClock struct{ now time.Time }

// calls records n calls of elapsed to backend, one per tick of the clock.
func (c *pressureClock) calls(tr *PressureTracker, backend string, n int, elapsed time.Duration, timedOut bool) {
	for range n {
		tr.observeAt(backend, c.now, elapsed, timedOut)
		c.now = c.now.Add(100 * time.Millisecond)
	}
}

// newTestPressureTracker builds a tracker over backends under the default
// policy, switched on unless enabled says otherwise.
func newTestPressureTracker(enabled bool, backends ...string) *PressureTracker {
	cfg := config.BackendPressureConfig{Enabled: new(enabled), PressurePolicy: config.DefaultPressurePolicy()}
	return NewPressureTracker(backends, syncutil.Fixed(&cfg))
}

// TestPressureTracker_SlowCallsDemote asserts a backend whose calls slow to
// over three times its baseline comes under slow pressure and reports it on
// its gauge, while a backend that stays fast does not.
func TestPressureTracker_SlowCallsDemote(t *testing.T) {
	t.Parallel()
	const backend = "pressure-slow-test"
	tr := newTestPressureTracker(true, backend, "steady")
	clock := &pressureClock{now: time.Now()}
	clock.calls(tr, backend, 100, fastCall, false)
	clock.calls(tr, "steady", 100, fastCall, false)
	clock.calls(tr, backend, 60, slowCall, false)
	tr.Evaluate(t.Context(), clock.now)

	if got := tr.Pressure(backend); got != PressureSlow {
		t.Errorf("slowed backend = %s, want slow", got)
	}
	if got := tr.Pressure("steady"); got != 0 {
		t.Errorf("steady backend = %s, want none", got)
	}
	if got := testutil.ToFloat64(telemetry.BackendPressured.WithLabelValues(backend, "slow")); got != 1 {
		t.Errorf("slow gauge = %v, want 1", got)
	}
	if st := tr.State(backend); st.Baseline < fastCall || st.Baseline >= slowCall {
		t.Errorf("baseline = %v, want near the fast calls", st.Baseline)
	}
}

// TestPressureTracker_TimeoutsDemote asserts a backend whose calls hit the
// backend timeout one time in twenty comes under timeout pressure.
func TestPressureTracker_TimeoutsDemote(t *testing.T) {
	t.Parallel()
	tr := newTestPressureTracker(true, "b1")
	clock := &pressureClock{now: time.Now()}
	clock.calls(tr, "b1", 50, fastCall, false)
	clock.calls(tr, "b1", 5, timeoutCall, true)
	tr.Evaluate(t.Context(), clock.now)

	if got := tr.Pressure("b1"); got != PressureTimeouts {
		t.Errorf("pressure = %s, want timeouts", got)
	}
}

// TestPressureTracker_TooFewCallsJudgeNothing asserts a backend with fewer
// calls than the minimum is under no pressure, whether it never had enough or
// its calls stopped while it was under pressure.
func TestPressureTracker_TooFewCallsJudgeNothing(t *testing.T) {
	t.Parallel()
	tr := newTestPressureTracker(true, "few", "stopped")
	clock := &pressureClock{now: time.Now()}
	clock.calls(tr, "few", 10, timeoutCall, true)
	clock.calls(tr, "stopped", 50, fastCall, false)
	clock.calls(tr, "stopped", 50, slowCall, false)
	tr.Evaluate(t.Context(), clock.now)
	if got := tr.Pressure("few"); got != 0 {
		t.Errorf("backend with 10 calls = %s, want none", got)
	}
	if tr.Pressure("stopped") == 0 {
		t.Fatal("backend was not under pressure before its calls stopped")
	}

	tr.Evaluate(t.Context(), clock.now.Add(5*time.Minute))
	if got := tr.Pressure("stopped"); got != 0 {
		t.Errorf("backend with no calls for 5m = %s, want none", got)
	}
}

// TestPressureTracker_HoldsUntilHalfTheThreshold asserts a backend under
// pressure stays under it while its share is between half the threshold and
// the threshold, and leaves once it drops below half.
func TestPressureTracker_HoldsUntilHalfTheThreshold(t *testing.T) {
	t.Parallel()
	tr := newTestPressureTracker(true, "b1")
	clock := &pressureClock{now: time.Now()}
	clock.calls(tr, "b1", 100, fastCall, false)
	clock.calls(tr, "b1", 60, slowCall, false)
	tr.Evaluate(t.Context(), clock.now)
	if tr.Pressure("b1") == 0 {
		t.Fatal("backend was not demoted")
	}

	clock.calls(tr, "b1", 150, fastCall, false)
	tr.Evaluate(t.Context(), clock.now)
	if s := tr.State("b1").Deciding().Slow; s >= 0.25 || s < 0.125 {
		t.Fatalf("slow share = %v, want between half the threshold and the threshold", s)
	}
	if tr.Pressure("b1") == 0 {
		t.Error("backend left pressure above half the threshold")
	}

	clock.calls(tr, "b1", 300, fastCall, false)
	tr.Evaluate(t.Context(), clock.now)
	if got := tr.Pressure("b1"); got != 0 {
		t.Errorf("backend at slow share %v = %s, want none", tr.State("b1").Deciding().Slow, got)
	}
}

// TestPressureTracker_BaselineFrozenWhileDemoted asserts the calls a backend
// answers while under pressure do not move its baseline.
func TestPressureTracker_BaselineFrozenWhileDemoted(t *testing.T) {
	t.Parallel()
	tr := newTestPressureTracker(true, "b1")
	clock := &pressureClock{now: time.Now()}
	clock.calls(tr, "b1", 100, fastCall, false)
	clock.calls(tr, "b1", 60, slowCall, false)
	tr.Evaluate(t.Context(), clock.now)
	before := tr.State("b1").Baseline

	clock.calls(tr, "b1", 200, slowCall, false)
	tr.Evaluate(t.Context(), clock.now)
	if after := tr.State("b1").Baseline; after != before {
		t.Errorf("baseline moved from %v to %v while under pressure", before, after)
	}
}

// TestPressureService_JudgesOnItsTick asserts the service judges the calls the
// tracker was handed once its tick comes round.
func TestPressureService_JudgesOnItsTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := newTestPressureTracker(true, "b1")
		for range 30 {
			tr.Observe("b1", timeoutCall, true)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = NewPressureService(tr).Run(ctx)
		}()
		time.Sleep(pressureTick)
		synctest.Wait()
		cancel()
		<-done
		if got := tr.Pressure("b1"); got != PressureTimeouts {
			t.Errorf("pressure after a tick = %s, want timeouts", got)
		}
	})
}

// TestPressureTracker_DisabledJudgesNothing asserts a tracker switched off
// puts no backend under pressure, and ignores calls to a backend it does not
// track.
func TestPressureTracker_DisabledJudgesNothing(t *testing.T) {
	t.Parallel()
	tr := newTestPressureTracker(false, "b1")
	clock := &pressureClock{now: time.Now()}
	clock.calls(tr, "b1", 100, fastCall, false)
	clock.calls(tr, "b1", 60, timeoutCall, true)
	clock.calls(tr, "unknown", 10, timeoutCall, true)
	tr.Evaluate(t.Context(), clock.now)

	if got := tr.Pressure("b1"); got != 0 {
		t.Errorf("pressure while switched off = %s, want none", got)
	}
	if st := tr.State("b1"); st.Deciding().Timeouts == 0 {
		t.Error("shares stopped being measured while switched off")
	}
}
