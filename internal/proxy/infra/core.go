// -------------------------------------------------------------------------------
// Backend BackendRuntime - Composition Layer for Storage Infrastructure
//
// Author: Alex Freidah
//
// *BackendRuntime is the public-facing composition root that other proxy
// subpackages (object, multipart, writepath, readpath) consume through
// their own consumer-declared interfaces. Internally it owns five
// focused capability services and delegates to them:
//
//   - backendRegistry : backend map, iteration order, drain/health filters
//   - usagePolicy     : per-backend usage limits + max object size
//   - timeoutPolicy   : per-call backend timeout + helpers that pair it
//                       with a single backend RPC (DeleteWithTimeout,
//                       StreamCopy)
//   - errorClassifier : store-error -> S3-error translation with span
//                       and telemetry side effects
//   - admissionGate   : bounded-concurrency admission semaphore
//
// *BackendRuntime exposes Backends(), GetBackend(), WithTimeout(), and
// friends as thin forwards into the appropriate capability, which keeps
// the consumer-declared interface pattern intact (callers see methods on
// *BackendRuntime, not producer-side interfaces) and makes each
// capability easy to test in isolation.
// -------------------------------------------------------------------------------

package infra

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/accounting"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// DrainChecker reports whether a named backend is currently being drained.
// *BackendRuntime consumes this so drain ownership can live in the drain subpackage
// while *BackendRuntime filters write eligibility.
type DrainChecker interface {
	IsDraining(name string) bool
}

// Config bundles every input *BackendRuntime needs at construction. Exposed so
// callers (root proxy package, tests) can build a *BackendRuntime directly.
type Config struct {
	Backends         map[string]backend.ObjectBackend
	Order            []string
	BackendTimeout   time.Duration
	Usage            *counter.UsageTracker
	RoutingStrategy  config.RoutingStrategy
	MaxObjectSizes   map[string]int64
	MetricsCollector *metrics.Collector
	AdmissionSem     chan struct{}
	Log              *slog.Logger
}

// BackendRuntime composes the five capability services every proxy subpackage
// needs. It deliberately holds no store: each collaborator takes the store
// roles it needs directly, which is what lets every worker reuse the runtime
// without dragging persistence along. For which methods belong here versus on
// a collaborator, see docs/style-guide.md "Where new methods live".
type BackendRuntime struct {
	registry         *backendRegistry
	usage            *usagePolicy
	timeouts         *timeoutPolicy
	classifier       *errorClassifier
	admission        *admissionGate
	routingStrategy  config.RoutingStrategy
	metricsCollector *metrics.Collector
	log              *slog.Logger
	recorder         *accounting.Recorder
}

// New constructs a *BackendRuntime from cfg. The drain checker is wired
// post-construction via SetDrainChecker to break the
// BackendRuntime <-> drain.Manager cycle. The accounting Recorder is
// built here so every consumer of *BackendRuntime shares one instance that
// observes the same usage tracker and the (later-wired) metrics
// collector via the closure over c.RecordOperation.
func New(cfg *Config) *BackendRuntime {
	c := &BackendRuntime{
		registry:         newBackendRegistry(cfg.Backends, cfg.Order),
		usage:            newUsagePolicy(cfg.Usage, cfg.MaxObjectSizes),
		timeouts:         newTimeoutPolicy(cfg.BackendTimeout),
		classifier:       newErrorClassifier(),
		admission:        newAdmissionGate(cfg.AdmissionSem),
		routingStrategy:  cfg.RoutingStrategy,
		metricsCollector: cfg.MetricsCollector,
		log:              cfg.Log,
	}
	c.recorder = accounting.New(cfg.Usage, c.RecordOperation)
	return c
}

// -------------------------------------------------------------------------
// ACCOUNTING + LOGGING + METRICS
// -------------------------------------------------------------------------

// Acct returns the shared accounting.Recorder. Consumers should call
// Acct().APICall / Egress / Ingress / Operation instead of reaching
// through Usage() and RecordOperation directly so the per-backend
// accounting rules stay centralised.
func (c *BackendRuntime) Acct() *accounting.Recorder {
	return c.recorder
}

// SetMetricsCollector installs the metrics collector after BackendRuntime
// construction. The collector depends on the usage tracker which is
// owned by *BackendRuntime, so the collector is built after *BackendRuntime and wired
// back in.
func (c *BackendRuntime) SetMetricsCollector(m *metrics.Collector) {
	c.metricsCollector = m
}

// MetricsCollector returns the wired metrics collector (nil if unset).
func (c *BackendRuntime) MetricsCollector() *metrics.Collector {
	return c.metricsCollector
}

// Log returns the component-scoped logger; falls back to slog.Default()
// when *BackendRuntime was constructed without one.
func (c *BackendRuntime) Log() *slog.Logger {
	if c.log == nil {
		return slog.Default()
	}
	return c.log
}

// RoutingStrategy returns the configured routing strategy.
func (c *BackendRuntime) RoutingStrategy() config.RoutingStrategy {
	return c.routingStrategy
}

// -------------------------------------------------------------------------
// DRAIN WIRING
// -------------------------------------------------------------------------

// SetDrainChecker points the eligibility filter at the drain manager so
// IsDraining reflects live drain state. Called once the drain manager
// exists, since it is built after the runtime.
func (c *BackendRuntime) SetDrainChecker(d DrainChecker) {
	c.registry.SetDrainChecker(d)
}

// -------------------------------------------------------------------------
// BACKEND REGISTRY (forwards to backendRegistry)
// -------------------------------------------------------------------------

// GetBackend returns the named backend, or an error if it doesn't exist.
func (c *BackendRuntime) GetBackend(name string) (backend.ObjectBackend, error) {
	return c.registry.Get(name)
}

// Backends returns the backend map (worker.Ops contract).
func (c *BackendRuntime) Backends() map[string]backend.ObjectBackend {
	return c.registry.All()
}

// BackendOrder returns the configured backend ordering (worker.Ops contract).
func (c *BackendRuntime) BackendOrder() []string {
	return c.registry.Order()
}

// IsDraining returns true if the named backend is currently being drained.
// Returns false when no drain manager is wired.
func (c *BackendRuntime) IsDraining(name string) bool {
	return c.registry.IsDraining(name)
}

// ExcludeDraining filters out backends that are currently draining.
func (c *BackendRuntime) ExcludeDraining(eligible []string) []string {
	return c.registry.ExcludeDraining(eligible)
}

// ExcludeUnhealthy filters out backends whose circuit breaker is open
// and not probe-eligible.
func (c *BackendRuntime) ExcludeUnhealthy(eligible []string) []string {
	return c.registry.ExcludeUnhealthy(eligible)
}

// -------------------------------------------------------------------------
// USAGE POLICY (forwards to usagePolicy)
// -------------------------------------------------------------------------

// Usage returns the usage tracker (worker.Ops contract).
func (c *BackendRuntime) Usage() *counter.UsageTracker {
	return c.usage.Tracker()
}

// MaxObjectSize returns the per-backend max object size; 0 means
// unlimited.
func (c *BackendRuntime) MaxObjectSize(name string) int64 {
	return c.usage.MaxObjectSize(name)
}

// EligibleForWrite returns backends that are not draining, not
// circuit-broken, and within usage limits / max-object-size for the
// given operation. Composed pipeline of registry filters + usage
// filter so each capability owns its half of the decision.
func (c *BackendRuntime) EligibleForWrite(ops []s3op.Operation, egress, ingress int64) []string {
	eligible := c.registry.ExcludeDraining(c.registry.Order())
	eligible = c.registry.ExcludeUnhealthy(eligible)
	return c.usage.FilterEligible(eligible, ops, egress, ingress)
}

// -------------------------------------------------------------------------
// ADMISSION (forwards to admissionGate)
// -------------------------------------------------------------------------

// AcquireAdmission blocks until a slot is available, or returns false
// if ctx is cancelled. Returns true immediately when no semaphore is
// wired.
func (c *BackendRuntime) AcquireAdmission(ctx context.Context) bool {
	return c.admission.Acquire(ctx)
}

// ReleaseAdmission returns a slot to the admission semaphore.
func (c *BackendRuntime) ReleaseAdmission() {
	c.admission.Release()
}

// AdmissionSem returns the underlying semaphore channel (nil if
// unwired). Callers that need the raw channel (split admission
// controllers) read this; the AcquireAdmission/ReleaseAdmission
// methods are preferred.
func (c *BackendRuntime) AdmissionSem() chan struct{} {
	return c.admission.Sem()
}

// -------------------------------------------------------------------------
// TIMEOUT (forwards to timeoutPolicy)
// -------------------------------------------------------------------------

// WithTimeout returns a context with the configured backend timeout
// applied.
func (c *BackendRuntime) WithTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return c.timeouts.WithTimeout(ctx)
}

// DeleteWithTimeout deletes an object from a backend using the
// configured backend timeout.
func (c *BackendRuntime) DeleteWithTimeout(ctx context.Context, be backend.ObjectBackend, key string) error {
	return c.timeouts.DeleteWithTimeout(ctx, be, key)
}

// StreamCopy reads an object from src and writes it to dst with timeouts
// applied to each leg, admitting the transfer against both backends' usage
// limits first. Returns the bytes moved, or a *backend.CopyError tagged with
// the failing phase.
//
// Admission lives here rather than at the call sites because this is the one
// place every backend-to-backend copy passes through. The replicator used to
// check only its destination and read from whichever source was healthy,
// which let a fleet-wide repair drain a source backend's monthly egress
// budget; the rebalancer checked both sides. Enforcing here makes the two
// agree by construction and leaves a caller nothing to forget.
//
// Accounting stays with the caller. Both callers charge the size their
// metadata commit settled on rather than the size that crossed the wire, and
// the two disagree only when an overwrite lands mid-copy, which each of them
// reports in its own terms. sizeEstimate is what admission is judged on.
func (c *BackendRuntime) StreamCopy(ctx context.Context, src, dst backend.CopyEndpoint, key string, sizeEstimate int64) (int64, error) {
	// Refusals are tagged with the leg that had no headroom, so callers get
	// the same structural retry answer they already act on for I/O failures:
	// another source may have egress left, but a destination that is full
	// ends the attempt.
	if !c.Acct().Allow(src.Name, []s3op.Operation{s3op.GetObject}, sizeEstimate, 0) {
		return 0, &backend.CopyError{
			Phase: backend.CopyPhaseRead,
			Err:   fmt.Errorf("source %s: %w", src.Name, core.ErrUsageLimitExceeded),
		}
	}
	if !c.Acct().Allow(dst.Name, []s3op.Operation{s3op.PutObject}, 0, sizeEstimate) {
		return 0, &backend.CopyError{
			Phase: backend.CopyPhaseWrite,
			Err:   fmt.Errorf("destination %s: %w", dst.Name, core.ErrUsageLimitExceeded),
		}
	}
	return c.timeouts.StreamCopy(ctx, src.Backend, dst.Backend, key)
}

// GetWithTimeout issues a GET against be using the configured backend
// timeout, returning the result and a cancel func the caller owns.
func (c *BackendRuntime) GetWithTimeout(ctx context.Context, be backend.ObjectBackend, key, rangeHeader string) (*backend.GetObjectResult, context.CancelFunc, error) {
	return c.timeouts.GetWithTimeout(ctx, be, key, rangeHeader)
}

// HeadWithTimeout issues a HEAD against be using the configured backend
// timeout.
func (c *BackendRuntime) HeadWithTimeout(ctx context.Context, be backend.ObjectBackend, key string) (*backend.HeadObjectResult, error) {
	return c.timeouts.HeadWithTimeout(ctx, be, key)
}

// -------------------------------------------------------------------------
// ERROR CLASSIFICATION (forwards to errorClassifier)
// -------------------------------------------------------------------------

// ClassifyWriteError translates store errors from write-path operations
// into S3-compatible errors and updates the tracing span.
func (c *BackendRuntime) ClassifyWriteError(span trace.Span, operation string, err error) error {
	return c.classifier.ClassifyWriteError(span, operation, err)
}

// -------------------------------------------------------------------------
// METRICS (delegates directly; collector lives on BackendRuntime)
// -------------------------------------------------------------------------

// RecordOperation delegates to the metrics collector.
func (c *BackendRuntime) RecordOperation(operation, backend string, start time.Time, err error) {
	c.metricsCollector.RecordOperation(operation, backend, start, err)
}

// UpdateQuotaMetrics refreshes Prometheus gauges from the metadata store.
func (c *BackendRuntime) UpdateQuotaMetrics(ctx context.Context) error {
	return c.metricsCollector.UpdateQuotaMetrics(ctx)
}
