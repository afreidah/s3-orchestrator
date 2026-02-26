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
// States: closed (healthy) → open (DB down) → half-open (probing) → closed.
// -------------------------------------------------------------------------------

package storage

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"
)

// -------------------------------------------------------------------------
// STATE
// -------------------------------------------------------------------------

type circuitState int

const (
	stateClosed   circuitState = iota // healthy — all calls pass through
	stateOpen                         // DB down — return ErrDBUnavailable
	stateHalfOpen                     // probing — one call allowed through
)

func (s circuitState) String() string {
	switch s {
	case stateClosed:
		return "closed"
	case stateOpen:
		return "open"
	case stateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// -------------------------------------------------------------------------
// CIRCUIT BREAKER STORE
// -------------------------------------------------------------------------

// CircuitBreakerStore implements MetadataStore by wrapping a real store with
// circuit breaker logic. When the database is unreachable, it returns
// ErrDBUnavailable instead of passing through to the real store.
type CircuitBreakerStore struct {
	real          MetadataStore
	mu            sync.RWMutex
	state         circuitState
	failures      int
	lastFailure   time.Time
	failThreshold int
	openTimeout   time.Duration
	probeInFlight atomic.Bool
}

// Compile-time check.
var _ MetadataStore = (*CircuitBreakerStore)(nil)

// NewCircuitBreakerStore wraps a real MetadataStore with circuit breaker logic.
func NewCircuitBreakerStore(real MetadataStore, cfg config.CircuitBreakerConfig) *CircuitBreakerStore {
	return &CircuitBreakerStore{
		real:          real,
		state:         stateClosed,
		failThreshold: cfg.FailureThreshold,
		openTimeout:   cfg.OpenTimeout,
	}
}

// IsHealthy returns true when the circuit is closed (database is reachable).
func (cb *CircuitBreakerStore) IsHealthy() bool {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state == stateClosed
}

// -------------------------------------------------------------------------
// STATE MACHINE
// -------------------------------------------------------------------------

// preCheck returns ErrDBUnavailable when the circuit is open. Transitions
// open → half-open when the timeout has elapsed, allowing one probe request.
func (cb *CircuitBreakerStore) preCheck() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case stateClosed:
		return nil
	case stateOpen:
		if time.Since(cb.lastFailure) >= cb.openTimeout {
			if !cb.probeInFlight.CompareAndSwap(false, true) {
				return ErrDBUnavailable // another probe already in flight
			}
			cb.transition(stateHalfOpen)
			return nil // allow this request as the probe
		}
		return ErrDBUnavailable
	case stateHalfOpen:
		return ErrDBUnavailable
	}
	return nil
}

// postCheck records the result of a real store call and transitions state.
// When a DB error causes the circuit to open (or reopen), the original error
// is replaced with ErrDBUnavailable so the manager always sees the canonical
// sentinel for "database down".
func (cb *CircuitBreakerStore) postCheck(err error) error {
	if !isDBError(err) {
		cb.onSuccess()
		return err
	}
	cb.onFailure()
	if !cb.IsHealthy() {
		return ErrDBUnavailable
	}
	return err
}

// onSuccess resets failures and transitions half-open → closed.
func (cb *CircuitBreakerStore) onSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.state == stateHalfOpen {
		cb.probeInFlight.Store(false)
		cb.transition(stateClosed)
	}
	cb.failures = 0
}

// onFailure increments the failure counter and transitions to open if the
// threshold is reached.
func (cb *CircuitBreakerStore) onFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failures++
	cb.lastFailure = time.Now()

	switch cb.state {
	case stateHalfOpen:
		cb.probeInFlight.Store(false)
		cb.transition(stateOpen)
	case stateClosed:
		if cb.failures >= cb.failThreshold {
			cb.transition(stateOpen)
		}
	}
}

// transition changes the circuit state and emits metrics + logs.
// Caller must hold cb.mu.
func (cb *CircuitBreakerStore) transition(to circuitState) {
	from := cb.state
	cb.state = to
	telemetry.CircuitBreakerState.Set(float64(to))
	telemetry.CircuitBreakerTransitionsTotal.WithLabelValues(from.String(), to.String()).Inc()

	switch to {
	case stateClosed:
		slog.Info("Circuit breaker: database recovered, circuit closed")
	case stateOpen:
		slog.Warn("Circuit breaker: database unreachable, circuit opened",
			"failures", cb.failures)
	case stateHalfOpen:
		slog.Warn("Circuit breaker: probing database")
	}
}

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
// FORWARDING HELPERS
// -------------------------------------------------------------------------

// cbCall wraps a store call that returns (T, error) with circuit breaker logic.
func cbCall[T any](cb *CircuitBreakerStore, fn func() (T, error)) (T, error) {
	var zero T
	if err := cb.preCheck(); err != nil {
		return zero, err
	}
	result, err := fn()
	return result, cb.postCheck(err)
}

// cbCallNoResult wraps a store call that returns only error with circuit breaker logic.
func cbCallNoResult(cb *CircuitBreakerStore, fn func() error) error {
	if err := cb.preCheck(); err != nil {
		return err
	}
	return cb.postCheck(fn())
}

// -------------------------------------------------------------------------
// FORWARDING METHODS
// -------------------------------------------------------------------------

// All forwarding methods below implement the MetadataStore interface.
// Each delegates to the real store via cbCall/cbCallNoResult for circuit
// breaker protection (preCheck → real.Method → postCheck).

func (cb *CircuitBreakerStore) GetAllObjectLocations(ctx context.Context, key string) ([]ObjectLocation, error) {
	return cbCall(cb, func() ([]ObjectLocation, error) { return cb.real.GetAllObjectLocations(ctx, key) })
}

func (cb *CircuitBreakerStore) RecordObject(ctx context.Context, key, backend string, size int64) error {
	return cbCallNoResult(cb, func() error { return cb.real.RecordObject(ctx, key, backend, size) })
}

func (cb *CircuitBreakerStore) DeleteObject(ctx context.Context, key string) ([]DeletedCopy, error) {
	return cbCall(cb, func() ([]DeletedCopy, error) { return cb.real.DeleteObject(ctx, key) })
}

func (cb *CircuitBreakerStore) ListObjects(ctx context.Context, prefix, startAfter string, maxKeys int) (*ListObjectsResult, error) {
	return cbCall(cb, func() (*ListObjectsResult, error) { return cb.real.ListObjects(ctx, prefix, startAfter, maxKeys) })
}

func (cb *CircuitBreakerStore) ListDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*DirectoryListResult, error) {
	return cbCall(cb, func() (*DirectoryListResult, error) {
		return cb.real.ListDirectoryChildren(ctx, prefix, startAfter, maxKeys)
	})
}

func (cb *CircuitBreakerStore) GetBackendWithSpace(ctx context.Context, size int64, backendOrder []string) (string, error) {
	return cbCall(cb, func() (string, error) { return cb.real.GetBackendWithSpace(ctx, size, backendOrder) })
}

func (cb *CircuitBreakerStore) GetLeastUtilizedBackend(ctx context.Context, size int64, eligible []string) (string, error) {
	return cbCall(cb, func() (string, error) { return cb.real.GetLeastUtilizedBackend(ctx, size, eligible) })
}

func (cb *CircuitBreakerStore) CreateMultipartUpload(ctx context.Context, uploadID, key, backend, contentType string) error {
	return cbCallNoResult(cb, func() error { return cb.real.CreateMultipartUpload(ctx, uploadID, key, backend, contentType) })
}

func (cb *CircuitBreakerStore) GetMultipartUpload(ctx context.Context, uploadID string) (*MultipartUpload, error) {
	return cbCall(cb, func() (*MultipartUpload, error) { return cb.real.GetMultipartUpload(ctx, uploadID) })
}

func (cb *CircuitBreakerStore) RecordPart(ctx context.Context, uploadID string, partNumber int, etag string, size int64) error {
	return cbCallNoResult(cb, func() error { return cb.real.RecordPart(ctx, uploadID, partNumber, etag, size) })
}

func (cb *CircuitBreakerStore) GetParts(ctx context.Context, uploadID string) ([]MultipartPart, error) {
	return cbCall(cb, func() ([]MultipartPart, error) { return cb.real.GetParts(ctx, uploadID) })
}

func (cb *CircuitBreakerStore) DeleteMultipartUpload(ctx context.Context, uploadID string) error {
	return cbCallNoResult(cb, func() error { return cb.real.DeleteMultipartUpload(ctx, uploadID) })
}

func (cb *CircuitBreakerStore) GetQuotaStats(ctx context.Context) (map[string]QuotaStat, error) {
	return cbCall(cb, func() (map[string]QuotaStat, error) { return cb.real.GetQuotaStats(ctx) })
}

func (cb *CircuitBreakerStore) GetObjectCounts(ctx context.Context) (map[string]int64, error) {
	return cbCall(cb, func() (map[string]int64, error) { return cb.real.GetObjectCounts(ctx) })
}

func (cb *CircuitBreakerStore) GetActiveMultipartCounts(ctx context.Context) (map[string]int64, error) {
	return cbCall(cb, func() (map[string]int64, error) { return cb.real.GetActiveMultipartCounts(ctx) })
}

func (cb *CircuitBreakerStore) GetStaleMultipartUploads(ctx context.Context, olderThan time.Duration) ([]MultipartUpload, error) {
	return cbCall(cb, func() ([]MultipartUpload, error) { return cb.real.GetStaleMultipartUploads(ctx, olderThan) })
}

func (cb *CircuitBreakerStore) ListObjectsByBackend(ctx context.Context, backendName string, limit int) ([]ObjectLocation, error) {
	return cbCall(cb, func() ([]ObjectLocation, error) { return cb.real.ListObjectsByBackend(ctx, backendName, limit) })
}

func (cb *CircuitBreakerStore) MoveObjectLocation(ctx context.Context, key, fromBackend, toBackend string) (int64, error) {
	return cbCall(cb, func() (int64, error) { return cb.real.MoveObjectLocation(ctx, key, fromBackend, toBackend) })
}

func (cb *CircuitBreakerStore) GetUnderReplicatedObjects(ctx context.Context, factor, limit int) ([]ObjectLocation, error) {
	return cbCall(cb, func() ([]ObjectLocation, error) { return cb.real.GetUnderReplicatedObjects(ctx, factor, limit) })
}

func (cb *CircuitBreakerStore) RecordReplica(ctx context.Context, key, targetBackend, sourceBackend string, size int64) (bool, error) {
	return cbCall(cb, func() (bool, error) { return cb.real.RecordReplica(ctx, key, targetBackend, sourceBackend, size) })
}

func (cb *CircuitBreakerStore) FlushUsageDeltas(ctx context.Context, backendName, period string, apiRequests, egressBytes, ingressBytes int64) error {
	return cbCallNoResult(cb, func() error {
		return cb.real.FlushUsageDeltas(ctx, backendName, period, apiRequests, egressBytes, ingressBytes)
	})
}

func (cb *CircuitBreakerStore) GetUsageForPeriod(ctx context.Context, period string) (map[string]UsageStat, error) {
	return cbCall(cb, func() (map[string]UsageStat, error) { return cb.real.GetUsageForPeriod(ctx, period) })
}

func (cb *CircuitBreakerStore) EnqueueCleanup(ctx context.Context, backendName, objectKey, reason string) error {
	return cbCallNoResult(cb, func() error { return cb.real.EnqueueCleanup(ctx, backendName, objectKey, reason) })
}

func (cb *CircuitBreakerStore) GetPendingCleanups(ctx context.Context, limit int) ([]CleanupItem, error) {
	return cbCall(cb, func() ([]CleanupItem, error) { return cb.real.GetPendingCleanups(ctx, limit) })
}

func (cb *CircuitBreakerStore) CompleteCleanupItem(ctx context.Context, id int64) error {
	return cbCallNoResult(cb, func() error { return cb.real.CompleteCleanupItem(ctx, id) })
}

func (cb *CircuitBreakerStore) RetryCleanupItem(ctx context.Context, id int64, backoff time.Duration, lastError string) error {
	return cbCallNoResult(cb, func() error { return cb.real.RetryCleanupItem(ctx, id, backoff, lastError) })
}

func (cb *CircuitBreakerStore) CleanupQueueDepth(ctx context.Context) (int64, error) {
	return cbCall(cb, func() (int64, error) { return cb.real.CleanupQueueDepth(ctx) })
}

// WithAdvisoryLock delegates directly to the underlying store without circuit
// breaker wrapping. Advisory locks are coordination-only; if the DB is down
// the lock attempt fails naturally and the task is skipped.
func (cb *CircuitBreakerStore) WithAdvisoryLock(ctx context.Context, lockID int64, fn func(ctx context.Context) error) (bool, error) {
	return cb.real.WithAdvisoryLock(ctx, lockID, fn)
}
