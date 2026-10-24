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
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

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

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// drainTimeoutOrDefault returns the configured backend timeout, or
// defaultDrainTimeout when it is unset, so the drain bound is never zero.
func drainTimeoutOrDefault(backendTimeout time.Duration) time.Duration {
	if backendTimeout <= 0 {
		return defaultDrainTimeout
	}
	return backendTimeout
}

// broadcastRead tries all backends when the DB is unavailable. It checks the
// location cache for a known-good backend first, then probes every backend.
func (b *Broadcaster) broadcastRead[T any](ctx context.Context, op readOp, probe Probe[T]) (value T, winner string, retErr error) {
	bcStart := time.Now()
	cacheHit := false
	defer func() {
		telemetry.DegradedBroadcastDuration.WithLabelValues(op.operation.String(), broadcastOutcome(cacheHit, retErr)).
			Observe(time.Since(bcStart).Seconds())
	}()

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

	return b.tryAllBackends(ctx, op, probe)
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

func (t *broadcastErrTally) recordMixedOutcomes(operation s3op.Operation) {
	if t.notFound > 0 && t.other > 0 {
		telemetry.DegradedBroadcastMixedOutcomesTotal.WithLabelValues(operation.String()).Inc()
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

// tryAllBackends runs every configured backend through ProbeScheduler, one at a
// time unless parallel broadcast is on, then applies degraded-read policy to the
// outcome: record the winner, or classify the failures (404 vs other) and wrap
// the all-failed terminal. The concurrency mechanics live in the scheduler.
func (b *Broadcaster) tryAllBackends[T any](ctx context.Context, op readOp, probe Probe[T]) (T, string, error) {
	parallelism := 1
	if b.parallel {
		parallelism = b.parallelism
	}
	sched := &ProbeScheduler[T]{
		pending:      b.eligibleBackends(),
		parallelism:  parallelism,
		drainTimeout: b.drainTimeout,
		operation:    op.operation,
	}
	res, errs, found := sched.FirstSuccess(ctx, probe)
	if found {
		b.recordBroadcastWinner(op, res.name, res.size)
		return res.value, res.name, nil
	}

	var tally broadcastErrTally
	for _, e := range errs {
		tally.add(e)
	}
	tally.recordMixedOutcomes(op.operation)
	var zero T
	return zero, "", broadcastAllFailed(op.span, errs)
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

// recordBroadcastWinner caches the winner's name for future degraded
// reads, emits operation metrics, and sets the success-path span
// attributes.
func (b *Broadcaster) recordBroadcastWinner(op readOp, name string, size int64) {
	b.cache.Set(op.key, name)
	b.core.Acct().Operation(op.operation, name, op.start, nil)
	op.span.SetAttributes(telemetry.AttrBackendName.String(name))
	op.span.SetAttributes(telemetry.AttrObjectSize.Int64(size))
	if b.parallel {
		op.span.SetAttributes(telemetry.AttrParallelBroadcast.Bool(true))
	}
	op.span.SetStatus(codes.Ok, "")
}

// broadcastAllFailed builds the error for a broadcast no backend answered. The
// last probe error is wrapped so the server can tell "backend unreachable"
// (502) from "object not found" (404); with no probe errors it is not found.
func broadcastAllFailed(span trace.Span, errs []error) error {
	if len(errs) == 0 {
		observe.MarkSpanError(span, "no backends available")
		return core.ErrObjectNotFound
	}
	lastErr := errs[len(errs)-1]
	observe.RecordSpanError(span, lastErr)
	return fmt.Errorf("all backends failed during degraded read: %w", lastErr)
}
