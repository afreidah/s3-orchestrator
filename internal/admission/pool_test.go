// -------------------------------------------------------------------------------
// Admission Pool Tests
//
// Author: Alex Freidah
//
// Covers who a freed slot goes to: the waiting request flow holding the
// fewest, oldest first among equals, and background work only under its half
// of the pool. Also covers the bounds on waiting: the wait limit, the queue a
// full pool refuses past, and a context that ends first.
// -------------------------------------------------------------------------------

package admission

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// longWait is long enough that no test here sees it run out unless it means
// to.
const longWait = time.Minute

// result is what a waiting Acquire came back with.
type result struct {
	slot Slot
	err  error
}

// recorder counts the client requests a pool reports waiting and keeps the
// waits it times.
type recorder struct {
	mu     sync.Mutex
	queued int
	waits  []float64
}

// Inc counts one more request waiting.
func (r *recorder) Inc() { r.mu.Lock(); r.queued++; r.mu.Unlock() }

// Dec counts one fewer request waiting.
func (r *recorder) Dec() { r.mu.Lock(); r.queued--; r.mu.Unlock() }

// Observe keeps one wait.
func (r *recorder) Observe(s float64) { r.mu.Lock(); r.waits = append(r.waits, s); r.mu.Unlock() }

// newPool builds a pool of capacity slots reporting to a recorder.
func newPool(capacity int) (*Pool, *recorder) {
	r := &recorder{}
	return New("test", capacity, Metrics{Queued: r, Waited: r}), r
}

// hold takes n slots in the request flow name, failing the test if any is
// refused.
func hold(t *testing.T, p *Pool, name string, n int) []Slot {
	t.Helper()
	slots := make([]Slot, n)
	for i := range slots {
		slot, err := p.Acquire(t.Context(), name, longWait)
		if err != nil {
			t.Fatalf("hold %s: %v", name, err)
		}
		slots[i] = slot
	}
	return slots
}

// queue starts an Acquire in the request flow name and waits until it is
// parked, so the order requests queue in is the order a test starts them.
func queue(ctx context.Context, p *Pool, name string) <-chan result {
	return parked(func() (Slot, error) { return p.Acquire(ctx, name, longWait) })
}

// queueBackground is queue for background work.
func queueBackground(ctx context.Context, p *Pool) <-chan result {
	return parked(func() (Slot, error) { return p.AcquireBackground(ctx) })
}

// parked runs acquire on its own goroutine and returns once every goroutine in
// the bubble is blocked, with the channel its result arrives on.
func parked(acquire func() (Slot, error)) <-chan result {
	done := make(chan result, 1)
	go func() {
		slot, err := acquire()
		done <- result{slot, err}
	}()
	synctest.Wait()
	return done
}

// admitted reports whether the request behind done has been given a slot,
// failing the test if it was refused instead. The slot stays held, so it is
// not handed on while the test checks the next request.
func admitted(t *testing.T, done <-chan result) bool {
	t.Helper()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("refused: %v", r.err)
		}
		return true
	default:
		return false
	}
}

// TestPool_FreeSlotIsTakenAtOnce verifies a pool with room admits without
// queueing or timing anything.
func TestPool_FreeSlotIsTakenAtOnce(t *testing.T) {
	t.Parallel()
	p, rec := newPool(2)
	hold(t, p, "a", 2)
	if p.inFlight != 2 || len(rec.waits) != 0 || rec.queued != 0 {
		t.Errorf("in flight %d, waits %v, queued %d; want two slots taken at once", p.inFlight, rec.waits, rec.queued)
	}
}

// TestPool_FreedSlotGoesToTheRequestFlowHoldingFewest verifies a request flow
// holding every slot does not get the next one ahead of one holding none, even
// though its request waited first.
func TestPool_FreedSlotGoesToTheRequestFlowHoldingFewest(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		p, rec := newPool(2)
		busy := hold(t, p, "busy", 2)
		busyWaiting := queue(t.Context(), p, "busy")
		quietWaiting := queue(t.Context(), p, "quiet")
		if rec.queued != 2 {
			t.Errorf("queued = %d, want 2 waiting", rec.queued)
		}

		busy[0].Release()
		synctest.Wait()
		if !admitted(t, quietWaiting) || admitted(t, busyWaiting) {
			t.Fatal("the freed slot did not go to the request flow holding none")
		}
		busy[1].Release()
		synctest.Wait()
		if !admitted(t, busyWaiting) {
			t.Error("the next freed slot did not reach the busy request flow")
		}
		if rec.queued != 0 || len(rec.waits) != 2 {
			t.Errorf("queued %d, waits %v; want none waiting and both waits timed", rec.queued, rec.waits)
		}
	})
}

// TestPool_EqualRequestFlowsAreServedOldestFirst verifies that between request
// flows holding as many, the one whose request waited longest is served.
func TestPool_EqualRequestFlowsAreServedOldestFirst(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		p, _ := newPool(2)
		held := hold(t, p, "held", 2)
		first := queue(t.Context(), p, "first")
		second := queue(t.Context(), p, "second")

		held[0].Release()
		synctest.Wait()
		if !admitted(t, first) || admitted(t, second) {
			t.Error("the freed slot did not go to the request that waited first")
		}
	})
}

// TestPool_WaitExpires verifies a request refused once its wait runs out has
// its wait timed, leaves the queue, and its idle request flow is forgotten.
func TestPool_WaitExpires(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		p, rec := newPool(1)
		hold(t, p, "held", 1)
		if _, err := p.Acquire(t.Context(), "late", 10*time.Millisecond); !errors.Is(err, ErrWaitExpired) {
			t.Errorf("Acquire = %v, want ErrWaitExpired", err)
		}
		if rec.queued != 0 || len(rec.waits) != 1 || rec.waits[0] != 0.01 {
			t.Errorf("queued %d, waits %v; want none waiting and one 10ms wait", rec.queued, rec.waits)
		}
		if _, ok := p.requestFlows["late"]; ok {
			t.Error("the refused request flow was not forgotten")
		}
	})
}

// TestPool_FullQueueRefusesTheLongestRequestFlowsNewest verifies a request
// arriving at full queues pushes out the newest request of the request flow
// with the most waiting, and is refused itself when its own has as many.
func TestPool_FullQueueRefusesTheLongestRequestFlowsNewest(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		p, _ := newPool(2)
		held := hold(t, p, "held", 2)
		older := queue(t.Context(), p, "bulk")
		newer := queue(t.Context(), p, "bulk")

		quiet := queue(t.Context(), p, "quiet")
		if r := <-newer; !errors.Is(r.err, ErrQueueFull) {
			t.Errorf("the longest request flow's newest = %v, want ErrQueueFull", r.err)
		}
		if _, err := p.Acquire(t.Context(), "bulk", longWait); !errors.Is(err, ErrQueueFull) {
			t.Errorf("an arrival whose request flow is as long as any = %v, want ErrQueueFull", err)
		}

		held[0].Release()
		held[1].Release()
		synctest.Wait()
		if !admitted(t, older) || !admitted(t, quiet) {
			t.Error("the requests left in the queue were not admitted")
		}
	})
}

// TestPool_CancelledWaitLeavesTheQueue verifies a request whose context ends
// while it waits is refused with the context's error and leaves nothing behind.
func TestPool_CancelledWaitLeavesTheQueue(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		p, _ := newPool(1)
		held := hold(t, p, "held", 1)
		ctx, cancel := context.WithCancel(t.Context())
		done := queue(ctx, p, "gone")
		cancel()
		if r := <-done; !errors.Is(r.err, context.Canceled) {
			t.Errorf("cancelled wait = %v, want context.Canceled", r.err)
		}

		held[0].Release()
		if p.inFlight != 0 || len(p.requestFlows) != 0 {
			t.Errorf("in flight %d, request flows %d; want an idle pool", p.inFlight, len(p.requestFlows))
		}
	})
}

// TestPool_SlotGivenAsTheWaitEnds verifies a slot handed to a request in the
// moment its wait ends is kept when the wait ran out, and passed on when its
// context ended.
func TestPool_SlotGivenAsTheWaitEnds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		cause    error
		inFlight int
	}{
		{"wait expired keeps it", ErrWaitExpired, 1},
		{"context ended passes it on", context.Canceled, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, _ := newPool(1)
			held := hold(t, p, "held", 1)
			p.mu.Lock()
			w, _ := p.enqueue(p.requestFlowOf("late"))
			p.mu.Unlock()
			held[0].Release()

			err := p.abandon(w, tc.cause)
			if tc.inFlight == 1 && err != nil {
				t.Errorf("abandon = %v, want the slot kept", err)
			}
			if tc.inFlight == 0 && !errors.Is(err, tc.cause) {
				t.Errorf("abandon = %v, want %v", err, tc.cause)
			}
			if p.inFlight != tc.inFlight {
				t.Errorf("in flight = %d, want %d", p.inFlight, tc.inFlight)
			}
		})
	}
}

// TestPool_BackgroundHoldsAtMostHalf verifies background work takes no more
// than half the pool while clients find the rest free, and gets its next slot
// when one of its own frees.
func TestPool_BackgroundHoldsAtMostHalf(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		p, rec := newPool(4)
		first, err := p.AcquireBackground(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.AcquireBackground(t.Context()); err != nil {
			t.Fatal(err)
		}
		third := queueBackground(t.Context(), p)
		if admitted(t, third) {
			t.Fatal("background work took more than half the pool")
		}
		hold(t, p, "client", 2)

		first.Release()
		synctest.Wait()
		if !admitted(t, third) {
			t.Error("background work did not get the slot of its own that freed")
		}
		if rec.queued != 0 || len(rec.waits) != 0 {
			t.Errorf("queued %d, waits %v; want background work kept out of the client metrics", rec.queued, rec.waits)
		}
	})
}

// TestPool_BackgroundTakesItsTurnOnABusyPool verifies background work holding
// nothing gets a freed slot ahead of a client request flow holding the pool.
func TestPool_BackgroundTakesItsTurnOnABusyPool(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		p, _ := newPool(2)
		busy := hold(t, p, "busy", 2)
		client := queue(t.Context(), p, "busy")
		background := queueBackground(t.Context(), p)

		busy[0].Release()
		synctest.Wait()
		if !admitted(t, background) || admitted(t, client) {
			t.Fatal("the freed slot did not go to background work holding none")
		}
		busy[1].Release()
		synctest.Wait()
		if !admitted(t, client) {
			t.Error("the next freed slot did not reach the client")
		}
	})
}

// TestPool_BackgroundCancelledWait verifies background work whose context ends
// while it waits is refused with the context's error and leaves the queue.
func TestPool_BackgroundCancelledWait(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		p, _ := newPool(1)
		hold(t, p, "held", 1)
		ctx, cancel := context.WithCancel(t.Context())
		done := queueBackground(ctx, p)
		cancel()
		if r := <-done; !errors.Is(r.err, context.Canceled) {
			t.Errorf("cancelled wait = %v, want context.Canceled", r.err)
		}
		if len(p.background.queue) != 0 {
			t.Error("the cancelled background request was left in the queue")
		}
	})
}

// TestPool_NilAdmitsEverything verifies a nil pool admits clients and
// background work at once, and its zero Slot releases as nothing.
func TestPool_NilAdmitsEverything(t *testing.T) {
	t.Parallel()
	var p *Pool
	slot, err := p.Acquire(t.Context(), "a", longWait)
	if err != nil {
		t.Errorf("Acquire = %v, want a slot", err)
	}
	slot.Release()
	bg, err := p.AcquireBackground(t.Context())
	if err != nil {
		t.Errorf("AcquireBackground = %v, want a slot", err)
	}
	bg.Release()
}

// TestPool_IdleRequestFlowIsReused verifies a request flow given back its last
// slot is forgotten and kept as the spare the next one starts from.
func TestPool_IdleRequestFlowIsReused(t *testing.T) {
	t.Parallel()
	p, _ := newPool(2)
	held := hold(t, p, "a", 1)
	spare := held[0].requestFlow
	held[0].Release()
	if len(p.requestFlows) != 0 || p.spare != spare {
		t.Fatal("the idle request flow was not forgotten into the spare")
	}
	next := hold(t, p, "b", 1)
	if next[0].requestFlow != spare || spare.name != "b" || p.spare != nil {
		t.Error("the next request flow did not start from the spare")
	}
	if p.Name() != "test" {
		t.Errorf("Name = %q, want %q", p.Name(), "test")
	}
}
