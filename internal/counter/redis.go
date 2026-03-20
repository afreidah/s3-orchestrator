// -------------------------------------------------------------------------------
// RedisCounterBackend - Shared Usage Counters via Redis
//
// Author: Alex Freidah
//
// Implements CounterBackend using Redis INCRBY/GET/GETSET for shared usage
// counters across multiple instances. Includes a circuit breaker that falls
// back to an embedded LocalCounterBackend when Redis is unavailable, and a
// health probe goroutine that recovers automatically when Redis returns.
//
// Redis key schema: {prefix}:usage:{YYYY-MM}:{backend}:{field}
// Keys receive a 35-day TTL so old months auto-expire without cleanup.
// -------------------------------------------------------------------------------

package counter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"

	"github.com/redis/go-redis/v9"
	"github.com/afreidah/s3-orchestrator/internal/breaker"
)

// keyTTL is applied to Redis counter keys on creation. Set to 35 days so
// that keys from the previous month expire naturally.
const keyTTL = 35 * 24 * time.Hour

// healthProbeInterval controls how often the background goroutine PINGs
// Redis while the circuit breaker is open.
const healthProbeInterval = 5 * time.Second

// -------------------------------------------------------------------------
// REDIS CLIENT INTERFACE
// -------------------------------------------------------------------------

//go:generate mockgen -destination=mock_redis_test.go -package=counter github.com/afreidah/s3-orchestrator/internal/counter RedisClient
// RedisClient abstracts the Redis operations used by RedisCounterBackend.
// The production implementation is *redis.Client; tests provide a mock.
type RedisClient interface {
	IncrBy(ctx context.Context, key string, value int64) *redis.IntCmd
	Get(ctx context.Context, key string) *redis.StringCmd
	GetSet(ctx context.Context, key string, value any) *redis.StringCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
	Expire(ctx context.Context, key string, expiration time.Duration) *redis.BoolCmd
	Ping(ctx context.Context) *redis.StatusCmd
	Pipeline() redis.Pipeliner
	Close() error
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// RedisCounterBackend stores per-backend usage deltas in Redis for
// cross-instance visibility. Falls back to local counters when Redis is
// unavailable.
type RedisCounterBackend struct {
	client   RedisClient
	prefix   string
	local    *LocalCounterBackend
	cb       *breaker.CircuitBreaker
	backends []string

	// fallback tracks whether the backend is currently using local counters.
	fallbackMu sync.RWMutex
	fallback   bool

	stopProbe chan struct{}
	probeDone chan struct{}
}

// NewRedisCounterBackend creates a shared counter backend backed by Redis.
// Pings Redis on creation; returns an error if Redis is unreachable (a
// configured dependency must be available at boot). Starts a background
// health probe goroutine.
func NewRedisCounterBackend(client RedisClient, cfg *config.RedisConfig, backendNames []string) (*RedisCounterBackend, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping failed: %w", err)
	}

	sentinel := errors.New("redis unavailable")
	cb := breaker.NewCircuitBreaker("redis", cfg.FailureThreshold, cfg.OpenTimeout, func(err error) bool {
		return err != nil
	}, sentinel)

	r := &RedisCounterBackend{
		client:    client,
		prefix:    cfg.KeyPrefix,
		local:     NewLocalCounterBackend(backendNames),
		cb:        cb,
		backends:  backendNames,
		stopProbe: make(chan struct{}),
		probeDone: make(chan struct{}),
	}

	go r.healthProbe()

	slog.Info("Redis counter backend initialized", //nolint:sloglint // constructor has no request context
		"address", cfg.Address,
		"prefix", cfg.KeyPrefix,
	)

	return r, nil
}

// -------------------------------------------------------------------------
// COUNTER BACKEND IMPLEMENTATION
// -------------------------------------------------------------------------

// Backends returns the list of backend names this counter tracks.
func (r *RedisCounterBackend) Backends() []string {
	return r.backends
}

// Add increments a single counter field in Redis, falling back to local on error.
func (r *RedisCounterBackend) Add(backend, field string, delta int64) {
	if r.inFallback() {
		r.local.Add(backend, field, delta)
		return
	}

	key := r.key(backend, field)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := r.client.IncrBy(ctx, key, delta).Err()
	if err != nil {
		telemetry.RedisOperationsTotal.WithLabelValues("incrby", "error").Inc()
		r.recordFailure(err)
		r.local.Add(backend, field, delta)
		return
	}
	telemetry.RedisOperationsTotal.WithLabelValues("incrby", "success").Inc()
	_ = r.cb.PostCheck(nil)

	// Set TTL on first write (best-effort)
	r.client.Expire(ctx, key, keyTTL)
}

// Load reads a counter field from Redis, falling back to local on error.
func (r *RedisCounterBackend) Load(backend, field string) int64 {
	if r.inFallback() {
		return r.local.Load(backend, field)
	}

	key := r.key(backend, field)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	val, err := r.client.Get(ctx, key).Int64()
	if err != nil && !errors.Is(err, redis.Nil) {
		telemetry.RedisOperationsTotal.WithLabelValues("get", "error").Inc()
		r.recordFailure(err)
		return r.local.Load(backend, field)
	}
	telemetry.RedisOperationsTotal.WithLabelValues("get", "success").Inc()
	_ = r.cb.PostCheck(nil)
	return val
}

// Swap atomically reads and resets a counter field via Redis GETSET.
func (r *RedisCounterBackend) Swap(backend, field string) int64 {
	if r.inFallback() {
		return r.local.Swap(backend, field)
	}

	key := r.key(backend, field)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	val, err := r.client.GetSet(ctx, key, 0).Int64()
	if err != nil && !errors.Is(err, redis.Nil) {
		telemetry.RedisOperationsTotal.WithLabelValues("getset", "error").Inc()
		r.recordFailure(err)
		return r.local.Swap(backend, field)
	}
	telemetry.RedisOperationsTotal.WithLabelValues("getset", "success").Inc()
	_ = r.cb.PostCheck(nil)
	return val
}

// AddAll increments all three counter fields in a Redis pipeline.
func (r *RedisCounterBackend) AddAll(backend string, apiReqs, egress, ingress int64) {
	if r.inFallback() {
		r.local.AddAll(backend, apiReqs, egress, ingress)
		return
	}

	if apiReqs <= 0 && egress <= 0 && ingress <= 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	pipe := r.client.Pipeline()
	keys := make([]string, 0, 3)
	if apiReqs > 0 {
		k := r.key(backend, FieldAPIRequests)
		pipe.IncrBy(ctx, k, apiReqs)
		keys = append(keys, k)
	}
	if egress > 0 {
		k := r.key(backend, FieldEgressBytes)
		pipe.IncrBy(ctx, k, egress)
		keys = append(keys, k)
	}
	if ingress > 0 {
		k := r.key(backend, FieldIngressBytes)
		pipe.IncrBy(ctx, k, ingress)
		keys = append(keys, k)
	}

	_, err := pipe.Exec(ctx)
	if err != nil {
		telemetry.RedisOperationsTotal.WithLabelValues("pipeline_add", "error").Inc()
		r.recordFailure(err)
		r.local.AddAll(backend, apiReqs, egress, ingress)
		return
	}
	telemetry.RedisOperationsTotal.WithLabelValues("pipeline_add", "success").Inc()
	_ = r.cb.PostCheck(nil)

	// Set TTL on written keys (best-effort)
	for _, k := range keys {
		r.client.Expire(ctx, k, keyTTL)
	}
}

// LoadAll reads all three counter values from Redis in a pipeline.
func (r *RedisCounterBackend) LoadAll(backend string) LoadAllResult {
	if r.inFallback() {
		return r.local.LoadAll(backend)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	pipe := r.client.Pipeline()
	apiCmd := pipe.Get(ctx, r.key(backend, FieldAPIRequests))
	egressCmd := pipe.Get(ctx, r.key(backend, FieldEgressBytes))
	ingressCmd := pipe.Get(ctx, r.key(backend, FieldIngressBytes))

	_, err := pipe.Exec(ctx)
	if err != nil && !isAllNil(err) {
		telemetry.RedisOperationsTotal.WithLabelValues("pipeline_load", "error").Inc()
		r.recordFailure(err)
		return r.local.LoadAll(backend)
	}
	telemetry.RedisOperationsTotal.WithLabelValues("pipeline_load", "success").Inc()
	_ = r.cb.PostCheck(nil)

	return LoadAllResult{
		APIRequests:  cmdInt64(apiCmd),
		EgressBytes:  cmdInt64(egressCmd),
		IngressBytes: cmdInt64(ingressCmd),
	}
}

// -------------------------------------------------------------------------
// HEALTH PROBE AND RECOVERY
// -------------------------------------------------------------------------

// healthProbe runs in a background goroutine, periodically PINGing Redis
// when the circuit breaker is open. On recovery, it syncs local deltas
// back to Redis and resumes normal operation.
func (r *RedisCounterBackend) healthProbe() {
	defer close(r.probeDone)
	ticker := time.NewTicker(healthProbeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if r.cb.IsHealthy() {
				continue
			}
			r.tryRecover()
		case <-r.stopProbe:
			return
		}
	}
}

// tryRecover PINGs Redis and, on success, atomically deletes stale keys
// and syncs local deltas in a single pipeline, then closes the circuit
// breaker.
//
//nolint:sloglint // health probe has no request context
func (r *RedisCounterBackend) tryRecover() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := r.client.Ping(ctx).Err(); err != nil {
		return
	}

	period := CurrentPeriod()
	fields := []string{FieldAPIRequests, FieldEgressBytes, FieldIngressBytes}

	// Collect local deltas before building the pipeline so we snapshot
	// a consistent view of what needs to be synced.
	type backendDeltas struct {
		name    string
		api     int64
		egress  int64
		ingress int64
	}
	deltas := make([]backendDeltas, 0, len(r.backends))
	for _, backend := range r.backends {
		cur := r.local.LoadAll(backend)
		deltas = append(deltas, backendDeltas{
			name:    backend,
			api:     cur.APIRequests,
			egress:  cur.EgressBytes,
			ingress: cur.IngressBytes,
		})
	}

	// Build a single pipeline: delete stale keys then INCRBY local deltas.
	// If the pipeline fails, nothing is committed — no counter loss.
	pipe := r.client.Pipeline()
	for _, backend := range r.backends {
		for _, field := range fields {
			pipe.Del(ctx, r.keyForPeriod(backend, field, period))
		}
	}
	for _, d := range deltas {
		if d.api > 0 {
			k := r.keyForPeriod(d.name, FieldAPIRequests, period)
			pipe.IncrBy(ctx, k, d.api)
			pipe.Expire(ctx, k, keyTTL)
		}
		if d.egress > 0 {
			k := r.keyForPeriod(d.name, FieldEgressBytes, period)
			pipe.IncrBy(ctx, k, d.egress)
			pipe.Expire(ctx, k, keyTTL)
		}
		if d.ingress > 0 {
			k := r.keyForPeriod(d.name, FieldIngressBytes, period)
			pipe.IncrBy(ctx, k, d.ingress)
			pipe.Expire(ctx, k, keyTTL)
		}
	}

	if _, err := pipe.Exec(ctx); err != nil {
		slog.Warn("Redis recovery pipeline failed, will retry", "error", err)
		return
	}

	// Pipeline committed — safe to zero local counters now.
	for _, backend := range r.backends {
		for _, field := range fields {
			r.local.Swap(backend, field)
		}
	}

	// Clear fallback state and close circuit breaker
	r.setFallback(false)
	_ = r.cb.PostCheck(nil)

	slog.Info("Redis counter backend recovered, local deltas synced")
}

// -------------------------------------------------------------------------
// FALLBACK MANAGEMENT
// -------------------------------------------------------------------------

// inFallback returns true when using local counters due to Redis unavailability.
func (r *RedisCounterBackend) inFallback() bool {
	r.fallbackMu.RLock()
	defer r.fallbackMu.RUnlock()
	return r.fallback
}

// setFallback toggles fallback mode and updates the Prometheus gauge.
func (r *RedisCounterBackend) setFallback(v bool) {
	r.fallbackMu.Lock()
	r.fallback = v
	r.fallbackMu.Unlock()
	if v {
		telemetry.RedisFallbackActive.Set(1)
	} else {
		telemetry.RedisFallbackActive.Set(0)
	}
}

// recordFailure feeds the error to the circuit breaker. If the breaker
// opens, transitions to fallback mode.
//
//nolint:sloglint // Counter interface has no request context; fallback is a system-level event.
func (r *RedisCounterBackend) recordFailure(err error) {
	_ = r.cb.PostCheck(err)
	if !r.cb.IsHealthy() && !r.inFallback() {
		r.setFallback(true)
		slog.Warn("Redis counter backend entering fallback to local counters")
	}
}

// -------------------------------------------------------------------------
// KEY HELPERS
// -------------------------------------------------------------------------

// key returns the Redis key for a backend field in the current period.
func (r *RedisCounterBackend) key(backend, field string) string {
	return r.keyForPeriod(backend, field, CurrentPeriod())
}

// keyForPeriod returns the Redis key for a backend field in a specific period.
func (r *RedisCounterBackend) keyForPeriod(backend, field, period string) string {
	return fmt.Sprintf("%s:usage:%s:%s:%s", r.prefix, period, backend, field)
}

// -------------------------------------------------------------------------
// REDIS RESULT HELPERS
// -------------------------------------------------------------------------

// cmdInt64 extracts an int64 from a Redis StringCmd, returning 0 for nil
// (key does not exist) or parse errors.
func cmdInt64(cmd *redis.StringCmd) int64 {
	v, err := cmd.Int64()
	if err != nil {
		return 0
	}
	return v
}

// isAllNil returns true when a pipeline error is entirely redis.Nil (all
// keys missing). This is expected for backends with no activity in the
// current period.
func isAllNil(err error) bool {
	return errors.Is(err, redis.Nil)
}

// -------------------------------------------------------------------------
// LIFECYCLE
// -------------------------------------------------------------------------

// IsHealthy returns true when Redis is reachable and the circuit is closed.
func (r *RedisCounterBackend) IsHealthy() bool {
	return r.cb.IsHealthy()
}

// InFallbackMode returns true when the backend is using local counters
// due to Redis unavailability.
func (r *RedisCounterBackend) InFallbackMode() bool {
	return r.inFallback()
}

// Close stops the health probe goroutine and closes the Redis client.
func (r *RedisCounterBackend) Close() error {
	close(r.stopProbe)
	<-r.probeDone
	return r.client.Close()
}
