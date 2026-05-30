// -------------------------------------------------------------------------------
// Lifecycle Manager Tests
//
// Author: Alex Freidah
//
// Tests for the background service lifecycle manager. Covers service registration,
// graceful shutdown propagation, and concurrent service orchestration.
// -------------------------------------------------------------------------------

package lifecycle

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/testutil/testx"
)

// -------------------------------------------------------------------------
// TEST HELPERS
// -------------------------------------------------------------------------

// counterService records the number of times Run was invoked so the
// supervisor's restart loop can be asserted across panic and clean-
// exit paths.
type counterService struct {
	count atomic.Int64
}

// Run runs .
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

// panicOnceService panics on its first Run and runs normally
// thereafter so tests can assert the supervisor recovers and
// restarts a panicking service rather than crashing the manager.
type panicOnceService struct {
	calls atomic.Int64
}

// Run runs .
func (s *panicOnceService) Run(ctx context.Context) error {
	n := s.calls.Add(1)
	if n == 1 {
		panic("boom")
	}
	<-ctx.Done()
	return nil
}

// errorOnceService returns an error on its first Run and runs
// normally thereafter so tests can assert the supervisor backs off
// and restarts on a returned error (separate path from a panic).
type errorOnceService struct {
	calls atomic.Int64
}

// Run runs .
func (s *errorOnceService) Run(ctx context.Context) error {
	n := s.calls.Add(1)
	if n == 1 {
		return context.DeadlineExceeded
	}
	<-ctx.Done()
	return nil
}

// stopErrorService is a Stopper whose Stop call returns an
// error. Lets tests assert the manager logs the error without
// blocking the rest of the shutdown sequence.
type stopErrorService struct {
	ran chan struct{}
}

// Run runs .
func (s *stopErrorService) Run(ctx context.Context) error {
	close(s.ran)
	<-ctx.Done()
	return nil
}

// Stop satisfies Stopper. The behaviour depends on the fixture:
// stopErrorService returns an error, stoppableService records the
// call, and the inline test version is a no-op.
func (s *stopErrorService) Stop(_ context.Context) error {
	return context.DeadlineExceeded
}

// stoppableService is a Stopper whose Stop call records the
// invocation so tests can assert the manager called Stop on
// shutdown.
type stoppableService struct {
	stopped chan string
	name    string
}

// Run runs .
func (s *stoppableService) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// Stop satisfies Stopper. The behaviour depends on the fixture:
// stopErrorService returns an error, stoppableService records the
// call, and the inline test version is a no-op.
func (s *stoppableService) Stop(_ context.Context) error {
	s.stopped <- s.name
	return nil
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestManager_RunAndStop verifies the manager run and stop path by exercising mgr.Register, context.WithCancel, context.Background.
func TestManager_RunAndStop(t *testing.T) {
	mgr := NewManager()
	svc := &counterService{}
	mgr.Register("counter", svc)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		mgr.Run(ctx)
		close(done)
	}()

	// Wait for at least a few ticks (each tick is 10ms)
	testx.Eventually(t, 2*time.Second, func() bool { return svc.count.Load() >= 3 },
		"service never ticked")
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Manager.Run did not return after context cancellation")
	}

	if svc.count.Load() == 0 {
		t.Error("Service never ran")
	}
}

// TestManager_PanicRecovery verifies the manager panic recovery path by exercising mgr.SetBackoff, mgr.Register, context.WithCancel.
func TestManager_PanicRecovery(t *testing.T) {
	t.Parallel()
	mgr := NewManager()
	mgr.SetBackoff(1*time.Millisecond, 10*time.Millisecond, 1*time.Hour)
	svc := &panicOnceService{}
	mgr.Register("panic-once", svc)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		mgr.Run(ctx)
		close(done)
	}()

	// Wait for panic -> restart -> second call (backoff is 1ms on first retry)
	testx.Eventually(t, 2*time.Second, func() bool { return svc.calls.Load() >= 2 },
		"service did not restart after panic")
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Manager.Run did not return after context cancellation")
	}
}

// TestManager_StopCallsStoppable verifies the manager stop calls stoppable contract.
// Asserts that Expected stop for svc-a, got.
func TestManager_StopCallsStoppable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mgr := NewManager()
		stopped := make(chan string, 1)
		svc := &stoppableService{stopped: stopped, name: "svc-a"}
		mgr.Register("svc-a", svc)

		// Also register a non-stoppable to verify it's skipped
		mgr.Register("counter", &counterService{})

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			mgr.Run(ctx)
			close(done)
		}()

		synctest.Wait()
		cancel()
		<-done

		mgr.Stop(5 * time.Second)

		select {
		case name := <-stopped:
			if name != "svc-a" {
				t.Errorf("Expected stop for svc-a, got %s", name)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Stop was never called on stoppable service")
		}
	})
}

// TestManager_StopReverseOrder verifies the manager stop reverse order contract.
// Asserts that Expected stop order , got.
func TestManager_StopReverseOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mgr := NewManager()
		var mu sync.Mutex
		var order []string
		stopped := make(chan string, 3)

		for _, name := range []string{"first", "second", "third"} {
			svc := &stoppableService{stopped: stopped, name: name}
			mgr.Register(name, svc)
		}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			mgr.Run(ctx)
			close(done)
		}()

		synctest.Wait()
		cancel()
		<-done

		// Stop collects in reverse registration order (synchronous per service)
		go func() {
			mgr.Stop(5 * time.Second)
		}()

		for range 3 {
			select {
			case name := <-stopped:
				mu.Lock()
				order = append(order, name)
				mu.Unlock()
			case <-time.After(2 * time.Second):
				t.Fatal("Timed out waiting for Stop calls")
			}
		}

		mu.Lock()
		defer mu.Unlock()
		expected := []string{"third", "second", "first"}
		for i, name := range expected {
			if i >= len(order) || order[i] != name {
				t.Errorf("Expected stop order %v, got %v", expected, order)
				break
			}
		}
	})
}

// TestManager_ErrorRestart verifies the manager error restart path by exercising mgr.SetBackoff, mgr.Register, context.WithCancel.
func TestManager_ErrorRestart(t *testing.T) {
	t.Parallel()
	mgr := NewManager()
	mgr.SetBackoff(1*time.Millisecond, 10*time.Millisecond, 1*time.Hour)
	svc := &errorOnceService{}
	mgr.Register("error-once", svc)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		mgr.Run(ctx)
		close(done)
	}()

	// Wait for error -> restart delay -> second call (backoff is 1ms on first retry)
	testx.Eventually(t, 2*time.Second, func() bool { return svc.calls.Load() >= 2 },
		"service did not restart after error")
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Manager.Run did not return after context cancellation")
	}
}

// TestManager_StopErrorDoesNotPanic verifies the manager stop error does not panic path by exercising mgr.Register, context.WithCancel, context.Background.
func TestManager_StopErrorDoesNotPanic(t *testing.T) {
	t.Parallel()
	mgr := NewManager()
	svc := &stopErrorService{ran: make(chan struct{})}
	mgr.Register("stop-err", svc)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		mgr.Run(ctx)
		close(done)
	}()

	<-svc.ran
	cancel()
	<-done

	// Stop should not panic even when Stop() returns an error
	mgr.Stop(5 * time.Second)
}

// alwaysPanicService panics on every call, used to verify backoff.
type alwaysPanicService struct {
	calls atomic.Int64
}

// Run runs .
func (s *alwaysPanicService) Run(_ context.Context) error {
	s.calls.Add(1)
	panic("always panic")
}

// TestManager_BackoffLimitsRestartRate verifies the manager backoff limits restart rate contract.
// Asserts that Expected <=5 restarts with exponential backoff, got.
func TestManager_BackoffLimitsRestartRate(t *testing.T) {
	t.Parallel()
	mgr := NewManager()
	// Scaled-down backoff so the test runs in tens of milliseconds instead
	// of seconds while exercising the same exponential schedule as prod
	// (initial, 2xinitial, 4xinitial, ...).
	mgr.SetBackoff(5*time.Millisecond, 150*time.Millisecond, 1*time.Hour)
	svc := &alwaysPanicService{}
	mgr.Register("always-panic", svc)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		mgr.Run(ctx)
		close(done)
	}()

	// With exponential backoff (5ms, 10ms, 20ms, ...) the first 25ms of the
	// window covers roughly 5+10=15ms of backoff plus a couple of instant
	// panic cycles  -  at most a handful of restarts, never the hundreds a
	// flat 5ms delay would allow.
	time.Sleep(25 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Manager.Run did not return after context cancellation")
	}

	calls := svc.calls.Load()
	if calls > 5 {
		t.Errorf("Expected <=5 restarts with exponential backoff, got %d", calls)
	}
	if calls < 2 {
		t.Errorf("Expected at least 2 restarts, got %d", calls)
	}
}

// TestManager_NoServicesRunsCleanly verifies the manager no services runs cleanly path by exercising context.WithCancel, context.Background, mgr.Run.
func TestManager_NoServicesRunsCleanly(t *testing.T) {
	t.Parallel()
	mgr := NewManager()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	done := make(chan struct{})
	go func() {
		mgr.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run with no services should return immediately")
	}

	// Stop with no services should not panic
	mgr.Stop(time.Second)
}

// slowStopService blocks in Stop until the context deadline expires, simulating
// a service that consumes its entire shutdown budget.
type slowStopService struct {
	stopped chan string
	name    string
}

// Run runs .
func (s *slowStopService) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// Stop satisfies Stopper. The behaviour depends on the fixture:
// stopErrorService returns an error, stoppableService records the
// call, and the inline test version is a no-op.
func (s *slowStopService) Stop(ctx context.Context) error {
	<-ctx.Done()
	s.stopped <- s.name
	return ctx.Err()
}

// TestManager_StopPerServiceTimeout verifies that a slow service cannot
// starve other services of their shutdown budget. With per-service timeouts,
// even if one service blocks until its deadline, the next service still gets
// a full share.
func TestManager_StopPerServiceTimeout(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		mgr := NewManager()
		stopped := make(chan string, 2)

		mgr.Register("slow", &slowStopService{stopped: stopped, name: "slow"})
		mgr.Register("fast", &stoppableService{stopped: stopped, name: "fast"})

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			mgr.Run(ctx)
			close(done)
		}()

		synctest.Wait()
		cancel()
		<-done

		// 200ms total, 2 stoppable services = 100ms each. The ratio matches
		// production (slow burns its full share, fast returns immediately)
		// without spending real wall-clock on a 1s-per-service budget.
		const totalBudget = 200 * time.Millisecond
		start := time.Now()
		mgr.Stop(totalBudget)
		elapsed := time.Since(start)

		var names []string
		for range 2 {
			select {
			case name := <-stopped:
				names = append(names, name)
			case <-time.After(500 * time.Millisecond):
				t.Fatalf("timed out waiting for stop calls, got %v", names)
			}
		}

		// Should be ~100ms (slow burns its share, fast stops instantly). Give
		// 3x headroom for slow CI.
		if elapsed > 3*totalBudget {
			t.Errorf("Stop took %v, expected ~%v (per-service budgets)", elapsed, totalBudget/2)
		}
	})
}
