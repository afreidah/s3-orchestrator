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
	"time"
)

// -------------------------------------------------------------------------
// TEST HELPERS
// -------------------------------------------------------------------------

type counterService struct {
	count atomic.Int64
}

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

type panicOnceService struct {
	calls atomic.Int64
}

func (s *panicOnceService) Run(ctx context.Context) error {
	n := s.calls.Add(1)
	if n == 1 {
		panic("boom")
	}
	<-ctx.Done()
	return nil
}

type stoppableService struct {
	stopped chan string
	name    string
}

func (s *stoppableService) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (s *stoppableService) Stop(_ context.Context) error {
	s.stopped <- s.name
	return nil
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

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

	// Let it tick a few times
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Manager.Run did not return after context cancellation")
	}

	if n := svc.count.Load(); n == 0 {
		t.Error("Service never ran")
	}
}

func TestManager_PanicRecovery(t *testing.T) {
	mgr := NewManager()
	svc := &panicOnceService{}
	mgr.Register("panic-once", svc)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		mgr.Run(ctx)
		close(done)
	}()

	// Wait long enough for the panic + restart + second call
	time.Sleep(2 * time.Second)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Manager.Run did not return after context cancellation")
	}

	if n := svc.calls.Load(); n < 2 {
		t.Errorf("Expected at least 2 calls (panic + restart), got %d", n)
	}
}

func TestManager_StopCallsStoppable(t *testing.T) {
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

	time.Sleep(50 * time.Millisecond)
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
}

func TestManager_StopReverseOrder(t *testing.T) {
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

	time.Sleep(50 * time.Millisecond)
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
}
