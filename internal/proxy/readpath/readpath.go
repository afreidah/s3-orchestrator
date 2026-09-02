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
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
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

// ProbeResult is the outcome of one successful backend probe. Value is the
// operation's result (e.g. *backend.GetObjectResult) and flows back to the
// caller when this probe wins. Cleanup releases the result's transient
// resources when the probe loses the race (e.g. closing a discarded GET body);
// it is never invoked on the winner, whose Value owns its own lifecycle.
// Cleanup may be NoopCleanup when there is nothing to release.
type ProbeResult[T any] struct {
	Value   T
	Size    int64
	Cleanup func()
}

// Probe is the per-backend callback the caller provides. loc carries the
// matching ObjectLocation row so callbacks that need encryption metadata can
// read it directly without a side-channel lookup; loc is nil in degraded-mode
// broadcasts where the DB is unreachable. beName is always populated
// (loc.BackendName during failover, or the degraded-mode caller's chosen name)
// so callbacks have a single source for span / usage attribution.
//
// The callback owns its own timeout context: it must release it on the error
// path, and for a winning result it transfers ownership to Value (a GET
// attaches the cancel to the body's Close; a HEAD has no body and releases the
// timeout before returning). A losing result is released via ProbeResult.Cleanup.
type Probe[T any] func(ctx context.Context, beName string, loc *core.ObjectLocation, b backend.ObjectBackend) (ProbeResult[T], error)

// NoopCleanup is the ProbeResult.Cleanup a Probe returns when a losing result
// holds nothing to release - e.g. a HEAD, which carries no body and has already
// cancelled its own timeout before returning.
func NoopCleanup() {
	// meant to be a noop function
}

// FailoverStores is the narrow persistence surface read failover needs: object
// location lookups for the normal DB-backed path. Declared locally so
// readpath does not pull in the full MetadataStore.
type FailoverStores interface {
	core.ObjectStore
}

// Failover orchestrates per-key read failover across backends.
// One instance per object.Manager; safe for concurrent reads.
type Failover struct {
	core        ReadRuntime
	stores      FailoverStores
	broadcaster *Broadcaster // degraded-mode fan-out; used only on DB outage
	// degradedReadsEnabled false makes degraded reads fail fast instead of broadcasting.
	degradedReadsEnabled bool
	log                  *slog.Logger
}

// FailoverDeps groups the read-failover constructor parameters. The three
// degraded-mode flags are named fields so call sites can't transpose them.
type FailoverDeps struct {
	Core                         ReadRuntime
	Stores                       FailoverStores
	Cache                        LocationCache
	ParallelBroadcast            bool
	DegradedBroadcastParallelism int
	DegradedReadsEnabled         bool
	// BackendTimeout bounds the loser-drain goroutine after a winner is
	// declared so a hung backend can't strand it. Zero falls back to a
	// safe default.
	BackendTimeout time.Duration
}

// New constructs a Failover. When DegradedReadsEnabled is false, a DB
// outage surfaces as ErrServiceUnavailable instead of broadcasting.
func New(deps *FailoverDeps) *Failover {
	must.NotNil("deps", deps)
	must.NotNil("Core", deps.Core)
	must.NotNil("Stores", deps.Stores)
	must.NotNil("Cache", deps.Cache)
	return &Failover{
		core:                 deps.Core,
		stores:               deps.Stores,
		degradedReadsEnabled: deps.DegradedReadsEnabled,
		log:                  slog.Default().With(logfmt.Component("readpath")),
		broadcaster: &Broadcaster{
			core:         deps.Core,
			cache:        deps.Cache,
			parallel:     deps.ParallelBroadcast,
			parallelism:  deps.DegradedBroadcastParallelism,
			drainTimeout: drainTimeoutOrDefault(deps.BackendTimeout),
		},
	}
}

// -------------------------------------------------------------------------
// READ FAILOVER
// -------------------------------------------------------------------------

// Read runs the full read-with-failover protocol: starts the span,
// resolves all locations for the key, and tries each backend in turn. On
// core.ErrDBUnavailable it enters degraded mode and delegates to the
// broadcaster (unless degraded reads are disabled). Returns the winning
// backend name and any error.
// readOp bundles the per-read context shared by every failover and broadcast
// helper: the operation label, key, start time, and span. Grouping them keeps
// the helper signatures small and the call sites readable.
type readOp struct {
	operation s3op.Operation
	key       string
	start     time.Time
	span      trace.Span
}

func (f *Failover) Read[T any](ctx context.Context, operation s3op.Operation, key string, probe Probe[T]) (T, string, error) {
	var zero T
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, spanPrefix+operation.String(),
		telemetry.AttrObjectKey.String(key),
	)
	defer span.End()
	op := readOp{operation: operation, key: key, start: start, span: span}

	locations, err := f.stores.GetAllObjectLocations(ctx, key)
	if err != nil {
		if errors.Is(err, core.ErrObjectNotFound) {
			observe.MarkSpanError(span, "object not found")
			return zero, "", err
		}
		if errors.Is(err, core.ErrDBUnavailable) {
			span.SetAttributes(telemetry.AttrDegradedMode.Bool(true))
			telemetry.DegradedReadsTotal.WithLabelValues(operation.String()).Inc()
			if !f.degradedReadsEnabled {
				observe.MarkSpanError(span, "degraded reads disabled by operator")
				return zero, "", core.ErrServiceUnavailable
			}
			return f.broadcaster.broadcastRead(ctx, op, probe)
		}
		observe.RecordSpanError(span, err)
		return zero, "", fmt.Errorf("failed to find object location: %w", err)
	}

	return f.tryEachLocation(ctx, op, locations, probe)
}

// tryEachLocation walks the resolved location list and runs the probe
// against each, returning on the first success. Records lastErr and a
// count of usage-limit skips so the failure-path return distinguishes
// "all backends declined for usage limits" from "all backends genuinely
// failed."
func (f *Failover) tryEachLocation[T any](ctx context.Context, op readOp, locations []core.ObjectLocation, probe Probe[T]) (T, string, error) {
	var zero T
	var lastErr error
	var limitSkips int
	for i := range locations {
		op.span.SetAttributes(telemetry.AttrBackendName.String(locations[i].BackendName))
		name := locations[i].BackendName

		be, ok := f.core.Backends()[name]
		if !ok {
			lastErr = fmt.Errorf("backend %s not found", name)
			continue
		}

		res, err := probe(ctx, name, &locations[i], be)
		if err != nil {
			lastErr = err
			if errors.Is(err, ErrUsageLimitSkip) {
				limitSkips++
			}
			if i < len(locations)-1 {
				f.log.WarnContext(ctx, op.operation.String()+": copy failed, trying next",
					"key", op.key, "failed_backend", name, "error", err)
			}
			continue
		}

		// First success wins; it returns immediately, so there are no losers to
		// clean up here. The winner's Value owns its own lifecycle.
		f.core.Acct().Operation(op.operation, name, op.start, nil)
		if i > 0 {
			op.span.SetAttributes(telemetry.AttrFailover.Bool(true))
		}
		op.span.SetAttributes(telemetry.AttrObjectSize.Int64(res.Size))
		op.span.SetStatus(codes.Ok, "")
		return res.Value, name, nil
	}

	return zero, "", failoverFailureResult(op.span, op.operation, locations, lastErr, limitSkips)
}

// failoverFailureResult finalises the span and chooses between the
// usage-limit-exceeded sentinel and the underlying lastErr based on
// whether every location declined for over-limit reasons.
func failoverFailureResult(span trace.Span, operation s3op.Operation, locations []core.ObjectLocation, lastErr error, limitSkips int) error {
	if limitSkips > 0 && limitSkips == len(locations) {
		telemetry.UsageLimitRejectionsTotal.WithLabelValues(operation.String(), "read").Inc()
		observe.MarkSpanError(span, "all copies over usage limit")
		return core.ErrUsageLimitExceeded
	}
	// No location was even attempted, so no probe error was recorded. The
	// store contract makes this unreachable, but a nil return here would read
	// as a successful zero-value response.
	if lastErr == nil {
		observe.MarkSpanError(span, "no copies to read from")
		return core.ErrObjectNotFound
	}
	observe.RecordSpanError(span, lastErr)
	return lastErr
}
