// -------------------------------------------------------------------------------
// Backend Core - Shared Infrastructure for Storage Components
//
// Author: Alex Freidah
//
// Common fields and utility methods shared across the BackendManager and the
// per-domain managers (object, multipart) plus the reconcile and writepath
// subpackages. Embedded into root-package BackendManager; consumed by
// subpackages either via embedding or as a concrete *Core dependency.
// -------------------------------------------------------------------------------

// Package infra exposes the proxy-package backend infrastructure (backend
// map, usage tracker, drain checker, metrics, admission, per-op timeouts)
// as an importable type so subpackages can share it without an import
// cycle back to the root proxy package.
package infra

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/accounting"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// DrainChecker reports whether a named backend is currently being drained.
// *Core consumes this so drain ownership can live in the drain subpackage
// while *Core filters write eligibility.
type DrainChecker interface {
	IsDraining(name string) bool
}

// Config bundles every input *Core needs at construction. Exposed so
// callers (root proxy package, tests) can build a *Core directly.
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

// Core holds the non-store infrastructure that multiple storage components
// need. Per-role store views live on the root-package BackendManager;
// *Core deliberately holds none.
type Core struct {
	backends         map[string]backend.ObjectBackend
	order            []string
	backendTimeout   time.Duration
	usage            *counter.UsageTracker
	routingStrategy  config.RoutingStrategy
	maxObjectSizes   map[string]int64
	drainMgr         DrainChecker
	metricsCollector *metrics.Collector
	admissionSem     chan struct{}
	log              *slog.Logger
	recorder         *accounting.Recorder // built once in New; centralizes per-backend usage + operation metric semantics
}

// New constructs a *Core from cfg. drainMgr is wired post-construction
// via SetDrainChecker to break the BackendManager ↔ drain.Manager cycle.
// The accounting Recorder is built here so every consumer of *Core
// shares one instance that observes the same usage tracker and the
// (later-wired) metrics collector via the closure over c.RecordOperation.
func New(cfg *Config) *Core {
	c := &Core{
		backends:         cfg.Backends,
		order:            cfg.Order,
		backendTimeout:   cfg.BackendTimeout,
		usage:            cfg.Usage,
		routingStrategy:  cfg.RoutingStrategy,
		maxObjectSizes:   cfg.MaxObjectSizes,
		metricsCollector: cfg.MetricsCollector,
		admissionSem:     cfg.AdmissionSem,
		log:              cfg.Log,
	}
	c.recorder = accounting.New(c.usage, c.RecordOperation)
	return c
}

// Acct returns the shared accounting.Recorder. Consumers should call
// Acct().APICall / Egress / Ingress / Operation instead of reaching
// through Usage() and RecordOperation directly so the per-backend
// accounting rules stay centralised.
func (c *Core) Acct() *accounting.Recorder {
	return c.recorder
}

// SetDrainChecker installs the drain manager after Core has been
// constructed; mirrors the historic post-construction WireDrain call on
// BackendManager.
func (c *Core) SetDrainChecker(d DrainChecker) {
	c.drainMgr = d
}

// SetMetricsCollector installs the metrics collector after Core
// construction. The collector depends on the usage tracker which is owned
// by *Core, so the collector is built after *Core and wired back in.
func (c *Core) SetMetricsCollector(m *metrics.Collector) {
	c.metricsCollector = m
}

// MetricsCollector returns the wired metrics collector (nil if unset).
func (c *Core) MetricsCollector() *metrics.Collector {
	return c.metricsCollector
}

// Log returns the component-scoped logger; falls back to slog.Default()
// when *Core was constructed without one.
func (c *Core) Log() *slog.Logger {
	if c.log == nil {
		return slog.Default()
	}
	return c.log
}

// RoutingStrategy returns the configured routing strategy.
func (c *Core) RoutingStrategy() config.RoutingStrategy {
	return c.routingStrategy
}

// MaxObjectSize returns the per-backend max object size; 0 means unlimited.
func (c *Core) MaxObjectSize(name string) int64 {
	return c.maxObjectSizes[name]
}

// -------------------------------------------------------------------------
// ADMISSION
// -------------------------------------------------------------------------

// AcquireAdmission blocks until a slot is available, or returns false if
// ctx is cancelled. Returns true immediately when no semaphore is wired.
func (c *Core) AcquireAdmission(ctx context.Context) bool {
	if c.admissionSem == nil {
		return true
	}
	select {
	case c.admissionSem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// ReleaseAdmission returns a slot to the admission semaphore.
func (c *Core) ReleaseAdmission() {
	if c.admissionSem == nil {
		return
	}
	<-c.admissionSem
}

// AdmissionSem returns the underlying semaphore channel (nil if unwired).
// Callers that need the raw channel (split admission controllers) read
// this; the AcquireAdmission/ReleaseAdmission methods are preferred.
func (c *Core) AdmissionSem() chan struct{} {
	return c.admissionSem
}

// -------------------------------------------------------------------------
// TIMEOUT
// -------------------------------------------------------------------------

// WithTimeout returns a context with the configured backend timeout
// applied. Honors a tighter parent deadline. Returns the original context
// if no timeout is configured.
func (c *Core) WithTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.backendTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < c.backendTimeout {
			return context.WithTimeout(ctx, remaining)
		}
	}
	return context.WithTimeout(ctx, c.backendTimeout)
}

// -------------------------------------------------------------------------
// BACKEND LOOKUP
// -------------------------------------------------------------------------

// GetBackend returns the named backend, or an error if it doesn't exist.
func (c *Core) GetBackend(name string) (backend.ObjectBackend, error) {
	b, ok := c.backends[name]
	if !ok {
		return nil, fmt.Errorf("backend %s not found", name)
	}
	return b, nil
}

// Backends returns the backend map (worker.Ops contract).
func (c *Core) Backends() map[string]backend.ObjectBackend { return c.backends }

// BackendOrder returns the configured backend ordering (worker.Ops contract).
func (c *Core) BackendOrder() []string { return c.order }

// Usage returns the usage tracker (worker.Ops contract).
func (c *Core) Usage() *counter.UsageTracker { return c.usage }

// -------------------------------------------------------------------------
// DRAIN STATE
// -------------------------------------------------------------------------

// IsDraining returns true if the named backend is currently being drained.
// Returns false when no drain manager is wired.
func (c *Core) IsDraining(name string) bool {
	if c.drainMgr == nil {
		return false
	}
	return c.drainMgr.IsDraining(name)
}

// ExcludeDraining filters out backends that are currently draining.
func (c *Core) ExcludeDraining(eligible []string) []string {
	filtered := make([]string, 0, len(eligible))
	for _, name := range eligible {
		if !c.IsDraining(name) {
			filtered = append(filtered, name)
		}
	}
	return filtered
}

// ExcludeUnhealthy filters out backends whose circuit breaker is open
// and not probe-eligible.
func (c *Core) ExcludeUnhealthy(eligible []string) []string {
	filtered := make([]string, 0, len(eligible))
	for _, name := range eligible {
		b, ok := c.backends[name]
		if !ok {
			continue
		}
		if cb, ok := b.(*backend.CircuitBreakerBackend); ok && cb.State() == breaker.StateOpen && !cb.ProbeEligible() {
			continue
		}
		filtered = append(filtered, name)
	}
	return filtered
}

// EligibleForWrite returns backends that are not draining, not
// circuit-broken, and within usage limits / max-object-size for the given
// operation.
func (c *Core) EligibleForWrite(apiCalls, egress, ingress int64) []string {
	eligible := make([]string, 0, len(c.order))
	for _, name := range c.order {
		if c.IsDraining(name) {
			continue
		}
		b, ok := c.backends[name]
		if !ok {
			continue
		}
		if cb, ok := b.(*backend.CircuitBreakerBackend); ok && cb.State() == breaker.StateOpen && !cb.ProbeEligible() {
			continue
		}
		if !c.usage.WithinLimits(name, apiCalls, egress, ingress) {
			continue
		}
		if max := c.maxObjectSizes[name]; max > 0 && ingress > max {
			continue
		}
		eligible = append(eligible, name)
	}
	return eligible
}

// -------------------------------------------------------------------------
// ERROR CLASSIFICATION
// -------------------------------------------------------------------------

// ClassifyWriteError translates store errors from write-path operations
// into S3-compatible errors and updates the tracing span.
func (c *Core) ClassifyWriteError(span trace.Span, operation string, err error) error {
	if errors.Is(err, core.ErrDBUnavailable) {
		observe.MarkSpanError(span, "database unavailable")
		telemetry.DegradedWriteRejectionsTotal.WithLabelValues(operation).Inc()
		return core.ErrServiceUnavailable
	}
	if errors.Is(err, core.ErrNoSpaceAvailable) {
		observe.MarkSpanError(span, "insufficient storage")
		return core.ErrInsufficientStorage
	}
	observe.RecordSpanError(span, err)
	return err
}

// DeleteWithTimeout deletes an object from a backend using the configured
// backend timeout.
func (c *Core) DeleteWithTimeout(ctx context.Context, be backend.ObjectBackend, key string) error {
	dctx, dcancel := c.WithTimeout(ctx)
	defer dcancel()
	return be.DeleteObject(dctx, key)
}

// StreamCopy reads an object from src and writes it to dst with timeouts
// applied to each leg. Returns a *backend.CopyError tagged with the
// failing phase.
func (c *Core) StreamCopy(ctx context.Context, src, dst backend.ObjectBackend, key string) error {
	rctx, rcancel := c.WithTimeout(ctx)
	defer rcancel()
	result, err := src.GetObject(rctx, key, "")
	if err != nil {
		return &backend.CopyError{Phase: backend.CopyPhaseRead, Err: err}
	}
	defer func() { _ = result.Body.Close() }()

	wctx, wcancel := c.WithTimeout(ctx)
	defer wcancel()
	_, err = dst.PutObject(wctx, key, result.Body, result.Size, result.ContentType, result.Metadata)
	if err != nil {
		return &backend.CopyError{Phase: backend.CopyPhaseWrite, Err: err}
	}
	return nil
}

// -------------------------------------------------------------------------
// METRICS
// -------------------------------------------------------------------------

// RecordOperation delegates to the metrics collector.
func (c *Core) RecordOperation(operation, backend string, start time.Time, err error) {
	c.metricsCollector.RecordOperation(operation, backend, start, err)
}

// UpdateQuotaMetrics refreshes Prometheus gauges from the metadata store.
func (c *Core) UpdateQuotaMetrics(ctx context.Context) error {
	return c.metricsCollector.UpdateQuotaMetrics(ctx)
}
