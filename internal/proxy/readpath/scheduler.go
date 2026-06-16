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
)

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
	parallelism  int           // 0 means no cap: fan out to every backend at once
	drainTimeout time.Duration // bound on the post-winner loser-drain
	operation    string        // label for the drain-timeout metric
}

// FirstSuccess launches the rolling window and returns the first successful
// probe. found is false when every probe failed; errs holds each failure in
// arrival order so the caller can classify the outcome. With no cap the first
// call launches every backend at once (historical behaviour); with a positive
// cap the first cap probes launch immediately and each failure replenishes the
// next pending backend, so at most cap goroutines are ever in flight.
//
// On success the losing probes' contexts are cancelled so their in-flight
// backend round trips, decryption, and integrity work stop promptly, and any
// already-launched results are drained and cleaned up in the background. The
// winning result is returned untouched: its Value owns its own lifecycle (a GET
// body keeps the cancel; a HEAD has already released its timeout), so the
// winner's cleanup is never invoked.
func (s *ProbeScheduler[T]) FirstSuccess(ctx context.Context, probe Probe[T]) (res broadcastResult[T], errs []error, found bool) {
	initial := broadcastSlotCount(s.parallelism, len(s.pending))
	ch := make(chan broadcastResult[T], len(s.pending))
	cancels := make(map[string]context.CancelFunc, initial)

	// launched tracks how many probes have been started so far. The receive
	// loop calls launchNext after each failure to refill a free slot; on
	// success the remaining pending entries are never launched.
	launched := 0
	launchNext := func() bool {
		if launched >= len(s.pending) {
			return false
		}
		p := s.pending[launched]
		launched++
		probeCtx, cancel := context.WithCancel(ctx) //nolint:gosec // G118: cancel reaches the call graph via the cancels map -> cancelLosers / all-failed loop.
		cancels[p.name] = cancel
		go runBackendProbe(probeCtx, p.name, p.be, probe, ch)
		return true
	}
	for range initial {
		launchNext()
	}

	received := 0
	for received < launched {
		r := <-ch
		received++
		if r.err != nil {
			errs = append(errs, r.err)
			launchNext() // backfill the slot; harmless when no pending probes remain
			continue
		}
		// Winner declared. Cancel every in-flight loser; backends that were
		// pending but never launched need no cancellation. Drain whatever the
		// losers already produced in the background so their cleanup runs.
		cancelLosers(cancels, r.name)
		if remaining := launched - received; remaining > 0 {
			go drainAndCleanupLosers(ctx, s.operation, ch, remaining, s.drainTimeout)
		}
		return r, errs, true
	}
	// All probes failed: no body to preserve, cancel every per-probe context so
	// any straggler in a retry/backoff returns immediately.
	for _, cancel := range cancels {
		cancel()
	}
	return broadcastResult[T]{}, errs, false
}

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

// cancelLosers invokes every cancel func except the winner's. The winner's
// context must stay alive because its response body is bound to it.
func cancelLosers(cancels map[string]context.CancelFunc, winner string) {
	for name, cancel := range cancels {
		if name != winner {
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

// drainAndCleanupLosers reads the remaining results from ch after a winner has
// been declared and invokes any cleanup the losers returned. It is bounded by
// timeout (and the request ctx) so a hung backend that never returns after
// cancellation cannot strand this goroutine indefinitely; a timeout is counted
// so the leak is observable rather than silent.
func drainAndCleanupLosers[T any](ctx context.Context, operation string, ch <-chan broadcastResult[T], remaining int, timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for range remaining {
		select {
		case lr := <-ch:
			if lr.cleanup != nil {
				lr.cleanup()
			}
		case <-ctx.Done():
			return
		case <-timer.C:
			telemetry.DegradedBroadcastDrainTimeoutTotal.WithLabelValues(operation).Inc()
			return
		}
	}
}
