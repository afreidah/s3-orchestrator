// -------------------------------------------------------------------------------
// Degraded-Mode Broadcast - DB-Unavailable Read Fan-Out
//
// Author: Alex Freidah
//
// When the metadata store is unreachable the read path cannot resolve which
// backend holds a key, so the Broadcaster fans the probe out across every
// configured backend (sequentially or in a bounded parallel window),
// returns the first success, cancels the losing probes, and remembers the
// winner in the location cache for the next degraded read. Failover.Read
// owns the policy decision to enter this path; the Broadcaster owns the
// mechanism.
// -------------------------------------------------------------------------------

package readpath

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// Broadcaster fans a degraded-mode read out across every configured backend
// and returns the first success. It holds no metadata-store dependency -
// degraded mode exists precisely because the store is unreachable - and
// caches the winning backend for subsequent degraded reads.
type Broadcaster struct {
	core         ReadRuntime
	cache        LocationCache
	parallel     bool          // fan out to every backend at once vs. one at a time
	parallelism  int           // cap on concurrent probes when parallel; 0 means uncapped
	drainTimeout time.Duration // bound on the loser-drain so a hung probe can't strand the goroutine
}

// defaultDrainTimeout bounds the loser-drain when no backend timeout is
// configured; it mirrors the config BackendTimeout default and guards
// zero-value construction (notably in tests).
const defaultDrainTimeout = 30 * time.Second

// drainTimeoutOrDefault returns the configured backend timeout, or
// defaultDrainTimeout when it is unset, so the drain bound is never zero.
func drainTimeoutOrDefault(backendTimeout time.Duration) time.Duration {
	if backendTimeout <= 0 {
		return defaultDrainTimeout
	}
	return backendTimeout
}

// Read tries all backends when the DB is unavailable. Checks the location
// cache first for a known-good backend, then dispatches to either parallel
// or sequential broadcast based on configuration.
func broadcastRead[T any](ctx context.Context, b *Broadcaster, op readOp, probe Probe[T]) (value T, winner string, retErr error) {
	bcStart := time.Now()
	cacheHit := false
	defer func() {
		telemetry.DegradedBroadcastDuration.WithLabelValues(op.operation, broadcastOutcome(cacheHit, retErr)).
			Observe(time.Since(bcStart).Seconds())
	}()

	// --- Check location cache first ---
	if cachedBackend, ok := b.cache.Get(op.key); ok {
		if be, exists := b.core.Backends()[cachedBackend]; exists {
			// Degraded mode: no DB row available, probe must handle nil loc.
			res, err := probe(ctx, cachedBackend, nil, be)
			if err == nil {
				// Cache hit is the sole winner; its Value owns its lifecycle.
				b.core.Acct().Operation(op.operation, cachedBackend, op.start, nil)
				op.span.SetAttributes(telemetry.AttrCacheHit.Bool(true))
				op.span.SetAttributes(telemetry.AttrObjectSize.Int64(res.Size))
				op.span.SetStatus(codes.Ok, "")
				telemetry.DegradedCacheHitsTotal.Inc()
				cacheHit = true
				return res.Value, cachedBackend, nil
			}
			// Cache hit but backend failed - fall through to broadcast.
			// The probe already released its timeout on the error path.
		}
	}

	concurrency := 1
	if b.parallel {
		concurrency = len(b.core.BackendOrder())
	}
	return tryAllBackends(ctx, b, op, concurrency, probe)
}

// broadcastOutcome classifies the terminal state of a degraded broadcast
// for the DegradedBroadcastDuration histogram label. cache_hit and
// success are both wins; not_found is "all backends agreed the key is
// missing"; error covers any other failure (provider divergence,
// network, usage limits).
func broadcastOutcome(cacheHit bool, err error) string {
	switch {
	case cacheHit:
		return "cache_hit"
	case err == nil:
		return "success"
	case errors.Is(err, core.ErrObjectNotFound):
		return "not_found"
	default:
		return "error"
	}
}

// tryAllBackends dispatches to the sequential or parallel branch based
// on concurrency. Both branches return the first backend whose probe
// succeeds and cache the winner's name for future degraded reads.
func tryAllBackends[T any](ctx context.Context, b *Broadcaster, op readOp, concurrency int, probe Probe[T]) (T, string, error) {
	if concurrency <= 1 {
		return tryBackendsSequentially(ctx, b, op, probe)
	}
	return tryBackendsInParallel(ctx, b, op, probe)
}

// tryBackendsSequentially walks BackendOrder one backend at a time. The
// first success short-circuits and is recorded as the broadcast winner;
// otherwise the last error (if any) is wrapped as a degraded-read failure.
func tryBackendsSequentially[T any](ctx context.Context, b *Broadcaster, op readOp, probe Probe[T]) (T, string, error) {
	var lastErr error
	var tally broadcastErrTally
	for _, name := range b.core.BackendOrder() {
		be, ok := b.core.Backends()[name]
		if !ok {
			continue
		}
		// Degraded mode: no DB row available, probe must handle nil loc.
		res, err := probe(ctx, name, nil, be)
		if err != nil {
			lastErr = err
			tally.add(err)
			continue
		}
		// First success wins; no losers to clean up. Value owns its lifecycle.
		b.recordBroadcastWinner(op, name, res.Size, false)
		return res.Value, name, nil
	}
	tally.recordMixedOutcomes(op.operation)
	return broadcastAllFailed[T](op.span, lastErr)
}

// broadcastErrTally tracks the 404-vs-other distribution of probe
// errors so the all-failed terminal can flag provider-divergence storms
// hidden under not_found.
type broadcastErrTally struct {
	notFound int
	other    int
}

func (t *broadcastErrTally) add(err error) {
	if backend.IsNotFound(err) {
		t.notFound++
	} else {
		t.other++
	}
}

func (t *broadcastErrTally) recordMixedOutcomes(operation string) {
	if t.notFound > 0 && t.other > 0 {
		telemetry.DegradedBroadcastMixedOutcomesTotal.WithLabelValues(operation).Inc()
	}
}

// broadcastResult carries one parallel-probe outcome back to the fan-in loop.
// value is the probe's result, surfaced only for the winner. cleanup releases
// a losing result's resources via the loser-drain goroutine; the winner's
// cleanup is never invoked because its value owns its own lifecycle.
type broadcastResult[T any] struct {
	name    string
	value   T
	size    int64
	err     error
	cleanup func()
}

// pendingProbe is one eligible backend awaiting (or running) its probe.
// Held in a flat slice so the rolling-window launcher can pick the next
// pending entry deterministically as slots free up.
type pendingProbe struct {
	name string
	be   backend.ObjectBackend
}

// tryBackendsInParallel launches probes for eligible backends in
// BackendOrder, capped to parallelism, and returns the first success,
// cancelling the losing probes' contexts so their in-flight backend round
// trips, decryption, and integrity work stop promptly instead of running
// to completion only to have their results discarded. With no cap the
// first call launches every backend at once (historical behaviour). With
// a positive cap, the first cap probes launch immediately and each failure
// replenishes the next pending backend so at most cap goroutines are ever
// in flight.
func tryBackendsInParallel[T any](ctx context.Context, b *Broadcaster, op readOp, probe Probe[T]) (T, string, error) {
	pending := b.eligibleBackends()
	if len(pending) == 0 {
		return broadcastAllFailed[T](op.span, nil)
	}

	initial := broadcastSlotCount(b.parallelism, len(pending))
	ch := make(chan broadcastResult[T], len(pending))
	cancels := make(map[string]context.CancelFunc, initial)

	// launched tracks how many probes have been started so far. The
	// receive loop calls launchNext after each failure to refill a
	// free slot; on success the remaining pending entries are never
	// launched because the winner cancels the in-flight set.
	launched := 0
	launchNext := func() bool {
		if launched >= len(pending) {
			return false
		}
		p := pending[launched]
		launched++
		probeCtx, cancel := context.WithCancel(ctx) //nolint:gosec // G118: cancel reaches the call graph via cancels map -> cancelLosers / all-failed loop.
		cancels[p.name] = cancel
		go runBackendProbe(probeCtx, p.name, p.be, probe, ch)
		return true
	}
	for range initial {
		launchNext()
	}

	var lastErr error
	var tally broadcastErrTally
	received := 0
	for received < launched {
		r := <-ch
		received++
		if r.err != nil {
			lastErr = r.err
			tally.add(r.err)
			launchNext() // backfill the slot; harmless when no pending probes remain
			continue
		}
		// Winner declared: cancel every in-flight probe so losing backends
		// stop wasting CPU, network, and API quota on work that will be
		// discarded. Backends that were pending but never launched do not
		// need cancellation - no goroutine exists yet. The winner's context
		// is intentionally left alive: its Value owns the streaming body and
		// the caller is still reading from it; the per-probe ctx is reaped
		// when the parent request ctx ends. The winner's cleanup is likewise
		// never invoked - that would release the very result we return.
		cancelLosers(cancels, r.name)
		if remaining := launched - received; remaining > 0 {
			go drainAndCleanupLosers(ctx, op.operation, ch, remaining, b.drainTimeout)
		}
		b.recordBroadcastWinner(op, r.name, r.size, true)
		return r.value, r.name, nil
	}
	// All probes failed: no body to preserve, cancel every per-probe
	// context so any straggler in a retry/backoff returns immediately.
	for _, cancel := range cancels {
		cancel()
	}
	tally.recordMixedOutcomes(op.operation)
	return broadcastAllFailed[T](op.span, lastErr)
}

// eligibleBackends returns the pending probes in BackendOrder, skipping
// any name absent from the backend map. The slice is the rolling-window
// launcher's source of truth: launched indexes into it; on success the
// remaining tail is simply never started.
func (b *Broadcaster) eligibleBackends() []pendingProbe {
	order := b.core.BackendOrder()
	backends := b.core.Backends()
	pending := make([]pendingProbe, 0, len(order))
	for _, name := range order {
		if be, ok := backends[name]; ok {
			pending = append(pending, pendingProbe{name: name, be: be})
		}
	}
	return pending
}

// broadcastSlotCount returns the initial in-flight slot count for the
// rolling window: the configured cap clamped to the eligible-backend
// count, with limit <= 0 meaning "no cap" (fan out to every backend at
// once, preserving the historical behaviour).
func broadcastSlotCount(limit, eligible int) int {
	if limit <= 0 || limit > eligible {
		return eligible
	}
	return limit
}

// cancelLosers invokes every cancel func except the winner's. The
// winner's context must stay alive because its response body is bound
// to it.
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

// drainAndCleanupLosers reads the remaining results from ch after a winner
// has been declared and invokes any cleanup the losers returned. It is bounded
// by timeout (and the request ctx) so a hung backend that never returns after
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

// recordBroadcastWinner caches the winner's name for future degraded
// reads, emits operation metrics, and sets the success-path span
// attributes.
func (b *Broadcaster) recordBroadcastWinner(op readOp, name string, size int64, parallel bool) {
	b.cache.Set(op.key, name)
	b.core.Acct().Operation(op.operation, name, op.start, nil)
	op.span.SetAttributes(telemetry.AttrBackendName.String(name))
	op.span.SetAttributes(telemetry.AttrObjectSize.Int64(size))
	if parallel {
		op.span.SetAttributes(telemetry.AttrParallelBroadcast.Bool(true))
	}
	op.span.SetStatus(codes.Ok, "")
}

// broadcastAllFailed builds the all-failed return value. When at least
// one backend returned an error, that error is wrapped so the server
// can distinguish "backend unreachable" (502) from "object not found"
// (404).
func broadcastAllFailed[T any](span trace.Span, lastErr error) (T, string, error) {
	var zero T
	if lastErr != nil {
		observe.RecordSpanError(span, lastErr)
		return zero, "", fmt.Errorf("all backends failed during degraded read: %w", lastErr)
	}
	observe.MarkSpanError(span, "no backends available")
	return zero, "", core.ErrObjectNotFound
}
