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

package storage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/telemetry"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// backendCore holds the shared infrastructure that multiple storage components
// need: the backend map, metadata store, usage tracker, and common utilities.
type backendCore struct {
	backends        map[string]ObjectBackend // name -> backend
	store           MetadataStore            // metadata persistence (Store or CircuitBreakerStore)
	order           []string                 // backend selection order
	backendTimeout  time.Duration            // per-operation timeout for backend S3 calls
	usage           *UsageTracker            // per-backend usage counters and limits
	routingStrategy string                   // "pack" or "spread"
	draining        sync.Map                 // map[string]*drainState — backends being drained
	metrics         *MetricsCollector        // Prometheus metric recording and gauge refresh
}

// -------------------------------------------------------------------------
// TIMEOUT
// -------------------------------------------------------------------------

// withTimeout returns a context with the configured backend timeout applied.
// If no timeout is configured, the original context is returned unchanged.
func (c *backendCore) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.backendTimeout > 0 {
		return context.WithTimeout(ctx, c.backendTimeout)
	}
	return ctx, func() {}
}

// -------------------------------------------------------------------------
// BACKEND LOOKUP
// -------------------------------------------------------------------------

// GetBackend returns the named backend, or an error if it doesn't exist.
// Exported for use by the admin handler (encrypt-existing endpoint).
func (c *backendCore) GetBackend(name string) (ObjectBackend, error) {
	return c.getBackend(name)
}

// getBackend returns the named backend, or an error if it doesn't exist.
func (c *backendCore) getBackend(name string) (ObjectBackend, error) {
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
// Half-open backends are allowed through so the circuit breaker's probe
// mechanism can test recovery via organic traffic.
func (c *backendCore) excludeUnhealthy(eligible []string) []string {
	filtered := make([]string, 0, len(eligible))
	for _, name := range eligible {
		b, ok := c.backends[name]
		if !ok {
			continue
		}
		if cb, ok := b.(*CircuitBreakerBackend); ok && cb.State() == stateOpen {
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
	if errors.Is(err, ErrDBUnavailable) {
		span.SetStatus(codes.Error, "database unavailable")
		telemetry.DegradedWriteRejectionsTotal.WithLabelValues(operation).Inc()
		return ErrServiceUnavailable
	}
	if errors.Is(err, ErrNoSpaceAvailable) {
		span.SetStatus(codes.Error, "insufficient storage")
		return ErrInsufficientStorage
	}
	span.SetStatus(codes.Error, err.Error())
	span.RecordError(err)
	return err
}

// -------------------------------------------------------------------------
// RECORD + CLEANUP
// -------------------------------------------------------------------------

// recordObjectOrCleanup calls RecordObject and, on failure, deletes the orphaned
// object from the backend. Updates the tracing span on error.
func (c *backendCore) recordObjectOrCleanup(ctx context.Context, span trace.Span, backend ObjectBackend, key, backendName string, size int64, enc *EncryptionMeta) error {
	if err := c.store.RecordObject(ctx, key, backendName, size, enc); err != nil {
		slog.Error("RecordObject failed, cleaning up orphan",
			"key", key, "backend", backendName, "error", err)
		c.usage.Record(backendName, 1, 0, 0)
		if delErr := backend.DeleteObject(ctx, key); delErr != nil {
			slog.Error("Failed to clean up orphaned object",
				"key", key, "backend", backendName, "error", delErr)
			c.enqueueCleanup(ctx, backendName, key, "orphan_record_failed")
		}
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return fmt.Errorf("failed to record object: %w", err)
	}
	return nil
}

// deleteWithTimeout deletes an object from a backend using the configured
// backend timeout. Returns the backend error directly.
func (c *backendCore) deleteWithTimeout(ctx context.Context, backend ObjectBackend, key string) error {
	dctx, dcancel := c.withTimeout(ctx)
	defer dcancel()
	return backend.DeleteObject(dctx, key)
}

// streamCopy reads an object from src and writes it to dst using the configured
// backend timeout for each operation. Returns an error tagged with "read:" or
// "write:" to indicate which leg failed.
func (c *backendCore) streamCopy(ctx context.Context, src, dst ObjectBackend, key string) error {
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
func (c *backendCore) deleteOrEnqueue(ctx context.Context, backend ObjectBackend, backendName, key, reason string) {
	if err := c.deleteWithTimeout(ctx, backend, key); err != nil {
		slog.Warn("Failed to delete object, enqueuing cleanup",
			"backend", backendName, "key", key, "reason", reason, "error", err)
		c.enqueueCleanup(ctx, backendName, key, reason)
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

// enqueueCleanup adds a failed cleanup operation to the retry queue.
// Best-effort: if the enqueue itself fails (e.g. DB down), logs the error
// and moves on since the circuit breaker is already handling DB outages.
func (c *backendCore) enqueueCleanup(ctx context.Context, backendName, objectKey, reason string) {
	if err := c.store.EnqueueCleanup(ctx, backendName, objectKey, reason); err != nil {
		slog.Error("Failed to enqueue cleanup (best-effort)",
			"backend", backendName, "key", objectKey, "reason", reason, "error", err)
		return
	}
	telemetry.CleanupQueueEnqueuedTotal.WithLabelValues(reason).Inc()
}
