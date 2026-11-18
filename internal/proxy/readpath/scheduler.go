// -------------------------------------------------------------------------------
// Degraded-Mode Probe Scheduler - Bounded Rolling-Window Fan-Out
//
// Author: Alex Freidah
//
// ProbeScheduler owns the concurrency mechanics of a degraded-mode broadcast:
// launching a bounded rolling window of per-backend probes, returning the first
// success, cancelling the losers, and draining their results. Keeping these
// mechanics here lets the Broadcaster stay focused on degraded-read policy -
// spans, metrics, the location cache, accounting, and winner recording.
// -------------------------------------------------------------------------------

package readpath

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// pendingProbe is one eligible backend awaiting (or running) its probe.
// Held in a flat slice so the rolling-window launcher can pick the next
// pending entry deterministically as slots free up.
type pendingProbe struct {
	name string
	be   backend.ObjectBackend
}

// ProbeScheduler runs a bounded rolling window of degraded-mode probes and
// returns the first success. It is built per broadcast (cheap, single-use) and
// holds only the mechanics inputs; policy, telemetry, caching, and accounting
// stay with the Broadcaster.
type ProbeScheduler[T any] struct {
	pending      []pendingProbe
	parallelism  int            // 0 means no cap: fan out to every backend at once
	drainTimeout time.Duration  // bound on the post-winner loser-drain
	operation    s3op.Operation // label for the drain-timeout metric
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// FirstSuccess launches the rolling window and returns the first successful
// probe. found is false when every probe failed; errs holds each failure in
// arrival order. With no cap every backend launches at once; with a positive
// cap at most cap probes run at a time, and each failure launches the next.
//
// On success the losing probes' contexts are cancelled, and results they
// already produced are drained and cleaned up in the background. The winner's
// cleanup is never invoked, because its Value owns its own lifecycle. If ctx
// ends first, every probe is cancelled and found is false.
func (s *ProbeScheduler[T]) FirstSuccess(ctx context.Context, probe Probe[T]) (res broadcastResult[T], errs []error, found bool) {
	initial := broadcastSlotCount(s.parallelism, len(s.pending))
	ch := make(chan broadcastResult[T], len(s.pending))
	cancels := make(map[string]context.CancelFunc, initial)

	// launched tracks how many probes have been started so far. The receive
	// loop calls launchNext after each failure to refill a free slot; on
	// success the remaining pending entries are never launched.
	launched := 0
	launchNext := func() {
		if launched >= len(s.pending) {
			return
		}
		p := s.pending[launched]
		launched++
		probeCtx, cancel := context.WithCancel(ctx) //nolint:gosec // G118: cancel reaches the call graph via the cancels map -> cancelProbes.
		cancels[p.name] = cancel
		go runBackendProbe(probeCtx, p.name, p.be, probe, ch)
	}
	for range initial {
		launchNext()
	}

	received := 0
	for received < launched {
		r, err := syncutil.Receive(ctx, ch)
		if err != nil {
			// The request ended: cancel every probe and drain the in-flight ones.
			cancelProbes(cancels, "")
			go drainAndCleanupLosers(s.operation, ch, launched-received, s.drainTimeout)
			return broadcastResult[T]{}, errs, false
		}
		received++
		if r.err != nil {
			errs = append(errs, r.err)
			launchNext() // backfill the slot; harmless when no pending probes remain
			continue
		}
		// Winner declared. Cancel every in-flight loser; backends that were
		// pending but never launched need no cancellation. Drain whatever the
		// losers already produced in the background so their cleanup runs.
		cancelProbes(cancels, r.name)
		if remaining := launched - received; remaining > 0 {
			go drainAndCleanupLosers(s.operation, ch, remaining, s.drainTimeout)
		}
		return r, errs, true
	}
	// All probes failed: no body to preserve, cancel every per-probe context so
	// any straggler in a retry/backoff returns immediately.
	cancelProbes(cancels, "")
	return broadcastResult[T]{}, errs, false
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// broadcastSlotCount returns the initial in-flight slot count for the rolling
// window: the configured cap clamped to the eligible-backend count, with
// limit <= 0 meaning "no cap" (fan out to every backend at once, preserving the
// historical behaviour).
func broadcastSlotCount(limit, eligible int) int {
	if limit <= 0 || limit > eligible {
		return eligible
	}
	return limit
}

// cancelProbes cancels every probe's context except keep's, or every one when
// keep is empty. A winner's context must stay alive because its response body
// is bound to it.
func cancelProbes(cancels map[string]context.CancelFunc, keep string) {
	for name, cancel := range cancels {
		if name != keep {
			cancel()
		}
	}
}

// runBackendProbe is the per-backend goroutine body. It forwards the probe's
// result and cleanup so the fan-in loop can surface the winner's value and the
// loser-drain can release every other result promptly instead of waiting for
// each deadline to fire on its own.
func runBackendProbe[T any](
	ctx context.Context,
	name string,
	be backend.ObjectBackend,
	probe Probe[T],
	ch chan<- broadcastResult[T],
) {
	// Degraded mode: no DB row available, probe must handle nil loc.
	res, err := probe(ctx, name, nil, be)
	if err != nil {
		ch <- broadcastResult[T]{name: name, err: err}
		return
	}
	ch <- broadcastResult[T]{name: name, value: res.Value, size: res.Size, cleanup: res.Cleanup}
}

// drainAndCleanupLosers reads the remaining results from ch and invokes any
// cleanup they returned. It is bounded by timeout alone, so a body that arrives
// after the client left is still closed; a timeout is counted so the leak is
// observable.
func drainAndCleanupLosers[T any](operation s3op.Operation, ch <-chan broadcastResult[T], remaining int, timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for range remaining {
		select {
		case lr := <-ch:
			if lr.cleanup != nil {
				lr.cleanup()
			}
		case <-timer.C:
			telemetry.DegradedBroadcastDrainTimeoutTotal.WithLabelValues(operation.String()).Inc()
			return
		}
	}
}
