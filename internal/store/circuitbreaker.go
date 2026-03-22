// -------------------------------------------------------------------------------
// CircuitBreakerStore - Self-Healing Database Degradation Wrapper
//
// Author: Alex Freidah
//
// Wraps a MetadataStore with a three-state circuit breaker that detects database
// outages and returns ErrDBUnavailable when the circuit is open. The manager
// uses this sentinel to trigger broadcast read fallback or reject writes with
// 503. When the database recovers, the circuit auto-closes.
//
// The state machine logic lives in CircuitBreaker (circuitbreaker_core.go).
// This file provides the DB-specific error filter and the MetadataStore
// forwarding methods.
// -------------------------------------------------------------------------------

package store

import (
	"context"
	"errors"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/breaker"
)

// -------------------------------------------------------------------------
// CIRCUIT BREAKER STORE
// -------------------------------------------------------------------------

// CircuitBreakerStore implements MetadataStore by wrapping a real store with
// circuit breaker logic. When the database is unreachable, it returns
// ErrDBUnavailable instead of passing through to the real store.
type CircuitBreakerStore struct {
	real MetadataStore
	*breaker.CircuitBreaker
}

// Compile-time check.
var _ MetadataStore = (*CircuitBreakerStore)(nil)

// NewCircuitBreakerStore wraps a real MetadataStore with circuit breaker logic.
func NewCircuitBreakerStore(real MetadataStore, cfg config.CircuitBreakerConfig) *CircuitBreakerStore {
	return &CircuitBreakerStore{
		real:           real,
		CircuitBreaker: breaker.NewCircuitBreaker("database", cfg.FailureThreshold, cfg.OpenTimeout, isDBError, ErrDBUnavailable),
	}
}

// -------------------------------------------------------------------------
// DB-SPECIFIC ERROR FILTER
// -------------------------------------------------------------------------

// isDBError returns true for genuine database failures. Application-level
// errors (S3Error, ErrNoSpaceAvailable) do not trip the circuit breaker.
func isDBError(err error) bool {
	if err == nil {
		return false
	}
	var s3err *S3Error
	if errors.As(err, &s3err) {
		return false
	}
	if errors.Is(err, ErrNoSpaceAvailable) {
		return false
	}
	return true
}

// -------------------------------------------------------------------------
// FORWARDING METHODS
// -------------------------------------------------------------------------

// GetAllObjectLocations delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) GetAllObjectLocations(ctx context.Context, key string) ([]ObjectLocation, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() ([]ObjectLocation, error) { return cb.real.GetAllObjectLocations(ctx, key) })
}

// RecordObject delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) RecordObject(ctx context.Context, key, backend string, size int64, enc *EncryptionMeta) ([]DeletedCopy, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() ([]DeletedCopy, error) { return cb.real.RecordObject(ctx, key, backend, size, enc) })
}

// DeleteObject delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) DeleteObject(ctx context.Context, key string) ([]DeletedCopy, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() ([]DeletedCopy, error) { return cb.real.DeleteObject(ctx, key) })
}

// ListObjects delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) ListObjects(ctx context.Context, prefix, startAfter string, maxKeys int) (*ListObjectsResult, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() (*ListObjectsResult, error) { return cb.real.ListObjects(ctx, prefix, startAfter, maxKeys) })
}

// ListDirectoryChildren delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) ListDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*DirectoryListResult, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() (*DirectoryListResult, error) {
		return cb.real.ListDirectoryChildren(ctx, prefix, startAfter, maxKeys)
	})
}

// GetBackendWithSpace delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) GetBackendWithSpace(ctx context.Context, size int64, backendOrder []string) (string, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() (string, error) { return cb.real.GetBackendWithSpace(ctx, size, backendOrder) })
}

// GetLeastUtilizedBackend delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) GetLeastUtilizedBackend(ctx context.Context, size int64, eligible []string) (string, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() (string, error) { return cb.real.GetLeastUtilizedBackend(ctx, size, eligible) })
}

// CreateMultipartUpload delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) CreateMultipartUpload(ctx context.Context, uploadID, key, backend, contentType string, metadata map[string]string) error {
	return breaker.CBCallNoResult(cb.CircuitBreaker, func() error {
		return cb.real.CreateMultipartUpload(ctx, uploadID, key, backend, contentType, metadata)
	})
}

// GetMultipartUpload delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) GetMultipartUpload(ctx context.Context, uploadID string) (*MultipartUpload, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() (*MultipartUpload, error) { return cb.real.GetMultipartUpload(ctx, uploadID) })
}

// RecordPart delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) RecordPart(ctx context.Context, uploadID string, partNumber int, etag string, size int64, enc *EncryptionMeta) error {
	return breaker.CBCallNoResult(cb.CircuitBreaker, func() error { return cb.real.RecordPart(ctx, uploadID, partNumber, etag, size, enc) })
}

// GetParts delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) GetParts(ctx context.Context, uploadID string) ([]MultipartPart, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() ([]MultipartPart, error) { return cb.real.GetParts(ctx, uploadID) })
}

// DeleteMultipartUpload delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) DeleteMultipartUpload(ctx context.Context, uploadID string) error {
	return breaker.CBCallNoResult(cb.CircuitBreaker, func() error { return cb.real.DeleteMultipartUpload(ctx, uploadID) })
}

// ListExpiredObjects delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) ListExpiredObjects(ctx context.Context, prefix string, cutoff time.Time, limit int) ([]ObjectLocation, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() ([]ObjectLocation, error) { return cb.real.ListExpiredObjects(ctx, prefix, cutoff, limit) })
}

// GetQuotaStats delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) GetQuotaStats(ctx context.Context) (map[string]QuotaStat, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() (map[string]QuotaStat, error) { return cb.real.GetQuotaStats(ctx) })
}

// GetObjectCounts delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) GetObjectCounts(ctx context.Context) (map[string]int64, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() (map[string]int64, error) { return cb.real.GetObjectCounts(ctx) })
}

// GetActiveMultipartCounts delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) GetActiveMultipartCounts(ctx context.Context) (map[string]int64, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() (map[string]int64, error) { return cb.real.GetActiveMultipartCounts(ctx) })
}

// GetStaleMultipartUploads delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) GetStaleMultipartUploads(ctx context.Context, olderThan time.Duration) ([]MultipartUpload, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() ([]MultipartUpload, error) { return cb.real.GetStaleMultipartUploads(ctx, olderThan) })
}

// GetMultipartUploadsByBackend delegates to the real store with circuit breaker protection.
// codecov:ignore:start -- thin CB wrapper, covered by integration tests
func (cb *CircuitBreakerStore) GetMultipartUploadsByBackend(ctx context.Context, backendName string) ([]MultipartUpload, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() ([]MultipartUpload, error) { return cb.real.GetMultipartUploadsByBackend(ctx, backendName) })
}

// codecov:ignore:end

// CountActiveMultipartUploads delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) CountActiveMultipartUploads(ctx context.Context, bucketPrefix string) (int64, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() (int64, error) { return cb.real.CountActiveMultipartUploads(ctx, bucketPrefix) })
}

// ListMultipartUploads delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) ListMultipartUploads(ctx context.Context, prefix string, maxUploads int) ([]MultipartUpload, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() ([]MultipartUpload, error) { return cb.real.ListMultipartUploads(ctx, prefix, maxUploads) })
}

// ListObjectsByBackend delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) ListObjectsByBackend(ctx context.Context, backendName string, limit int) ([]ObjectLocation, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() ([]ObjectLocation, error) { return cb.real.ListObjectsByBackend(ctx, backendName, limit) })
}

// MoveObjectLocation delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) MoveObjectLocation(ctx context.Context, key, fromBackend, toBackend string) (int64, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() (int64, error) { return cb.real.MoveObjectLocation(ctx, key, fromBackend, toBackend) })
}

// GetUnderReplicatedObjects delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) GetUnderReplicatedObjects(ctx context.Context, factor, limit int) ([]ObjectLocation, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() ([]ObjectLocation, error) { return cb.real.GetUnderReplicatedObjects(ctx, factor, limit) })
}

// GetUnderReplicatedObjectsExcluding delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) GetUnderReplicatedObjectsExcluding(ctx context.Context, factor, limit int, excludedBackends []string) ([]ObjectLocation, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() ([]ObjectLocation, error) {
		return cb.real.GetUnderReplicatedObjectsExcluding(ctx, factor, limit, excludedBackends)
	})
}

// RecordReplica delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) RecordReplica(ctx context.Context, key, targetBackend, sourceBackend string, size int64) (bool, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() (bool, error) { return cb.real.RecordReplica(ctx, key, targetBackend, sourceBackend, size) })
}

// GetOverReplicatedObjects delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) GetOverReplicatedObjects(ctx context.Context, factor, limit int) ([]ObjectLocation, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() ([]ObjectLocation, error) { return cb.real.GetOverReplicatedObjects(ctx, factor, limit) })
}

// CountOverReplicatedObjects delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) CountOverReplicatedObjects(ctx context.Context, factor int) (int64, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() (int64, error) { return cb.real.CountOverReplicatedObjects(ctx, factor) })
}

// RemoveExcessCopy delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) RemoveExcessCopy(ctx context.Context, key, backendName string, size int64) error {
	return breaker.CBCallNoResult(cb.CircuitBreaker, func() error { return cb.real.RemoveExcessCopy(ctx, key, backendName, size) })
}

// FlushUsageDeltas delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) FlushUsageDeltas(ctx context.Context, backendName, period string, apiRequests, egressBytes, ingressBytes int64) error {
	return breaker.CBCallNoResult(cb.CircuitBreaker, func() error {
		return cb.real.FlushUsageDeltas(ctx, backendName, period, apiRequests, egressBytes, ingressBytes)
	})
}

// GetUsageForPeriod delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) GetUsageForPeriod(ctx context.Context, period string) (map[string]UsageStat, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() (map[string]UsageStat, error) { return cb.real.GetUsageForPeriod(ctx, period) })
}

// EnqueueCleanup delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) EnqueueCleanup(ctx context.Context, backendName, objectKey, reason string, sizeBytes int64) error {
	return breaker.CBCallNoResult(cb.CircuitBreaker, func() error {
		return cb.real.EnqueueCleanup(ctx, backendName, objectKey, reason, sizeBytes)
	})
}

// IncrementOrphanBytes delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) IncrementOrphanBytes(ctx context.Context, backendName string, amount int64) error {
	return breaker.CBCallNoResult(cb.CircuitBreaker, func() error { return cb.real.IncrementOrphanBytes(ctx, backendName, amount) })
}

// DecrementOrphanBytes delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) DecrementOrphanBytes(ctx context.Context, backendName string, amount int64) error {
	return breaker.CBCallNoResult(cb.CircuitBreaker, func() error { return cb.real.DecrementOrphanBytes(ctx, backendName, amount) })
}

// GetPendingCleanups delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) GetPendingCleanups(ctx context.Context, limit int) ([]CleanupItem, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() ([]CleanupItem, error) { return cb.real.GetPendingCleanups(ctx, limit) })
}

// CompleteCleanupItem delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) CompleteCleanupItem(ctx context.Context, id int64) error {
	return breaker.CBCallNoResult(cb.CircuitBreaker, func() error { return cb.real.CompleteCleanupItem(ctx, id) })
}

// RetryCleanupItem delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) RetryCleanupItem(ctx context.Context, id int64, backoff time.Duration, lastError string) error {
	return breaker.CBCallNoResult(cb.CircuitBreaker, func() error { return cb.real.RetryCleanupItem(ctx, id, backoff, lastError) })
}

// CleanupQueueDepth delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) CleanupQueueDepth(ctx context.Context) (int64, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() (int64, error) { return cb.real.CleanupQueueDepth(ctx) })
}

// WithAdvisoryLock delegates directly to the underlying store without circuit
// breaker wrapping. Advisory locks are coordination-only; if the DB is down
// the lock attempt fails naturally and the task is skipped.
func (cb *CircuitBreakerStore) WithAdvisoryLock(ctx context.Context, lockID int64, fn func(ctx context.Context) error) (bool, error) {
	return cb.real.WithAdvisoryLock(ctx, lockID, fn)
}

// ImportObject delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) ImportObject(ctx context.Context, key, backend string, size int64) (bool, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() (bool, error) { return cb.real.ImportObject(ctx, key, backend, size) })
}

// BackendObjectStats delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) BackendObjectStats(ctx context.Context, backendName string) (int64, int64, error) {
	if err := cb.PreCheck(); err != nil {
		return 0, 0, err
	}
	count, bytes, err := cb.real.BackendObjectStats(ctx, backendName)
	return count, bytes, cb.PostCheck(err)
}

// DeleteBackendData delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) DeleteBackendData(ctx context.Context, backendName string) error {
	return breaker.CBCallNoResult(cb.CircuitBreaker, func() error { return cb.real.DeleteBackendData(ctx, backendName) })
}

// DeleteObjectLocation delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) DeleteObjectLocation(ctx context.Context, key, backendName string) error {
	return breaker.CBCallNoResult(cb.CircuitBreaker, func() error { return cb.real.DeleteObjectLocation(ctx, key, backendName) })
}

// GetRandomHashedObjects delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) GetRandomHashedObjects(ctx context.Context, limit int) ([]ObjectLocation, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() ([]ObjectLocation, error) { return cb.real.GetRandomHashedObjects(ctx, limit) })
}

// GetObjectsWithoutHash delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) GetObjectsWithoutHash(ctx context.Context, limit, offset int) ([]ObjectLocation, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() ([]ObjectLocation, error) { return cb.real.GetObjectsWithoutHash(ctx, limit, offset) })
}

// UpdateContentHash delegates to the real store with circuit breaker protection.
func (cb *CircuitBreakerStore) UpdateContentHash(ctx context.Context, key, backendName, hash string) error {
	return breaker.CBCallNoResult(cb.CircuitBreaker, func() error { return cb.real.UpdateContentHash(ctx, key, backendName, hash) })
}
