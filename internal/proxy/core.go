// -------------------------------------------------------------------------------
// Backend Core - Shared Infrastructure for Storage Components
//
// Author: Alex Freidah
//
// Common fields and utility methods shared across the BackendManager and its
// background worker components (rebalancer, replicator, cleanup, lifecycle).
// BackendManager and future worker types embed *backendCore to inherit these
// utilities through Go's method promotion.
// -------------------------------------------------------------------------------

package proxy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/store/core"

	"go.opentelemetry.io/otel/trace"
)

// drainChecker reports whether a named backend is currently being drained.
// backendCore consumes this so drain ownership can live in the drain
// subpackage while backendCore filters write eligibility.
type drainChecker interface {
	IsDraining(name string) bool
}

// backendCore holds the non-store infrastructure that multiple storage
// components need: the backend map, usage tracker, admission gate,
// drain-checker, metrics collector, and per-op timeouts. Per-role store
// views live on *BackendManager; backendCore deliberately holds none.
type backendCore struct {
	backends         map[string]backend.ObjectBackend // name -> backend
	order            []string                         // backend selection order
	backendTimeout   time.Duration                    // per-operation timeout for backend S3 calls
	usage            *counter.UsageTracker            // per-backend usage counters and limits
	routingStrategy  config.RoutingStrategy           // RoutingPack or RoutingSpread
	maxObjectSizes   map[string]int64                 // per-backend max object size (0 = unlimited)
	drainMgr         drainChecker                     // owned by drain.Manager; wired post-construction
	metricsCollector *metrics.Collector               // Prometheus metric recording and gauge refresh
	admissionSem     chan struct{}                    // shared concurrency semaphore (nil = unlimited)
}

// -------------------------------------------------------------------------
// ADMISSION
// -------------------------------------------------------------------------

// AcquireAdmission blocks until a slot is available in the shared admission
// semaphore, or returns false if ctx is cancelled. Returns true immediately
// when no semaphore is configured.
func (c *backendCore) AcquireAdmission(ctx context.Context) bool {
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

// ReleaseAdmission returns a slot to the admission semaphore. No-op when
// no semaphore is configured.
func (c *backendCore) ReleaseAdmission() {
	if c.admissionSem == nil {
		return
	}
	<-c.admissionSem
}

// -------------------------------------------------------------------------
// TIMEOUT
// -------------------------------------------------------------------------

// WithTimeout returns a context with the configured backend timeout applied.
// If the parent context already has a tighter deadline, the parent deadline
// is preserved to avoid masking upstream timeouts (e.g. HTTP WriteTimeout).
// If no timeout is configured, the original context is returned unchanged.
func (c *backendCore) WithTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.backendTimeout <= 0 {
		// No timeout configured  -  return a no-op cancel so the caller can
		// defer cancel() unconditionally without a nil check.
		return ctx, func() {
			// Intentionally empty: nothing to cancel when no timeout was set.
		}
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
func (c *backendCore) GetBackend(name string) (backend.ObjectBackend, error) {
	b, ok := c.backends[name]
	if !ok {
		return nil, fmt.Errorf("backend %s not found", name)
	}
	return b, nil
}

// -------------------------------------------------------------------------
// DRAIN STATE
// -------------------------------------------------------------------------

// IsDraining returns true if the named backend is currently being drained.
// Returns false when no drain manager is wired (e.g. early in test
// fixtures that build a backendCore without a manager).
func (c *backendCore) IsDraining(name string) bool {
	if c.drainMgr == nil {
		return false
	}
	return c.drainMgr.IsDraining(name)
}

// ExcludeDraining filters out backends that are currently draining.
func (c *backendCore) ExcludeDraining(eligible []string) []string {
	filtered := make([]string, 0, len(eligible))
	for _, name := range eligible {
		if !c.IsDraining(name) {
			filtered = append(filtered, name)
		}
	}
	return filtered
}

// excludeUnhealthy filters out backends whose circuit breaker is open.
// Half-open and probe-eligible backends are allowed through so the circuit
// breaker's probe mechanism can test recovery via organic traffic. Without
// this, all backends tripping simultaneously would deadlock  -  no request
// would ever reach PreCheck to trigger the open -> half-open transition.
func (c *backendCore) excludeUnhealthy(eligible []string) []string {
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

// eligibleForWrite returns backends that are not draining, not circuit-broken,
// and within usage limits for the given operation. Combines excludeDraining,
// excludeUnhealthy, and BackendsWithinLimits into a single pass to avoid
// intermediate slice allocations.
func (c *backendCore) eligibleForWrite(apiCalls, egress, ingress int64) []string {
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

// classifyWriteError translates store errors from write-path operations into
// S3-compatible errors and updates the tracing span. Handles the three common
// cases: database unavailable (503), no space available (507), and generic
// errors. Returns the translated error.
func (c *backendCore) classifyWriteError(span trace.Span, operation string, err error) error {
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

// deleteWithTimeout deletes an object from a backend using the configured
// backend timeout. Returns the backend error directly.
func (c *backendCore) DeleteWithTimeout(ctx context.Context, be backend.ObjectBackend, key string) error {
	dctx, dcancel := c.WithTimeout(ctx)
	defer dcancel()
	return be.DeleteObject(dctx, key)
}

// StreamCopy reads an object from src and writes it to dst using the configured
// backend timeout for each operation. Returns an error tagged with "read:" or
// "write:" to indicate which leg failed.
func (c *backendCore) StreamCopy(ctx context.Context, src, dst backend.ObjectBackend, key string) error {
	rctx, rcancel := c.WithTimeout(ctx)
	defer rcancel()
	result, err := src.GetObject(rctx, key, "")
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	defer func() { _ = result.Body.Close() }()

	wctx, wcancel := c.WithTimeout(ctx)
	defer wcancel()
	_, err = dst.PutObject(wctx, key, result.Body, result.Size, result.ContentType, result.Metadata)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// -------------------------------------------------------------------------
// METRICS
// -------------------------------------------------------------------------

// recordOperation delegates to the MetricsCollector.
func (c *backendCore) recordOperation(operation, backend string, start time.Time, err error) {
	c.metricsCollector.RecordOperation(operation, backend, start, err)
}

// UpdateQuotaMetrics refreshes Prometheus gauges from the metadata store.
func (c *backendCore) UpdateQuotaMetrics(ctx context.Context) error {
	return c.metricsCollector.UpdateQuotaMetrics(ctx)
}

// -------------------------------------------------------------------------
// worker.Ops IMPLEMENTATION
// -------------------------------------------------------------------------

// Backends returns the backend map.
func (c *backendCore) Backends() map[string]backend.ObjectBackend { return c.backends }

// BackendOrder returns the configured backend ordering.
func (c *backendCore) BackendOrder() []string { return c.order }

// Usage returns the usage tracker.
func (c *backendCore) Usage() *counter.UsageTracker { return c.usage }
