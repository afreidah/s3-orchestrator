// -------------------------------------------------------------------------------
// Lifecycle Manager Tests
//
// Author: Alex Freidah
//
// Tests for the background service lifecycle manager. Covers service registration,
// graceful shutdown propagation, and concurrent service orchestration. Every
// test runs on a fake clock, so restart backoff is checked at its production
// values and counts are exact.
// -------------------------------------------------------------------------------

package lifecycle

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// -------------------------------------------------------------------------
// TEST HELPERS
// -------------------------------------------------------------------------

// counterService counts ticks of a 10ms ticker until its context ends.
type counterService struct {
	count atomic.Int64
}

// Run ticks until ctx is done.
func (s *counterService) Run(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.count.Add(1)
		case <-ctx.Done():
			return nil
		}
	}
}

// failingService fails its first failures runs, by panicking or by returning
// an error, and then runs until its context ends.
type failingService struct {
	calls    atomic.Int64
	failures int64
	panics   bool
}

// Run fails while calls are within failures, then blocks until ctx is done.
func (s *failingService) Run(ctx context.Context) error {
	if s.calls.Add(1) <= s.failures {
		if s.panics {
			panic("boom")
		}
		return context.DeadlineExceeded
	}
	<-ctx.Done()
	return nil
}

// stoppableService records its Stop call on stopped. With slow set, Stop
// first waits out its whole budget.
type stoppableService struct {
	stopped chan string
	name    string
	slow    bool
	err     error
}

// Run blocks until ctx is done.
func (s *stoppableService) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// Stop records the call, after waiting out ctx when slow.
func (s *stoppableService) Stop(ctx context.Context) error {
	if s.slow {
		<-ctx.Done()
	}
	s.stopped <- s.name
	return s.err
}

// runManager starts mgr and returns a func that cancels it and waits for Run
// to return. Call it inside a synctest bubble.
func runManager(mgr *Manager) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		mgr.Run(ctx)
		close(done)
	}()
	return func() {
		cancel()
		<-done
	}
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestManager_RunAndStop asserts a registered service runs until the manager
// is cancelled and that Run then returns.
func TestManager_RunAndStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mgr := NewManager()
		svc := &counterService{}
		mgr.Register("counter", svc)

		stop := runManager(mgr)
		synctest.Sleep(35 * time.Millisecond)
		stop()

		if got := svc.count.Load(); got != 3 {
			t.Errorf("ticks = %d, want 3", got)
		}
	})
}

// TestManager_RestartsAfterFailure asserts a service that panics or returns an
// error is restarted after the initial backoff, and not before.
func TestManager_RestartsAfterFailure(t *testing.T) {
	for name, panics := range map[string]bool{"panic": true, "error": false} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				mgr := NewManager()
				svc := &failingService{failures: 1, panics: panics}
				mgr.Register("failing", svc)
				stop := runManager(mgr)
				defer stop()

				synctest.Sleep(defaultInitialBackoff - time.Millisecond)
				if got := svc.calls.Load(); got != 1 {
					t.Errorf("calls before the backoff = %d, want 1", got)
				}
				synctest.Sleep(time.Millisecond)
				if got := svc.calls.Load(); got != 2 {
					t.Errorf("calls after the backoff = %d, want 2", got)
				}
			})
		})
	}
}

// TestManager_BackoffDoublesBetweenRestarts asserts a service that keeps
// failing is restarted after 1s, 2s, 4s: four runs by 7s, not a fifth until 15s.
func TestManager_BackoffDoublesBetweenRestarts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mgr := NewManager()
		svc := &failingService{failures: 100, panics: true}
		mgr.Register("always-panic", svc)
		stop := runManager(mgr)
		defer stop()

		synctest.Sleep(7 * time.Second)
		if got := svc.calls.Load(); got != 4 {
			t.Errorf("runs by 7s = %d, want 4", got)
		}
		synctest.Sleep(8*time.Second - time.Millisecond)
		if got := svc.calls.Load(); got != 4 {
			t.Errorf("runs just before 15s = %d, want 4", got)
		}
	})
}

// TestManager_StopReverseOrder asserts Stop reaches every Stopper, in reverse
// registration order, skips services that are not Stoppers, and carries on
// past one whose Stop fails.
func TestManager_StopReverseOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mgr := NewManager()
		stopped := make(chan string, 3)
		mgr.Register("first", &stoppableService{stopped: stopped, name: "first"})
		mgr.Register("counter", &counterService{})
		mgr.Register("second", &stoppableService{stopped: stopped, name: "second", err: context.DeadlineExceeded})
		mgr.Register("third", &stoppableService{stopped: stopped, name: "third"})
		runManager(mgr)()

		mgr.Stop(5 * time.Second)

		if got := drain(stopped); !slices.Equal(got, []string{"third", "second", "first"}) {
			t.Errorf("stop order = %v, want [third second first]", got)
		}
	})
}

// TestManager_StopPerServiceTimeout asserts each Stopper gets its own share of
// the budget, so a service that burns its share cannot starve the next.
func TestManager_StopPerServiceTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mgr := NewManager()
		stopped := make(chan string, 2)
		mgr.Register("slow", &stoppableService{stopped: stopped, name: "slow", slow: true})
		mgr.Register("fast", &stoppableService{stopped: stopped, name: "fast"})
		runManager(mgr)()

		const budget = 2 * time.Second
		start := time.Now()
		mgr.Stop(budget)

		if elapsed := time.Since(start); elapsed != budget/2 {
			t.Errorf("Stop took %v, want %v: the slow service's share and no more", elapsed, budget/2)
		}
		if got := drain(stopped); !slices.Equal(got, []string{"fast", "slow"}) {
			t.Errorf("stopped = %v, want both", got)
		}
	})
}

// TestManager_NoServicesRunsCleanly asserts a manager with nothing registered
// returns from Run and Stop at once.
func TestManager_NoServicesRunsCleanly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mgr := NewManager()
		runManager(mgr)()
		mgr.Stop(time.Second)
	})
}

// drain returns what was buffered in ch, in order, after closing it.
func drain(ch chan string) []string {
	close(ch)
	var out []string
	for v := range ch {
		out = append(out, v)
	}
	return out
}
