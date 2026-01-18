// -------------------------------------------------------------------------------
// Manager - Multi-Backend Object Storage Manager
//
// Project: Munchbox / Author: Alex Freidah
//
// Manages multiple S3-compatible backends with quota-aware routing. Implements the
// ObjectBackend interface so it can be used as a drop-in replacement for a single
// backend. Automatically selects backends based on available quota and routes
// GET/HEAD/DELETE requests to the correct backend.
// -------------------------------------------------------------------------------

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// -------------------------------------------------------------------------
// ERRORS
// -------------------------------------------------------------------------

var (
	// ErrInsufficientStorage is returned when no backend has enough quota.
	ErrInsufficientStorage = errors.New("insufficient storage: all backends at capacity")
)

// -------------------------------------------------------------------------
// BACKEND MANAGER
// -------------------------------------------------------------------------

// BackendManager manages multiple storage backends with quota tracking.
type BackendManager struct {
	backends map[string]*S3Backend // name -> backend
	store    *Store                // PostgreSQL store for quota/location
	order    []string              // backend selection order
}

// NewBackendManager creates a new backend manager with the given backends and store.
func NewBackendManager(backends map[string]*S3Backend, store *Store, order []string) *BackendManager {
	return &BackendManager{
		backends: backends,
		store:    store,
		order:    order,
	}
}

// -------------------------------------------------------------------------
// OBJECT BACKEND INTERFACE IMPLEMENTATION
// -------------------------------------------------------------------------

// PutObject uploads an object to the first backend with available quota.
func (m *BackendManager) PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string) (string, error) {
	const operation = "PutObject"
	start := time.Now()

	// --- Start tracing span ---
	ctx, span := StartSpan(ctx, "Manager "+operation,
		AttrObjectKey.String(key),
		AttrObjectSize.Int64(size),
	)
	defer span.End()

	// --- Find backend with available quota ---
	backendName, err := m.store.GetBackendWithSpace(ctx, size, m.order)
	if err != nil {
		if errors.Is(err, ErrNoSpaceAvailable) {
			span.SetStatus(codes.Error, "insufficient storage")
			span.SetAttributes(attribute.String("error.type", "quota_exceeded"))
			return "", ErrInsufficientStorage
		}
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return "", fmt.Errorf("failed to find backend: %w", err)
	}

	span.SetAttributes(AttrBackendName.String(backendName))

	// --- Get the backend ---
	backend, ok := m.backends[backendName]
	if !ok {
		err := fmt.Errorf("backend %s not found", backendName)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}

	// --- Upload to backend ---
	etag, err := backend.PutObject(ctx, key, body, size, contentType)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return "", err
	}

	// --- Record object location and update quota ---
	if err := m.store.RecordObject(ctx, key, backendName, size); err != nil {
		// Object was uploaded but we failed to record it
		// This is a critical error - log it but still return success since data is stored
		span.SetAttributes(attribute.Bool("quota.record_failed", true))
		span.RecordError(err)
		// TODO: Consider cleanup or retry logic
	}

	// --- Record metrics ---
	m.recordOperation(operation, backendName, start, nil)

	span.SetStatus(codes.Ok, "")
	return etag, nil
}

// GetObject retrieves an object from the backend where it's stored.
func (m *BackendManager) GetObject(ctx context.Context, key string) (io.ReadCloser, int64, string, string, error) {
	const operation = "GetObject"
	start := time.Now()

	// --- Start tracing span ---
	ctx, span := StartSpan(ctx, "Manager "+operation,
		AttrObjectKey.String(key),
	)
	defer span.End()

	// --- Find object location ---
	loc, err := m.store.GetObjectLocation(ctx, key)
	if err != nil {
		if errors.Is(err, ErrObjectNotFound) {
			span.SetStatus(codes.Error, "object not found")
			return nil, 0, "", "", err
		}
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return nil, 0, "", "", fmt.Errorf("failed to find object location: %w", err)
	}

	span.SetAttributes(AttrBackendName.String(loc.BackendName))

	// --- Get the backend ---
	backend, ok := m.backends[loc.BackendName]
	if !ok {
		err := fmt.Errorf("backend %s not found", loc.BackendName)
		span.SetStatus(codes.Error, err.Error())
		return nil, 0, "", "", err
	}

	// --- Get from backend ---
	body, size, contentType, etag, err := backend.GetObject(ctx, key)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return nil, 0, "", "", err
	}

	// --- Record metrics ---
	m.recordOperation(operation, loc.BackendName, start, nil)

	span.SetAttributes(AttrObjectSize.Int64(size))
	span.SetStatus(codes.Ok, "")
	return body, size, contentType, etag, nil
}

// HeadObject retrieves object metadata from the backend where it's stored.
func (m *BackendManager) HeadObject(ctx context.Context, key string) (int64, string, string, error) {
	const operation = "HeadObject"
	start := time.Now()

	// --- Start tracing span ---
	ctx, span := StartSpan(ctx, "Manager "+operation,
		AttrObjectKey.String(key),
	)
	defer span.End()

	// --- Find object location ---
	loc, err := m.store.GetObjectLocation(ctx, key)
	if err != nil {
		if errors.Is(err, ErrObjectNotFound) {
			span.SetStatus(codes.Error, "object not found")
			return 0, "", "", err
		}
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return 0, "", "", fmt.Errorf("failed to find object location: %w", err)
	}

	span.SetAttributes(AttrBackendName.String(loc.BackendName))

	// --- Get the backend ---
	backend, ok := m.backends[loc.BackendName]
	if !ok {
		err := fmt.Errorf("backend %s not found", loc.BackendName)
		span.SetStatus(codes.Error, err.Error())
		return 0, "", "", err
	}

	// --- Head from backend ---
	size, contentType, etag, err := backend.HeadObject(ctx, key)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return 0, "", "", err
	}

	// --- Record metrics ---
	m.recordOperation(operation, loc.BackendName, start, nil)

	span.SetAttributes(AttrObjectSize.Int64(size))
	span.SetStatus(codes.Ok, "")
	return size, contentType, etag, nil
}

// DeleteObject removes an object from the backend where it's stored.
func (m *BackendManager) DeleteObject(ctx context.Context, key string) error {
	const operation = "DeleteObject"
	start := time.Now()

	// --- Start tracing span ---
	ctx, span := StartSpan(ctx, "Manager "+operation,
		AttrObjectKey.String(key),
	)
	defer span.End()

	// --- Delete from store (get location first) ---
	backendName, size, err := m.store.DeleteObject(ctx, key)
	if err != nil {
		if errors.Is(err, ErrObjectNotFound) {
			// Object not in our tracking - treat as success (idempotent delete)
			span.SetStatus(codes.Ok, "object not found - treating as success")
			return nil
		}
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return fmt.Errorf("failed to delete object record: %w", err)
	}

	span.SetAttributes(
		AttrBackendName.String(backendName),
		AttrObjectSize.Int64(size),
	)

	// --- Get the backend ---
	backend, ok := m.backends[backendName]
	if !ok {
		err := fmt.Errorf("backend %s not found", backendName)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	// --- Delete from backend ---
	if err := backend.DeleteObject(ctx, key); err != nil {
		// Log error but don't fail - quota is already updated
		span.RecordError(err)
		span.SetAttributes(attribute.Bool("backend.delete_failed", true))
	}

	// --- Record metrics ---
	m.recordOperation(operation, backendName, start, nil)

	span.SetStatus(codes.Ok, "")
	return nil
}

// -------------------------------------------------------------------------
// METRICS
// -------------------------------------------------------------------------

// recordOperation updates Prometheus metrics for a manager operation.
func (m *BackendManager) recordOperation(operation, backend string, start time.Time, err error) {
	status := "success"
	if err != nil {
		status = "error"
	}

	ManagerRequestsTotal.WithLabelValues(operation, backend, status).Inc()
	ManagerDuration.WithLabelValues(operation, backend).Observe(time.Since(start).Seconds())
}

// -------------------------------------------------------------------------
// QUOTA STATS
// -------------------------------------------------------------------------

// UpdateQuotaMetrics fetches quota stats and updates Prometheus gauges.
func (m *BackendManager) UpdateQuotaMetrics(ctx context.Context) error {
	stats, err := m.store.GetQuotaStats(ctx)
	if err != nil {
		return err
	}

	for name, stat := range stats {
		QuotaBytesUsed.WithLabelValues(name).Set(float64(stat.BytesUsed))
		QuotaBytesLimit.WithLabelValues(name).Set(float64(stat.BytesLimit))
		QuotaBytesAvailable.WithLabelValues(name).Set(float64(stat.BytesLimit - stat.BytesUsed))
	}

	return nil
}
