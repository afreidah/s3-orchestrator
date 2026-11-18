// -------------------------------------------------------------------------------
// Every Tests
//
// Author: Alex Freidah
//
// Pins when the loop calls its function: after each interval, once straight
// away when asked, later by the jitter on the first wait, at the period Next
// returns, and never once its context ends. Also pins that Sleep reports
// whether its wait ran, and that Receive gives up when its context ends.
// -------------------------------------------------------------------------------

package syncutil

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// runFor runs the schedule for d of fake time and returns how many calls it
// made.
func runFor(t *testing.T, s Schedule, d time.Duration) int64 {
	t.Helper()
	var calls atomic.Int64
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx, func(context.Context) { calls.Add(1) })
	}()
	time.Sleep(d)
	synctest.Wait()
	cancel()
	<-done
	return calls.Load()
}

// TestSchedule_CallsOncePerInterval verifies a plain loop calls once per
// interval, starting one interval in.
func TestSchedule_CallsOncePerInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		if got := runFor(t, Schedule{Interval: time.Second}, 3*time.Second); got != 3 {
			t.Errorf("calls = %d, want 3", got)
		}
	})
}

// TestSchedule_ImmediatelyCallsFirst verifies Immediately adds a call before
// the first wait.
func TestSchedule_ImmediatelyCallsFirst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		got := runFor(t, Schedule{Interval: time.Second, Immediately: true}, 2500*time.Millisecond)
		if got != 3 {
			t.Errorf("calls = %d, want 3", got)
		}
	})
}

// TestSchedule_JitterDelaysOnlyTheFirstCall verifies the jitter lands within
// half an interval of the first call and leaves the period alone after it.
func TestSchedule_JitterDelaysOnlyTheFirstCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := Schedule{Interval: 2 * time.Second, Jitter: true}
		if got := runFor(t, s, 2*time.Second-time.Millisecond); got != 0 {
			t.Errorf("calls before the first interval = %d, want 0", got)
		}
		if got := runFor(t, s, 3*time.Second); got != 1 {
			t.Errorf("calls by one and a half intervals = %d, want 1", got)
		}
	})
}

// TestSchedule_NextSetsEachWait verifies Next is read before every wait, so a
// loop can change its period between calls.
func TestSchedule_NextSetsEachWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		waits := []time.Duration{time.Second, 3 * time.Second, time.Second}
		var n int
		s := Schedule{Next: func() time.Duration {
			w := waits[min(n, len(waits)-1)]
			n++
			return w
		}}
		if got := runFor(t, s, 5*time.Second); got != 3 {
			t.Errorf("calls after waits of 1s, 3s, 1s = %d, want 3", got)
		}
	})
}

// TestEvery_StopsWhenContextEnds verifies a cancelled context makes no call
// and returns.
func TestEvery_StopsWhenContextEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		var calls atomic.Int64
		Every(ctx, time.Second, func(context.Context) { calls.Add(1) })
		if calls.Load() != 0 {
			t.Errorf("calls = %d, want 0", calls.Load())
		}
	})
}

// TestSleep_ReportsWhetherTheWaitRan verifies Sleep reports a full wait as
// true and gives up with false once its context ends.
func TestSleep_ReportsWhetherTheWaitRan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		if !Sleep(t.Context(), time.Second) || time.Since(start) != time.Second {
			t.Errorf("full wait: want true after 1s, slept %v", time.Since(start))
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		start = time.Now()
		if Sleep(ctx, time.Hour) || time.Since(start) != time.Second {
			t.Errorf("cancelled wait: want false after 1s, slept %v", time.Since(start))
		}
	})
}

// TestReceive_ReturnsTheValueOrTheContextError verifies Receive returns a sent
// value, or the context's error when nothing is sent.
func TestReceive_ReturnsTheValueOrTheContextError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ch := make(chan int, 1)
		ch <- 7
		if v, err := Receive(t.Context(), ch); v != 7 || err != nil {
			t.Errorf("sent value: got %d, %v; want 7, nil", v, err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if v, err := Receive(ctx, ch); v != 0 || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("nothing sent: got %d, %v; want 0, deadline exceeded", v, err)
		}
	})
}
