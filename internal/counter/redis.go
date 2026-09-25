// -------------------------------------------------------------------------------
// RedisCounterBackend - Shared Usage Counters via Redis
//
// Author: Alex Freidah
//
// Implements Backend using Redis INCRBY/GET/GETSET for shared usage
// counters across multiple instances. Includes a circuit breaker that falls
// back to an embedded LocalCounterBackend when Redis is unavailable, and a
// health probe goroutine that recovers automatically when Redis returns.
//
// Redis key schema: {prefix}:usage:{YYYY-MM}:{backend}:{field}
// Keys receive a 35-day TTL so old months auto-expire without cleanup.
//
// Also carries shared state: a value one instance computes for the whole fleet
// and the others read, under {prefix}:shared:{name}, and change notifications
// on pub/sub channels of the same name. It rides the same client and circuit
// breaker, so a Redis outage is one condition, not two.
// -------------------------------------------------------------------------------

package counter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"

	"github.com/redis/go-redis/v9"
)

// keyTTL is applied to Redis counter keys on creation. Set to 35 days so
// that keys from the previous month expire naturally.
const keyTTL = 35 * 24 * time.Hour

// healthProbeInterval controls how often the background goroutine PINGs
// Redis while the circuit breaker is open.
const healthProbeInterval = 5 * time.Second

// opTimeout bounds individual Redis round-trip operations (GET, INCRBY,
// EXPIRE, pipeline exec). Must be short enough that a stalled Redis can't
// stall a request thread, but long enough to tolerate transient latency.
const opTimeout = 2 * time.Second

// pingTimeout bounds Redis liveness checks  -  both the initial boot-time
// PING and the ongoing health probe that runs while the circuit breaker
// is open. Longer than opTimeout because these aren't on the request path.
const pingTimeout = 5 * time.Second

// watchRetryDelay is how long WatchShared waits before resubscribing after
// the subscription connection fails.
const watchRetryDelay = time.Second

// -------------------------------------------------------------------------
// REDIS CLIENT INTERFACE
// -------------------------------------------------------------------------

// RedisClient abstracts the Redis operations used by RedisCounterBackend.
// The production implementation is *redis.Client; tests provide a mock.
//
//go:generate mockgen -destination=mock_redis_test.go -package=counter github.com/afreidah/s3-orchestrator/internal/counter RedisClient
type RedisClient interface {
	IncrBy(ctx context.Context, key string, value int64) *redis.IntCmd
	Get(ctx context.Context, key string) *redis.StringCmd
	Set(ctx context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd
	Publish(ctx context.Context, channel string, message any) *redis.IntCmd
	Subscribe(ctx context.Context, channels ...string) *redis.PubSub
	GetSet(ctx context.Context, key string, value any) *redis.StringCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
	Expire(ctx context.Context, key string, expiration time.Duration) *redis.BoolCmd
	HGet(ctx context.Context, key, field string) *redis.StringCmd
	Ping(ctx context.Context) *redis.StatusCmd
	Pipeline() redis.Pipeliner
	TxPipeline() redis.Pipeliner
	Close() error
}

// subscription is the receive side of a pub/sub subscription. *redis.PubSub
// satisfies it: Receive returns a *redis.Subscription each time the channel is
// subscribed, including after go-redis reconnects, and a *redis.Message for
// each message published on it.
type subscription interface {
	Receive(ctx context.Context) (any, error)
	Close() error
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// RedisCounterBackend stores per-backend usage deltas in Redis for
// cross-instance visibility. Falls back to local counters when Redis is
// unavailable.
//
// Tests construct this type directly and may leave log nil, so every caller
// routes through logger() rather than reading the field.
type RedisCounterBackend struct {
	client    RedisClient
	subscribe func(ctx context.Context, channel string) subscription
	prefix    string
	local     *LocalCounterBackend
	cb        *breaker.CircuitBreaker
	backends  []string
	log       *slog.Logger

	fallbackMu sync.RWMutex
	fallback   bool // currently serving from the local counters

	stopProbe chan struct{}
	probeDone chan struct{}
	closeOnce sync.Once
}

// logger returns the component-scoped logger, falling back to
// slog.Default() when the backend was constructed by a test that did
// not set the log field.
func (r *RedisCounterBackend) logger() *slog.Logger {
	if r.log == nil {
		return slog.Default()
	}
	return r.log
}

// NewRedisCounterBackend creates a shared counter backend backed by Redis and
// starts the background health probe.
//
// An unreachable Redis at boot is the same condition as Redis failing while
// running: the backend starts in fallback on local counters, logs it at error
// level, and the health probe moves it onto Redis once Redis answers. Refusing
// to start would take the whole service down over a dependency it can run
// without, and starting without the backend at all would leave the instance on
// local counters with nothing to bring it back.
func NewRedisCounterBackend(client RedisClient, cfg *config.RedisConfig, backendNames []string) *RedisCounterBackend {
	sentinel := errors.New("redis unavailable")
	cb := breaker.NewCircuitBreaker(breaker.Config{
		Name:      "redis",
		Threshold: cfg.FailureThreshold,
		Timeout:   cfg.OpenTimeout,
		IsError:   func(err error) bool { return err != nil },
		Sentinel:  sentinel,
	})
	cb.SetOnStateChange(telemetry.NewCircuitBreakerHook("redis"))

	r := &RedisCounterBackend{
		client: client,
		subscribe: func(ctx context.Context, channel string) subscription {
			return client.Subscribe(ctx, channel)
		},
		prefix:    cfg.KeyPrefix,
		local:     NewLocalCounterBackend(backendNames),
		cb:        cb,
		backends:  backendNames,
		log:       slog.Default().With(logfmt.Component("redis_counters")),
		stopProbe: make(chan struct{}),
		probeDone: make(chan struct{}),
	}

	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		r.setFallback(true)
		r.logger().ErrorContext(ctx, "Redis unreachable at startup, running on local counters until it answers",
			"address", cfg.Address,
			logfmt.Err(err),
		)
	} else {
		r.logger().InfoContext(ctx, "initialized",
			"address", cfg.Address,
			"prefix", cfg.KeyPrefix,
		)
	}

	go r.healthProbe()
	return r
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
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	err := r.client.IncrBy(ctx, key, delta).Err()
	if err != nil {
		telemetry.RedisOperationsTotal.WithLabelValues("incrby", "error").Inc()
		r.recordFailure(err)
		r.local.Add(backend, field, delta)
		return
	}
	telemetry.RedisOperationsTotal.WithLabelValues("incrby", "success").Inc()
	r.notePostCheck("incrby", nil)

	// Set TTL on first write (best-effort)
	r.client.Expire(ctx, key, keyTTL)
}

// Load reads a counter field from Redis, falling back to local on error.
func (r *RedisCounterBackend) Load(backend, field string) int64 {
	if r.inFallback() {
		return r.local.Load(backend, field)
	}

	key := r.key(backend, field)
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	val, err := r.client.Get(ctx, key).Int64()
	if err != nil && !errors.Is(err, redis.Nil) {
		telemetry.RedisOperationsTotal.WithLabelValues("get", "error").Inc()
		r.recordFailure(err)
		return r.local.Load(backend, field)
	}
	telemetry.RedisOperationsTotal.WithLabelValues("get", "success").Inc()
	r.notePostCheck("get", nil)
	return val
}

// Swap atomically reads and resets a counter field via Redis GETSET.
func (r *RedisCounterBackend) Swap(backend, field string) int64 {
	if r.inFallback() {
		return r.local.Swap(backend, field)
	}

	key := r.key(backend, field)
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	val, err := r.client.GetSet(ctx, key, 0).Int64()
	if err != nil && !errors.Is(err, redis.Nil) {
		telemetry.RedisOperationsTotal.WithLabelValues("getset", "error").Inc()
		r.recordFailure(err)
		return r.local.Swap(backend, field)
	}
	telemetry.RedisOperationsTotal.WithLabelValues("getset", "success").Inc()
	r.notePostCheck("getset", nil)
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

	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	pipe := r.client.Pipeline()
	if apiReqs > 0 {
		k := r.key(backend, FieldAPIRequests)
		pipe.IncrBy(ctx, k, apiReqs)
		pipe.Expire(ctx, k, keyTTL)
	}
	if egress > 0 {
		k := r.key(backend, FieldEgressBytes)
		pipe.IncrBy(ctx, k, egress)
		pipe.Expire(ctx, k, keyTTL)
	}
	if ingress > 0 {
		k := r.key(backend, FieldIngressBytes)
		pipe.IncrBy(ctx, k, ingress)
		pipe.Expire(ctx, k, keyTTL)
	}

	_, err := pipe.Exec(ctx)
	if err != nil {
		telemetry.RedisOperationsTotal.WithLabelValues("pipeline_add", "error").Inc()
		r.recordFailure(err)
		r.local.AddAll(backend, apiReqs, egress, ingress)
		return
	}
	telemetry.RedisOperationsTotal.WithLabelValues("pipeline_add", "success").Inc()
	r.notePostCheck("pipeline_add", nil)
}

// LoadAll reads all three counter values from Redis in a pipeline.
func (r *RedisCounterBackend) LoadAll(backend string) LoadAllResult {
	if r.inFallback() {
		return r.local.LoadAll(backend)
	}

	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
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
	r.notePostCheck("pipeline_load", nil)

	return LoadAllResult{
		APIRequests:  cmdInt64(apiCmd),
		EgressBytes:  cmdInt64(egressCmd),
		IngressBytes: cmdInt64(ingressCmd),
	}
}

// AddPools increments the named pool counters in one pipelined pass over the
// backend's pool hash.
func (r *RedisCounterBackend) AddPools(backend string, deltas map[string]int64) {
	if len(deltas) == 0 {
		return
	}
	if r.inFallback() {
		r.local.AddPools(backend, deltas)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	key := r.poolKey(backend)
	pipe := r.client.Pipeline()
	for pool, delta := range deltas {
		if delta > 0 {
			pipe.HIncrBy(ctx, key, pool, delta)
		}
	}
	pipe.Expire(ctx, key, keyTTL)

	if _, err := pipe.Exec(ctx); err != nil {
		telemetry.RedisOperationsTotal.WithLabelValues("pipeline_pool_add", "error").Inc()
		r.recordFailure(err)
		r.local.AddPools(backend, deltas)
		return
	}
	telemetry.RedisOperationsTotal.WithLabelValues("pipeline_pool_add", "success").Inc()
	r.notePostCheck("pipeline_pool_add", nil)
}

// LoadPool reads one pool counter, falling back to local on error.
func (r *RedisCounterBackend) LoadPool(backend, pool string) int64 {
	if r.inFallback() {
		return r.local.LoadPool(backend, pool)
	}

	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	val, err := r.client.HGet(ctx, r.poolKey(backend), pool).Int64()
	if err != nil && !errors.Is(err, redis.Nil) {
		telemetry.RedisOperationsTotal.WithLabelValues("hget", "error").Inc()
		r.recordFailure(err)
		return r.local.LoadPool(backend, pool)
	}
	telemetry.RedisOperationsTotal.WithLabelValues("hget", "success").Inc()
	r.notePostCheck("hget", nil)
	return val
}

// SwapPools reads and clears the backend's pool hash.
//
// Read and delete run in one transaction so a charge landing between the two
// is not silently dropped: without it a pool increment arriving after the
// read but before the delete would be flushed to nobody.
func (r *RedisCounterBackend) SwapPools(backend string) map[string]int64 {
	if r.inFallback() {
		return r.local.SwapPools(backend)
	}

	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	key := r.poolKey(backend)
	tx := r.client.TxPipeline()
	all := tx.HGetAll(ctx, key)
	tx.Del(ctx, key)

	if _, err := tx.Exec(ctx); err != nil && !isAllNil(err) {
		telemetry.RedisOperationsTotal.WithLabelValues("pool_swap", "error").Inc()
		r.recordFailure(err)
		return r.local.SwapPools(backend)
	}
	telemetry.RedisOperationsTotal.WithLabelValues("pool_swap", "success").Inc()
	r.notePostCheck("pool_swap", nil)

	fields := all.Val()
	if len(fields) == 0 {
		return nil
	}
	out := make(map[string]int64, len(fields))
	for pool, raw := range fields {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			r.logger().WarnContext(ctx, "discarding unparseable pool counter",
				slog.String("backend", backend), slog.String("pool", pool), slog.String("value", raw))
			continue
		}
		out[pool] = v
	}
	return out
}

// -------------------------------------------------------------------------
// HEALTH PROBE AND RECOVERY
// -------------------------------------------------------------------------

// healthProbe runs in a background goroutine, periodically PINGing Redis
// while the backend is in fallback. On recovery, it syncs local deltas back
// to Redis and resumes normal operation. It keys off the fallback flag rather
// than the breaker, because a backend that could not reach Redis at startup
// is in fallback without the breaker ever having opened.
func (r *RedisCounterBackend) healthProbe() {
	defer close(r.probeDone)
	ticker := time.NewTicker(healthProbeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if !r.inFallback() {
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
// queueReplay adds one backend's locally-buffered deltas to the recovery
// pipeline: the three fixed counters as INCRBYs, and the pool counters as
// HINCRBYs on the period's pool hash.
func (r *RedisCounterBackend) queueReplay(ctx context.Context, pipe redis.Pipeliner, name, period string, d Snapshot) {
	incr := func(field string, delta int64) {
		if delta <= 0 {
			return
		}
		k := r.keyForPeriod(name, field, period)
		pipe.IncrBy(ctx, k, delta)
		pipe.Expire(ctx, k, keyTTL)
	}
	incr(FieldAPIRequests, d.APIRequests)
	incr(FieldEgressBytes, d.EgressBytes)
	incr(FieldIngressBytes, d.IngressBytes)

	if len(d.Pools) == 0 {
		return
	}
	k := r.poolKeyForPeriod(name, period)
	for pool, delta := range d.Pools {
		if delta > 0 {
			pipe.HIncrBy(ctx, k, pool, delta)
		}
	}
	pipe.Expire(ctx, k, keyTTL)
}

func (r *RedisCounterBackend) tryRecover() {
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	if err := r.client.Ping(ctx).Err(); err != nil {
		return
	}

	period := CurrentPeriod()

	// Atomically swap the entire local counter map in one operation. This
	// prevents the race where per-backend SwapAll calls allow concurrent
	// Add calls to slip between swaps and lose deltas.
	allDeltas := r.local.SwapAllBackends()

	// Build a pipeline of INCRBY commands (additive, safe for concurrent
	// execution by multiple instances). No DEL  -  stale keys from before
	// the outage expire via TTL, and INCRBY is idempotent across instances.
	pipe := r.client.Pipeline()
	for name, d := range allDeltas {
		r.queueReplay(ctx, pipe, name, period, d)
	}

	if _, err := pipe.Exec(ctx); err != nil {
		// Pipeline failed  -  restore swapped deltas to local counters so
		// they are retried on the next recovery attempt.
		for name, d := range allDeltas {
			r.local.AddAll(name, d.APIRequests, d.EgressBytes, d.IngressBytes)
		}
		r.logger().WarnContext(context.Background(), "Redis recovery pipeline failed, will retry", "error", err)
		return
	}

	// Clear fallback state and close circuit breaker. cb.Recover()
	// is used here instead of PostCheck(nil) because the redis hot
	// path bypasses PreCheck (it branches on inFallback() directly),
	// so the breaker never reaches HalfOpen on its own; PostCheck(nil)
	// would leave an Open breaker stuck and IsHealthy() permanently
	// false. Recover() also zeroes the failure counter so a single
	// transient error post-recovery does not immediately re-trip.
	r.setFallback(false)
	r.cb.Recover()

	r.logger().InfoContext(context.Background(), "Redis counter backend recovered, local deltas synced")
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

// notePostCheck feeds the operation outcome to the circuit breaker and
// records any bookkeeping error to the internal-errors metric. Prefer this
// over a bare `_ = r.cb.PostCheck(...)`: it makes state-transition bugs
// observable via s3o_circuit_breaker_internal_errors_total.
func (r *RedisCounterBackend) notePostCheck(op string, opErr error) {
	if cbErr := r.cb.PostCheck(opErr); cbErr != nil {
		telemetry.CircuitBreakerInternalErrorsTotal.WithLabelValues("redis", op).Inc()
		r.logger().WarnContext(context.Background(), "Redis circuit breaker PostCheck reported error",
			"operation", op, "error", cbErr)
	}
}

// recordFailure feeds the error to the circuit breaker. If the breaker
// opens, transitions to fallback mode.
func (r *RedisCounterBackend) recordFailure(err error) {
	r.notePostCheck("failure", err)
	if !r.cb.IsHealthy() && !r.inFallback() {
		r.setFallback(true)
		r.logger().WarnContext(context.Background(), "Redis counter backend entering fallback to local counters")
	}
}

// -------------------------------------------------------------------------
// KEY HELPERS
// -------------------------------------------------------------------------

// key returns the Redis key for a backend field in the current period.
func (r *RedisCounterBackend) key(backend, field string) string {
	return r.keyForPeriod(backend, field, CurrentPeriod())
}

// sharedKey returns the Redis key for a named piece of shared state.
func (r *RedisCounterBackend) sharedKey(name string) string {
	return fmt.Sprintf("%s:shared:%s", r.prefix, name)
}

// keyForPeriod returns the Redis key for a backend field in a specific period.
func (r *RedisCounterBackend) keyForPeriod(backend, field, period string) string {
	return fmt.Sprintf("%s:usage:%s:%s:%s", r.prefix, period, backend, field)
}

// poolKey returns the Redis hash holding every pool counter for a backend in
// the current period. One hash rather than a key per pool, so a flush can
// enumerate the pools that were actually charged without scanning the
// keyspace or being told which pools config currently declares.
func (r *RedisCounterBackend) poolKey(backend string) string {
	return r.poolKeyForPeriod(backend, CurrentPeriod())
}

// poolKeyForPeriod returns the pool hash key for a specific period.
func (r *RedisCounterBackend) poolKeyForPeriod(backend, period string) string {
	return fmt.Sprintf("%s:usage:%s:%s:pools", r.prefix, period, backend)
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
// SHARED STATE
// -------------------------------------------------------------------------

// ErrSharedStateUnavailable is returned while Redis is in fallback, when
// there is nowhere shared to write to or read from.
var ErrSharedStateUnavailable = errors.New("redis shared state unavailable")

// PutShared stores value under name for every instance to read, expiring
// after ttl.
func (r *RedisCounterBackend) PutShared(ctx context.Context, name string, value []byte, ttl time.Duration) error {
	if r.inFallback() {
		return ErrSharedStateUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	if err := r.client.Set(ctx, r.sharedKey(name), value, ttl).Err(); err != nil {
		telemetry.RedisOperationsTotal.WithLabelValues("set", "error").Inc()
		r.recordFailure(err)
		return err
	}
	telemetry.RedisOperationsTotal.WithLabelValues("set", "success").Inc()
	r.notePostCheck("set", nil)
	return nil
}

// GetShared returns the value stored under name, or nil when there is none.
func (r *RedisCounterBackend) GetShared(ctx context.Context, name string) ([]byte, error) {
	if r.inFallback() {
		return nil, ErrSharedStateUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	val, err := r.client.Get(ctx, r.sharedKey(name)).Bytes()
	if err != nil && !errors.Is(err, redis.Nil) {
		telemetry.RedisOperationsTotal.WithLabelValues("get", "error").Inc()
		r.recordFailure(err)
		return nil, err
	}
	telemetry.RedisOperationsTotal.WithLabelValues("get", "success").Inc()
	r.notePostCheck("get", nil)
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	return val, nil
}

// NotifyShared tells every instance watching channel that the state it
// covers has changed. The message carries nothing; a watcher rereads the
// state itself.
func (r *RedisCounterBackend) NotifyShared(ctx context.Context, channel string) error {
	if r.inFallback() {
		return ErrSharedStateUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	if err := r.client.Publish(ctx, r.sharedKey(channel), "changed").Err(); err != nil {
		telemetry.RedisOperationsTotal.WithLabelValues("publish", "error").Inc()
		r.recordFailure(err)
		return err
	}
	telemetry.RedisOperationsTotal.WithLabelValues("publish", "success").Inc()
	r.notePostCheck("publish", nil)
	return nil
}

// WatchShared calls onChange for every notification on channel, and once
// each time the subscription is established: at start and after every
// reconnect. Pub/sub keeps nothing for a subscriber that was away, so the
// watcher assumes it missed a change whenever it (re)joins. Blocks until ctx
// is done.
func (r *RedisCounterBackend) WatchShared(ctx context.Context, channel string, onChange func(context.Context)) {
	sub := r.subscribe(ctx, r.sharedKey(channel))
	defer func() { _ = sub.Close() }()

	interrupted := false
	for {
		msg, err := sub.Receive(ctx)
		if err != nil {
			if !r.awaitResubscribe(ctx, channel, err, !interrupted) {
				return
			}
			interrupted = true
			continue
		}

		change, subscribed := classifySharedMessage(msg)
		if subscribed && interrupted {
			r.logger().InfoContext(ctx, "shared state subscription restored", "channel", channel)
			interrupted = false
		}
		if change {
			onChange(ctx)
		}
	}
}

// awaitResubscribe waits out the retry delay after the subscription fails,
// logging the first failure of an outage. Reports false when ctx ended and the
// watcher should stop.
func (r *RedisCounterBackend) awaitResubscribe(ctx context.Context, channel string, err error, first bool) bool {
	if ctx.Err() != nil {
		return false
	}
	if first {
		r.logger().WarnContext(ctx, "shared state subscription lost, resubscribing",
			"channel", channel, logfmt.Err(err))
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(watchRetryDelay):
		return true
	}
}

// classifySharedMessage reports whether a received pub/sub message means the
// shared state may have changed, and whether it confirms a subscription. A
// subscription counts as a change because anything published while the
// subscriber was away is gone.
func classifySharedMessage(msg any) (change, subscribed bool) {
	switch m := msg.(type) {
	case *redis.Subscription:
		return m.Kind == "subscribe", m.Kind == "subscribe"
	case *redis.Message:
		return true, false
	default:
		return false, false
	}
}

// -------------------------------------------------------------------------
// LIFECYCLE
// -------------------------------------------------------------------------

// IsHealthy returns true when Redis is reachable and the circuit is closed.
func (r *RedisCounterBackend) IsHealthy() bool {
	return r.cb.IsHealthy()
}

// Close stops the health probe goroutine and closes the Redis client.
// Safe to call multiple times.
func (r *RedisCounterBackend) Close() error {
	r.closeOnce.Do(func() {
		close(r.stopProbe)
	})
	<-r.probeDone
	return r.client.Close()
}
