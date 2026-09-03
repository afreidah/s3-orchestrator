// -------------------------------------------------------------------------------
// Probe Scheduler Tests - Rolling-Window Concurrency Mechanics
//
// Author: Alex Freidah
//
// Exercises ProbeScheduler.FirstSuccess directly with fake probes, so the
// concurrency mechanics (rolling window, first-success cut-over, loser
// cancellation, bounded parallelism) are covered without the full Broadcaster,
// cache, or telemetry stack.
// -------------------------------------------------------------------------------

package readpath

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// mkPending builds a pending-probe slice from backend names. The backend is
// left nil because the test probes key off the name and ignore it.
func mkPending(names ...string) []pendingProbe {
	p := make([]pendingProbe, len(names))
	for i, n := range names {
		p[i] = pendingProbe{name: n}
	}
	return p
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestProbeScheduler_AllFail verifies that when every probe fails the scheduler
// reports found=false and returns one error per probe for the caller to
// classify.
func TestProbeScheduler_AllFail(t *testing.T) {
	t.Parallel()
	s := &ProbeScheduler[int]{pending: mkPending("a", "b", "c"), drainTimeout: time.Second}
	probe := func(_ context.Context, name string, _ *core.ObjectLocation, _ backend.ObjectBackend) (ProbeResult[int], error) {
		return ProbeResult[int]{}, fmt.Errorf("fail %s", name)
	}

	res, errs, found := s.FirstSuccess(context.Background(), probe)
	if found {
		t.Fatalf("found = true, want false (every probe failed); res = %+v", res)
	}
	if len(errs) != 3 {
		t.Errorf("len(errs) = %d, want 3", len(errs))
	}
}

// TestProbeScheduler_SequentialWinnerAfterFailures uses a parallelism of 1 to
// force deterministic ordering: the rolling window must launch each pending
// backend in turn after the prior one fails, and surface the first success with
// the preceding failures collected.
func TestProbeScheduler_SequentialWinnerAfterFailures(t *testing.T) {
	t.Parallel()
	s := &ProbeScheduler[int]{pending: mkPending("a", "b", "c"), parallelism: 1, drainTimeout: time.Second}
	probe := func(_ context.Context, name string, _ *core.ObjectLocation, _ backend.ObjectBackend) (ProbeResult[int], error) {
		if name == "c" {
			return ProbeResult[int]{Value: 7, Size: 7, Cleanup: NoopCleanup}, nil
		}
		return ProbeResult[int]{}, errors.New(name + " failed")
	}

	res, errs, found := s.FirstSuccess(context.Background(), probe)
	if !found {
		t.Fatal("found = false, want true (c succeeds)")
	}
	if res.name != "c" || res.value != 7 {
		t.Errorf("winner = %q value = %d, want c / 7", res.name, res.value)
	}
	if len(errs) != 2 {
		t.Errorf("len(errs) = %d, want 2 (a, b failed before c)", len(errs))
	}
}

// TestProbeScheduler_WinnerCancelsLosers verifies that once a winner is
// declared the still-running losers have their contexts cancelled.
func TestProbeScheduler_WinnerCancelsLosers(t *testing.T) {
	t.Parallel()
	loserCancelled := make(chan struct{})
	s := &ProbeScheduler[int]{pending: mkPending("winner", "loser"), drainTimeout: time.Second}
	probe := func(ctx context.Context, name string, _ *core.ObjectLocation, _ backend.ObjectBackend) (ProbeResult[int], error) {
		if name == "winner" {
			return ProbeResult[int]{Value: 1, Cleanup: NoopCleanup}, nil
		}
		// Loser: block until the winner cancels this probe's context.
		<-ctx.Done()
		close(loserCancelled)
		return ProbeResult[int]{}, ctx.Err()
	}

	res, _, found := s.FirstSuccess(context.Background(), probe)
	if !found || res.name != "winner" {
		t.Fatalf("winner = %q found = %v, want winner / true", res.name, found)
	}
	select {
	case <-loserCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("loser probe was not cancelled after the winner was declared")
	}
}

// TestProbeScheduler_RespectsParallelismCap verifies the rolling window never
// runs more than `parallelism` probes at once: with a cap of 2 and 5 backends,
// only 2 probes start until one returns.
func TestProbeScheduler_RespectsParallelismCap(t *testing.T) {
	t.Parallel()
	started := make(chan string, 5)
	release := make(chan struct{})

	s := &ProbeScheduler[int]{pending: mkPending("a", "b", "c", "d", "e"), parallelism: 2, drainTimeout: time.Second}
	probe := func(_ context.Context, name string, _ *core.ObjectLocation, _ backend.ObjectBackend) (ProbeResult[int], error) {
		started <- name
		<-release // hold the slot until the test releases it
		return ProbeResult[int]{}, errors.New(name + " failed")
	}

	done := make(chan struct{})
	go func() {
		_, _, found := s.FirstSuccess(context.Background(), probe)
		if found {
			t.Errorf("found = true, want false (every probe fails)")
		}
		close(done)
	}()

	// Exactly two probes should start while the slots are held.
	<-started
	<-started
	select {
	case n := <-started:
		t.Fatalf("a third probe (%q) started while two slots were held; parallelism cap leaked", n)
	case <-time.After(50 * time.Millisecond):
		// Good: the cap held the window at two.
	}

	close(release) // let the held probes fail; the window now drains the rest
	<-done
}
