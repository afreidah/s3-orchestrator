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
	"log/slog"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/store"

	"github.com/afreidah/s3-orchestrator/internal/audit"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// backendCore holds the shared infrastructure that multiple storage components
// need: the backend map, metadata store, usage tracker, and common utilities.
type backendCore struct {
	backends        map[string]backend.ObjectBackend // name -> backend
	store           store.MetadataStore              // metadata persistence (Store or CircuitBreakerStore)
	order           []string                         // backend selection order
	backendTimeout  time.Duration                    // per-operation timeout for backend S3 calls
	usage           *counter.UsageTracker            // per-backend usage counters and limits
	routingStrategy string                           // "pack" or "spread"
	draining        sync.Map                         // map[string]*drainState — backends being drained
	metrics         *MetricsCollector                // Prometheus metric recording and gauge refresh
	admissionSem    chan struct{}                    // shared concurrency semaphore (nil = unlimited)
}

// -------------------------------------------------------------------------
// ADMISSION
// -------------------------------------------------------------------------

// acquireAdmission blocks until a slot is available in the shared admission
// semaphore, or returns false if ctx is cancelled. Returns true immediately
// when no semaphore is configured.
func (c *backendCore) acquireAdmission(ctx context.Context) bool {
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

// releaseAdmission returns a slot to the admission semaphore. No-op when
// no semaphore is configured.
func (c *backendCore) releaseAdmission() {
	if c.admissionSem == nil {
		return
	}
	<-c.admissionSem
}

// -------------------------------------------------------------------------
// TIMEOUT
// -------------------------------------------------------------------------

// withTimeout returns a context with the configured backend timeout applied.
// If the parent context already has a tighter deadline, the parent deadline
// is preserved to avoid masking upstream timeouts (e.g. HTTP WriteTimeout).
// If no timeout is configured, the original context is returned unchanged.
func (c *backendCore) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.backendTimeout <= 0 {
		return ctx, func() {}
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
// Exported for use by the admin handler (encrypt-existing endpoint).
func (c *backendCore) GetBackend(name string) (backend.ObjectBackend, error) {
	return c.getBackend(name)
}

// getBackend returns the named backend, or an error if it doesn't exist.
func (c *backendCore) getBackend(name string) (backend.ObjectBackend, error) {
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
func (c *backendCore) IsDraining(name string) bool {
	_, ok := c.draining.Load(name)
	return ok
}

// excludeDraining filters out backends that are currently draining.
func (c *backendCore) excludeDraining(eligible []string) []string {
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
// this, all backends tripping simultaneously would deadlock — no request
// would ever reach PreCheck to trigger the open → half-open transition.
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

// -------------------------------------------------------------------------
// ROUTING
// -------------------------------------------------------------------------

// selectBackendForWrite picks the target backend for a write operation using
// the configured routing strategy. "pack" returns the first backend with space,
// "spread" returns the least-utilized backend.
func (c *backendCore) selectBackendForWrite(ctx context.Context, size int64, eligible []string) (string, error) {
	if c.routingStrategy == "spread" {
		return c.store.GetLeastUtilizedBackend(ctx, size, eligible)
	}
	return c.store.GetBackendWithSpace(ctx, size, eligible)
}

// -------------------------------------------------------------------------
// ERROR CLASSIFICATION
// -------------------------------------------------------------------------

// classifyWriteError translates store errors from write-path operations into
// S3-compatible errors and updates the tracing span. Handles the three common
// cases: database unavailable (503), no space available (507), and generic
// errors. Returns the translated error.
func (c *backendCore) classifyWriteError(span trace.Span, operation string, err error) error {
	if errors.Is(err, store.ErrDBUnavailable) {
		span.SetStatus(codes.Error, "database unavailable")
		telemetry.DegradedWriteRejectionsTotal.WithLabelValues(operation).Inc()
		return store.ErrServiceUnavailable
	}
	if errors.Is(err, store.ErrNoSpaceAvailable) {
		span.SetStatus(codes.Error, "insufficient storage")
		return store.ErrInsufficientStorage
	}
	span.SetStatus(codes.Error, err.Error())
	span.RecordError(err)
	return err
}

// -------------------------------------------------------------------------
// RECORD + CLEANUP
// -------------------------------------------------------------------------

// recordObjectOrCleanup calls RecordObject and, on failure, deletes the orphaned
// object from the backend. On success, enqueues cleanup for any displaced copies
// on other backends (from overwrites). Updates the tracing span on error.
func (c *backendCore) recordObjectOrCleanup(ctx context.Context, span trace.Span, backend backend.ObjectBackend, key, backendName string, size int64, enc *store.EncryptionMeta) error {
	displaced, err := c.store.RecordObject(ctx, key, backendName, size, enc)
	if err != nil {
		slog.ErrorContext(ctx, "RecordObject failed, cleaning up orphan",
			"key", key, "backend", backendName, "error", err)
		c.usage.Record(backendName, 1, 0, 0)
		if delErr := backend.DeleteObject(ctx, key); delErr != nil {
			slog.ErrorContext(ctx, "Failed to clean up orphaned object",
				"key", key, "backend", backendName, "error", delErr)
			c.enqueueCleanup(ctx, backendName, key, "orphan_record_failed", size)
		}
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return fmt.Errorf("failed to record object: %w", err)
	}

	// Clean up stale copies on other backends displaced by this overwrite.
	for _, dc := range displaced {
		dcBackend, ok := c.backends[dc.BackendName]
		if !ok {
			slog.WarnContext(ctx, "Displaced copy backend not found",
				"backend", dc.BackendName, "key", key)
			continue
		}
		c.deleteOrEnqueue(ctx, dcBackend, dc.BackendName, key, "overwrite_displaced", dc.SizeBytes)
	}

	if len(displaced) > 0 {
		audit.Log(ctx, "storage.overwrite_displaced",
			slog.String("key", key),
			slog.String("new_backend", backendName),
			slog.Int("displaced_copies", len(displaced)),
		)
	}

	return nil
}

// deleteWithTimeout deletes an object from a backend using the configured
// backend timeout. Returns the backend error directly.
func (c *backendCore) deleteWithTimeout(ctx context.Context, backend backend.ObjectBackend, key string) error {
	dctx, dcancel := c.withTimeout(ctx)
	defer dcancel()
	return backend.DeleteObject(dctx, key)
}

// streamCopy reads an object from src and writes it to dst using the configured
// backend timeout for each operation. Returns an error tagged with "read:" or
// "write:" to indicate which leg failed.
func (c *backendCore) streamCopy(ctx context.Context, src, dst backend.ObjectBackend, key string) error {
	rctx, rcancel := c.withTimeout(ctx)
	result, err := src.GetObject(rctx, key, "")
	if err != nil {
		rcancel()
		return fmt.Errorf("read: %w", err)
	}

	wctx, wcancel := c.withTimeout(ctx)
	_, err = dst.PutObject(wctx, key, result.Body, result.Size, result.ContentType, result.Metadata)
	_ = result.Body.Close()
	rcancel()
	wcancel()
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// deleteOrEnqueue attempts to delete an object from a backend. If the delete
// fails, it logs a warning and enqueues the key for background retry. This is
// the standard "best-effort orphan cleanup" primitive used throughout the
// manager: rebalancer, replicator, multipart cleanup, and delete paths.
// sizeBytes is tracked as orphan bytes when the delete is enqueued.
func (c *backendCore) deleteOrEnqueue(ctx context.Context, backend backend.ObjectBackend, backendName, key, reason string, sizeBytes int64) {
	if err := c.deleteWithTimeout(ctx, backend, key); err != nil {
		slog.WarnContext(ctx, "Failed to delete object, enqueuing cleanup",
			"backend", backendName, "key", key, "reason", reason, "error", err)
		c.enqueueCleanup(ctx, backendName, key, reason, sizeBytes)
	}
}

// -------------------------------------------------------------------------
// METRICS
// -------------------------------------------------------------------------

// recordOperation delegates to the MetricsCollector.
func (c *backendCore) recordOperation(operation, backend string, start time.Time, err error) {
	c.metrics.RecordOperation(operation, backend, start, err)
}

// UpdateQuotaMetrics refreshes Prometheus gauges from the metadata store.
func (c *backendCore) UpdateQuotaMetrics(ctx context.Context) error {
	return c.metrics.UpdateQuotaMetrics(ctx)
}

// enqueueCleanup adds a failed cleanup operation to the retry queue and
// increments orphan_bytes so the write path accounts for the physically
// unreleased space. Best-effort: if the enqueue or orphan update fails
// (e.g. DB down), logs the error and moves on since the circuit breaker
// is already handling DB outages.
func (c *backendCore) enqueueCleanup(ctx context.Context, backendName, objectKey, reason string, sizeBytes int64) {
	if err := c.store.EnqueueCleanup(ctx, backendName, objectKey, reason, sizeBytes); err != nil {
		slog.ErrorContext(ctx, "Failed to enqueue cleanup (best-effort)",
			"backend", backendName, "key", objectKey, "reason", reason, "error", err)
		return
	}
	if sizeBytes > 0 {
		if err := c.store.IncrementOrphanBytes(ctx, backendName, sizeBytes); err != nil {
			slog.ErrorContext(ctx, "Failed to increment orphan bytes (best-effort)",
				"backend", backendName, "key", objectKey, "size", sizeBytes, "error", err)
		}
	}
	telemetry.CleanupQueueEnqueuedTotal.WithLabelValues(reason).Inc()
}

// -------------------------------------------------------------------------
// worker.Ops IMPLEMENTATION
// -------------------------------------------------------------------------

// AcquireAdmission blocks until a slot is available in the shared admission semaphore.
func (c *backendCore) AcquireAdmission(ctx context.Context) bool { return c.acquireAdmission(ctx) }

// ReleaseAdmission returns a slot to the admission semaphore.
func (c *backendCore) ReleaseAdmission() { c.releaseAdmission() }

// WithTimeout returns a context with the configured backend timeout applied.
func (c *backendCore) WithTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return c.withTimeout(ctx)
}

// Backends returns the backend map.
func (c *backendCore) Backends() map[string]backend.ObjectBackend { return c.backends }

// BackendOrder returns the configured backend ordering.
func (c *backendCore) BackendOrder() []string { return c.order }

// Store returns the metadata store.
func (c *backendCore) Store() store.MetadataStore { return c.store }

// Usage returns the usage tracker.
func (c *backendCore) Usage() *counter.UsageTracker { return c.usage }

// StreamCopy pipes a GetObject→PutObject between two backends.
func (c *backendCore) StreamCopy(ctx context.Context, src, dst backend.ObjectBackend, key string) error {
	return c.streamCopy(ctx, src, dst, key)
}

// DeleteWithTimeout deletes an object from a backend with the configured timeout.
func (c *backendCore) DeleteWithTimeout(ctx context.Context, be backend.ObjectBackend, key string) error {
	return c.deleteWithTimeout(ctx, be, key)
}

// DeleteOrEnqueue deletes an object, enqueueing for retry on failure.
func (c *backendCore) DeleteOrEnqueue(ctx context.Context, be backend.ObjectBackend, backendName, key, reason string, sizeBytes int64) {
	c.deleteOrEnqueue(ctx, be, backendName, key, reason, sizeBytes)
}

// RecordOperation records a backend operation for metrics.
func (c *backendCore) RecordOperation(operation, backendName string, start time.Time, err error) {
	c.recordOperation(operation, backendName, start, err)
}

// ExcludeDraining filters out backends that are being drained.
func (c *backendCore) ExcludeDraining(eligible []string) []string { return c.excludeDraining(eligible) }
