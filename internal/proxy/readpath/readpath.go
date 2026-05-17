// -------------------------------------------------------------------------------
// Read Failover - Per-Key Backend Failover and Degraded-Mode Broadcast
//
// Author: Alex Freidah
//
// Orchestrates the read protocol on behalf of object.Manager:
//   - happy path: resolve all object_locations rows for the key, try each
//     backend in stored order until one succeeds.
//   - DB-unavailable path: fall back to a broadcast over every configured
//     backend (sequential or parallel per configuration), remembering the
//     winner in the in-memory location cache for the next degraded read.
//
// The caller supplies a Probe callback that knows how to perform the
// actual backend operation (HeadObject, GetObject, integrity-wrapped GET,
// ...) and how to release any per-attempt timeout. Failover does not
// care what the operation is, only that the probe returns a size for
// observability and a cleanup func to release the winner's resources.
//
// All observability (span creation, failover attribute, broadcast
// attribute, metric emission) lives here so consumers do not have to
// reproduce the per-operation telemetry shape at every call site.
// -------------------------------------------------------------------------------

package readpath

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// spanPrefix is prepended to every OpenTelemetry span name the
// Failover creates so traces clearly distinguish the manager layer
// ("Manager GetObject") from the backend layer ("Backend GetObject")
// in the same end-to-end trace. Matches the prefix used by other proxy
// subsystems for consistency at the operator's filter level.
const spanPrefix = "Manager "

// ErrUsageLimitSkip is the sentinel a Probe returns when it declined to
// attempt a backend purely because its usage limit would be exceeded.
// Failover counts these separately from genuine failures so the final
// "all backends declined for usage limits" outcome can be reported as
// core.ErrUsageLimitExceeded rather than as the last underlying error.
var ErrUsageLimitSkip = errors.New("backend skipped: usage limits exceeded")

// Probe is the per-backend callback the caller provides. loc carries
// the matching ObjectLocation row so callbacks that need encryption
// metadata can read it directly without a side-channel lookup; loc is
// nil in degraded-mode broadcasts where the DB is unreachable.
// beName is always populated (loc.BackendName during failover, or the
// degraded-mode caller's chosen name) so callbacks have a single
// source for span / usage attribution.
//
// The callback owns its own timeout context and is responsible for
// releasing it on the error path. On success it returns a cleanup func
// the orchestrator invokes once the winner is declared; that cleanup
// either releases the timeout immediately (HEAD - no streaming body)
// or is a no-op because the callback already attached the cancel to
// the result body's Close (GET).
type Probe func(ctx context.Context, beName string, loc *core.ObjectLocation, b backend.ObjectBackend) (size int64, cleanup func(), err error)

// NoopCleanup is the cleanup a Probe returns when there is nothing for
// the orchestrator to release: error paths where the callback already
// cancelled its own timeout, and the GET success path where the cancel
// is attached to the body's Close.
func NoopCleanup() {
	// meant to be a noop function
}

// Failover orchestrates per-key read failover across backends.
// One instance per object.Manager; safe for concurrent reads.
type Failover struct {
	core              Core
	stores            ObjectLocationLister
	cache             LocationCache
	parallelBroadcast bool
	log               *slog.Logger
}

// New constructs a Failover. parallelBroadcast selects between
// sequential and per-backend-goroutine broadcasts in degraded mode.
// The component-scoped logger is built in the constructor body per the
// project's logging convention.
func New(core Core, stores ObjectLocationLister, cache LocationCache, parallelBroadcast bool) *Failover {
	return &Failover{
		core:              core,
		stores:            stores,
		cache:             cache,
		parallelBroadcast: parallelBroadcast,
		log:               slog.Default().With(logfmt.Component("readpath")),
	}
}

// -------------------------------------------------------------------------
// READ FAILOVER
// -------------------------------------------------------------------------

// Read runs the full read-with-failover protocol: starts the span,
// resolves all locations for the key, and tries each backend in
// turn. On core.ErrDBUnavailable falls back to broadcastRead. Returns
// the winning backend name and any error.
func (f *Failover) Read(ctx context.Context, operation, key string, probe Probe) (string, error) {
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, spanPrefix+operation,
		telemetry.AttrObjectKey.String(key),
	)
	defer span.End()

	locations, err := f.stores.GetAllObjectLocations(ctx, key)
	if err != nil {
		if errors.Is(err, core.ErrObjectNotFound) {
			observe.MarkSpanError(span, "object not found")
			return "", err
		}
		if errors.Is(err, core.ErrDBUnavailable) {
			return f.broadcastRead(ctx, operation, key, start, span, probe)
		}
		observe.RecordSpanError(span, err)
		return "", fmt.Errorf("failed to find object location: %w", err)
	}

	return f.tryEachLocation(ctx, operation, key, start, span, locations, probe)
}

// tryEachLocation walks the resolved location list and runs the probe
// against each, returning on the first success. Records lastErr and a
// count of usage-limit skips so the failure-path return distinguishes
// "all backends declined for usage limits" from "all backends genuinely
// failed."
func (f *Failover) tryEachLocation(ctx context.Context, operation, key string, start time.Time, span trace.Span, locations []core.ObjectLocation, probe Probe) (string, error) {
	var lastErr error
	var limitSkips int
	for i := range locations {
		span.SetAttributes(telemetry.AttrBackendName.String(locations[i].BackendName))
		name := locations[i].BackendName

		be, ok := f.core.Backends()[name]
		if !ok {
			lastErr = fmt.Errorf("backend %s not found", name)
			continue
		}

		size, cleanup, err := probe(ctx, name, &locations[i], be)
		if err != nil {
			lastErr = err
			if errors.Is(err, ErrUsageLimitSkip) {
				limitSkips++
			}
			if i < len(locations)-1 {
				f.log.WarnContext(ctx, operation+": copy failed, trying next",
					"key", key, "failed_backend", name, "error", err)
			}
			continue
		}

		cleanup()
		f.core.RecordOperation(operation, name, start, nil)
		if i > 0 {
			span.SetAttributes(telemetry.AttrFailover.Bool(true))
		}
		span.SetAttributes(telemetry.AttrObjectSize.Int64(size))
		span.SetStatus(codes.Ok, "")
		return name, nil
	}

	return "", failoverFailureResult(span, operation, locations, lastErr, limitSkips)
}

// failoverFailureResult finalises the span and chooses between the
// usage-limit-exceeded sentinel and the underlying lastErr based on
// whether every location declined for over-limit reasons.
func failoverFailureResult(span trace.Span, operation string, locations []core.ObjectLocation, lastErr error, limitSkips int) error {
	if limitSkips > 0 && limitSkips == len(locations) {
		telemetry.UsageLimitRejectionsTotal.WithLabelValues(operation, "read").Inc()
		observe.MarkSpanError(span, "all copies over usage limit")
		return core.ErrUsageLimitExceeded
	}
	observe.RecordSpanError(span, lastErr)
	return lastErr
}

// -------------------------------------------------------------------------
// DEGRADED-MODE BROADCAST
// -------------------------------------------------------------------------

// broadcastRead tries all backends when the DB is unavailable. Checks
// the location cache first for a known-good backend, then dispatches
// to either parallel or sequential broadcast based on configuration.
func (f *Failover) broadcastRead(ctx context.Context, operation, key string, start time.Time, span trace.Span, probe Probe) (string, error) {
	span.SetAttributes(telemetry.AttrDegradedMode.Bool(true))
	telemetry.DegradedReadsTotal.WithLabelValues(operation).Inc()

	// --- Check location cache first ---
	if cachedBackend, ok := f.cache.Get(key); ok {
		if be, exists := f.core.Backends()[cachedBackend]; exists {
			// Degraded mode: no DB row available, probe must handle nil loc.
			size, cleanup, err := probe(ctx, cachedBackend, nil, be)
			if err == nil {
				cleanup()
				f.core.RecordOperation(operation, cachedBackend, start, nil)
				span.SetAttributes(telemetry.AttrCacheHit.Bool(true))
				span.SetAttributes(telemetry.AttrObjectSize.Int64(size))
				span.SetStatus(codes.Ok, "")
				telemetry.DegradedCacheHitsTotal.Inc()
				return cachedBackend, nil
			}
			// Cache hit but backend failed - fall through to broadcast.
			// The probe already released its timeout on the error path.
		}
	}

	concurrency := 1
	if f.parallelBroadcast {
		concurrency = len(f.core.BackendOrder())
	}
	return f.tryAllBackends(ctx, operation, key, start, span, concurrency, probe)
}

// tryAllBackends dispatches to the sequential or parallel branch based
// on concurrency. Both branches return the first backend whose probe
// succeeds and cache the winner's name for future degraded reads.
func (f *Failover) tryAllBackends(
	ctx context.Context,
	operation, key string,
	start time.Time,
	span trace.Span,
	concurrency int,
	probe Probe,
) (string, error) {
	if concurrency <= 1 {
		return f.tryBackendsSequentially(ctx, operation, key, start, span, probe)
	}
	return f.tryBackendsInParallel(ctx, operation, key, start, span, probe)
}

// tryBackendsSequentially walks BackendOrder one backend at a time. The
// first success short-circuits and is recorded as the broadcast winner;
// otherwise the last error (if any) is wrapped as a degraded-read failure.
func (f *Failover) tryBackendsSequentially(
	ctx context.Context,
	operation, key string,
	start time.Time,
	span trace.Span,
	probe Probe,
) (string, error) {
	var lastErr error
	for _, name := range f.core.BackendOrder() {
		be, ok := f.core.Backends()[name]
		if !ok {
			continue
		}
		// Degraded mode: no DB row available, probe must handle nil loc.
		size, cleanup, err := probe(ctx, name, nil, be)
		if err != nil {
			lastErr = err
			continue
		}
		cleanup()
		f.recordBroadcastWinner(operation, key, name, size, start, span, false)
		return name, nil
	}
	return broadcastAllFailed(span, lastErr)
}

// broadcastResult carries one parallel-probe outcome back to the
// fan-in loop. cleanup is the success-path cleanup the orchestrator
// invokes either inline (winner) or via the loser-drain goroutine.
type broadcastResult struct {
	name    string
	size    int64
	err     error
	cleanup func()
}

// tryBackendsInParallel launches one probe per backend in BackendOrder
// and returns the first success, draining the remaining responses in
// the background so loser cleanups fire promptly.
func (f *Failover) tryBackendsInParallel(
	ctx context.Context,
	operation, key string,
	start time.Time,
	span trace.Span,
	probe Probe,
) (string, error) {
	ch := make(chan broadcastResult, len(f.core.BackendOrder()))
	launched := f.launchBackendProbes(ctx, ch, probe)

	var lastErr error
	for received := 0; received < launched; received++ { //nolint:intrange // received used in arithmetic below
		r := <-ch
		if r.err != nil {
			lastErr = r.err
			continue
		}
		if r.cleanup != nil {
			r.cleanup()
		}
		if remaining := launched - received - 1; remaining > 0 {
			go drainAndCleanupLosers(ch, remaining)
		}
		f.recordBroadcastWinner(operation, key, r.name, r.size, start, span, true)
		return r.name, nil
	}
	return broadcastAllFailed(span, lastErr)
}

// launchBackendProbes spawns one goroutine per eligible backend, each
// of which writes its outcome to ch. Returns the number of probes
// launched so the caller knows how many results to read.
func (f *Failover) launchBackendProbes(
	ctx context.Context,
	ch chan<- broadcastResult,
	probe Probe,
) int {
	launched := 0
	for _, name := range f.core.BackendOrder() {
		be, ok := f.core.Backends()[name]
		if !ok {
			continue
		}
		launched++
		go runBackendProbe(ctx, name, be, probe, ch)
	}
	return launched
}

// runBackendProbe is the per-backend goroutine body. On success it
// forwards the probe's cleanup so the orchestrator (winner) or the
// loser-drain (everyone else) can release the timeout context promptly
// instead of waiting for the deadline to fire on its own.
func runBackendProbe(
	ctx context.Context,
	name string,
	be backend.ObjectBackend,
	probe Probe,
	ch chan<- broadcastResult,
) {
	// Degraded mode: no DB row available, probe must handle nil loc.
	size, cleanup, err := probe(ctx, name, nil, be)
	if err != nil {
		ch <- broadcastResult{name: name, err: err}
		return
	}
	ch <- broadcastResult{name: name, size: size, cleanup: cleanup}
}

// drainAndCleanupLosers reads the remaining results from ch after a
// winner has been declared and invokes any cleanup the losers returned.
// Best-effort: a sender that panicked or closed early is tolerated.
func drainAndCleanupLosers(ch <-chan broadcastResult, remaining int) {
	defer func() { recover() }() //nolint:errcheck // best-effort drain
	for range remaining {
		if lr := <-ch; lr.cleanup != nil {
			lr.cleanup()
		}
	}
}

// recordBroadcastWinner caches the winner's name for future degraded
// reads, emits operation metrics, and sets the success-path span
// attributes.
func (f *Failover) recordBroadcastWinner(
	operation, key, name string,
	size int64,
	start time.Time,
	span trace.Span,
	parallel bool,
) {
	f.cache.Set(key, name)
	f.core.RecordOperation(operation, name, start, nil)
	span.SetAttributes(telemetry.AttrBackendName.String(name))
	span.SetAttributes(telemetry.AttrObjectSize.Int64(size))
	if parallel {
		span.SetAttributes(telemetry.AttrParallelBroadcast.Bool(true))
	}
	span.SetStatus(codes.Ok, "")
}

// broadcastAllFailed builds the all-failed return value. When at least
// one backend returned an error, that error is wrapped so the server
// can distinguish "backend unreachable" (502) from "object not found"
// (404).
func broadcastAllFailed(span trace.Span, lastErr error) (string, error) {
	if lastErr != nil {
		observe.RecordSpanError(span, lastErr)
		return "", fmt.Errorf("all backends failed during degraded read: %w", lastErr)
	}
	observe.MarkSpanError(span, "no backends available")
	return "", core.ErrObjectNotFound
}
