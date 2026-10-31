// -------------------------------------------------------------------------------
// RedisCounterBackend - Shared Usage Counters via Redis
//
// Author: Alex Freidah
//
// Implements SharedBackend with one Redis hash per backend, so every instance
// charges the same counters. Includes a circuit breaker that falls
// back to an embedded LocalCounterBackend when Redis is unavailable, and a
// health probe goroutine that returns to Redis once it answers. Charges kept
// locally are flushed by the instance that holds them, never replayed into
// Redis.
//
// Redis key schema:
//   - {prefix}:usage:{backend} - hash of the live counters, with fields
//     {bucket}:w:{workload}:{field} and {bucket}:w:{workload}:pool:{name}.
//   - {prefix}:usage-staged:{backend}:{flushID} - a hash taken for a flush,
//     renamed from the live one and deleted once the store has applied it. The
//     store records the flush ID with the deltas, so a staged hash found again
//     after its commit is not counted twice.
//
// Keys receive a 35-day TTL, which a rename keeps, so abandoned keys expire.
//
// Also carries shared state: values the instances write for each other under
// {prefix}:shared:{name}, and messages on pub/sub channels of the same name.
// It rides the same client and circuit breaker, so a Redis outage is one
// condition, not two.
// -------------------------------------------------------------------------------

package counter

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"

	"github.com/redis/go-redis/v9"
)

// keyTTL is applied to Redis counter keys on every charge, so a hash nothing
// flushes, such as a removed backend's, expires on its own.
const keyTTL = 35 * 24 * time.Hour

// opTimeout bounds individual Redis round-trip operations. Must be short
// enough that a stalled Redis can't stall a request thread, but long enough
// to tolerate transient latency.
const opTimeout = 2 * time.Second

// pingTimeout bounds the boot-time PING. Longer than opTimeout because it is
// not on the request path.
const pingTimeout = 5 * time.Second

// watchRetryDelay is how long WatchShared waits before resubscribing after
// the subscription connection fails.
const watchRetryDelay = time.Second

// scanCount is the SCAN batch hint used to find staged hashes.
const scanCount = 100

// globEscaper escapes the characters SCAN MATCH treats as a pattern, so a key
// prefix containing one matches only itself.
var globEscaper = strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`, `]`, `\]`)

// Hash field names. A pool's field is its name under poolFieldPrefix, so a
// pool cannot share a field with a total. Every field sits under
// {bucket}:w:{workload}:, so Load reads every bucket in one call.
const (
	fieldAPIRequests    = "api_requests"
	fieldEgressBytes    = "egress_bytes"
	fieldIngressBytes   = "ingress_bytes"
	poolFieldPrefix     = "pool:"
	workloadFieldPrefix = "w:"
)

// fieldPrefix is the hash field prefix a bucket and workload's counters sit
// under: {unix}:w:{workload}:.
func fieldPrefix(bucket time.Time, workload string) string {
	return fmt.Sprintf("%d:%s%s:", bucket.Unix(), workloadFieldPrefix, workload)
}

// parseField splits a hash field into its bucket, workload and counter name,
// reporting false for a field not in the {unix}:w:{workload}: layout.
func parseField(field string) (time.Time, string, string, bool) {
	unix, rest, ok := strings.Cut(field, ":")
	if !ok {
		return time.Time{}, "", "", false
	}
	secs, err := strconv.ParseInt(unix, 10, 64)
	rest, hasWorkload := strings.CutPrefix(rest, workloadFieldPrefix)
	if err != nil || !hasWorkload {
		return time.Time{}, "", "", false
	}
	workload, name, ok := strings.Cut(rest, ":")
	return time.Unix(secs, 0).UTC(), workload, name, ok
}

// -------------------------------------------------------------------------
// REDIS CLIENT INTERFACE
// -------------------------------------------------------------------------

// RedisClient abstracts the Redis operations used by RedisCounterBackend.
// The production implementation is *redis.Client; tests provide a mock.
//
//go:generate mockgen -destination=mock_redis_test.go -package=counter github.com/afreidah/s3-orchestrator/internal/counter RedisClient
type RedisClient interface {
	Get(ctx context.Context, key string) *redis.StringCmd
	Set(ctx context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd
	Publish(ctx context.Context, channel string, message any) *redis.IntCmd
	Subscribe(ctx context.Context, channels ...string) *redis.PubSub
	HGetAll(ctx context.Context, key string) *redis.MapStringStringCmd
	Rename(ctx context.Context, key, newkey string) *redis.StatusCmd
	Scan(ctx context.Context, cursor uint64, match string, count int64) *redis.ScanCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
	Ping(ctx context.Context) *redis.StatusCmd
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

// RedisCounterBackend stores per-backend usage counters in Redis for
// cross-instance visibility. While its circuit breaker is open it serves from
// local counters instead, and the watchdog's prober moves it back onto Redis.
type RedisCounterBackend struct {
	client    RedisClient
	subscribe func(ctx context.Context, channel string) subscription
	prefix    string
	local     *LocalCounterBackend
	cb        *breaker.CircuitBreaker
	backends  []string
	log       *slog.Logger
}

// NewRedisCounterBackend creates a shared counter backend backed by Redis. An
// unreachable Redis at boot does not fail startup: the breaker opens, the
// backend serves from local counters and logs at error level, and the
// breaker's prober moves it onto Redis once Redis answers.
func NewRedisCounterBackend(client RedisClient, cfg *config.RedisConfig, backendNames []string) *RedisCounterBackend {
	sentinel := errors.New("redis unavailable")
	cb := breaker.NewCircuitBreaker(breaker.Config{
		Name:      "redis",
		Threshold: cfg.FailureThreshold,
		Timeout:   cfg.OpenTimeout,
		IsError:   func(err error) bool { return err != nil },
		Sentinel:  sentinel,
	})
	cb.AddOnStateChange(telemetry.NewCircuitBreakerHook("redis"))

	r := &RedisCounterBackend{
		client: client,
		subscribe: func(ctx context.Context, channel string) subscription {
			return client.Subscribe(ctx, channel)
		},
		prefix:   cfg.KeyPrefix,
		local:    NewLocalCounterBackend(backendNames),
		cb:       cb,
		backends: backendNames,
		log:      slog.Default().With(logfmt.Component("redis_counters")),
	}
	cb.AddOnStateChange(r.onStateChange)

	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		cb.Trip(err)
		r.log.ErrorContext(ctx, "Redis unreachable at startup, running on local counters until it answers",
			"address", cfg.Address,
			logfmt.Err(err),
		)
	} else {
		r.log.InfoContext(ctx, "initialized",
			"address", cfg.Address,
			"prefix", cfg.KeyPrefix,
		)
	}
	return r
}

// Prober returns the watchdog prober that pings Redis while the breaker is
// open and closes it once Redis answers. A ping costs nothing, so the checks
// do not back off.
func (r *RedisCounterBackend) Prober() breaker.Prober {
	return breaker.NewRecoveryProber(r.cb, breaker.Recovery{
		Check: func(ctx context.Context) error { return r.client.Ping(ctx).Err() },
	})
}

// -------------------------------------------------------------------------
// COUNTER BACKEND IMPLEMENTATION
// -------------------------------------------------------------------------

// Backends returns the list of backend names this counter tracks.
func (r *RedisCounterBackend) Backends() []string {
	return r.backends
}

// Charge adds one charge to the backend's hash in one transaction, so a
// failure leaves none of it applied and the charge is kept locally instead. A
// backend not named at construction is dropped, since no flush would stage it.
func (r *RedisCounterBackend) Charge(backend string, bucket time.Time, c Charge) {
	if c.empty() || !slices.Contains(r.backends, backend) {
		return
	}
	if r.inFallback() {
		r.local.Charge(backend, bucket, c)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	key := r.hashKey(backend)
	prefix := fieldPrefix(bucket, c.Workload)
	tx := r.client.TxPipeline()
	incr := func(field string, n int64) {
		if n > 0 {
			tx.HIncrBy(ctx, key, prefix+field, n)
		}
	}
	incr(fieldAPIRequests, c.APIRequests)
	incr(fieldEgressBytes, c.EgressBytes)
	incr(fieldIngressBytes, c.IngressBytes)
	for i := range c.Pools {
		incr(poolFieldPrefix+c.Pools[i].Name, c.PoolN)
	}
	tx.Expire(ctx, key, keyTTL)

	_, err := tx.Exec(ctx)
	if r.observe("charge", err) != nil {
		r.local.Charge(backend, bucket, c)
	}
}

// Load adds the backend's unflushed counters in buckets from since onward into
// u: whatever this instance holds locally, plus the shared hash.
func (r *RedisCounterBackend) Load(backend string, since time.Time, u *core.Usage) {
	r.local.Load(backend, since, u)
	if r.inFallback() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	fields, err := r.client.HGetAll(ctx, r.hashKey(backend)).Result()
	if r.observe("load", err) == nil {
		r.addFields(ctx, fields, func(bucket time.Time, _ string) *core.Usage {
			if bucket.Before(since) {
				return nil
			}
			return u
		})
	}
}

// Take returns the charges this instance holds locally, from a fallback or a
// failed charge. Every instance flushes its own.
func (r *RedisCounterBackend) Take(ctx context.Context) []core.UsageBatch {
	return r.local.Take(ctx)
}

// TakeShared stages each backend's hash under a fresh flush ID, then returns
// every staged hash, including ones an earlier flush on any instance staged
// and never deleted.
func (r *RedisCounterBackend) TakeShared(ctx context.Context) []core.UsageBatch {
	if r.inFallback() {
		return nil
	}
	r.stage(ctx)
	return r.readStaged(ctx)
}

// Settle deletes a staged hash once the store has applied it. A staged hash
// the store did not apply stays for the next flush; a local batch goes back
// to the local counters.
func (r *RedisCounterBackend) Settle(ctx context.Context, b *core.UsageBatch, applied bool) {
	if b.ID == "" {
		r.local.Settle(ctx, b, applied)
		return
	}
	if !applied || r.inFallback() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	_ = r.observe("unstage", r.client.Del(ctx, r.stagedKey(b.Backend, b.ID)).Err())
}

// -------------------------------------------------------------------------
// STAGING
// -------------------------------------------------------------------------

// stage renames each live hash to a staged key. A charge landing after the
// rename starts a fresh live hash, so nothing is lost between the two. A
// missing hash means the backend was not charged and is skipped.
func (r *RedisCounterBackend) stage(ctx context.Context) {
	for _, backend := range r.backends {
		octx, cancel := context.WithTimeout(ctx, opTimeout)
		err := r.client.Rename(octx, r.hashKey(backend), r.stagedKey(backend, rand.Text())).Err()
		cancel()
		if err != nil && strings.Contains(err.Error(), "no such key") {
			continue
		}
		if r.observe("stage", err) != nil {
			return
		}
	}
}

// readStaged returns a batch for every staged hash. A hash that is gone by the
// time it is read was applied and deleted by another flush.
func (r *RedisCounterBackend) readStaged(ctx context.Context) []core.UsageBatch {
	var batches []core.UsageBatch
	_ = r.scanKeys(ctx, "scan_staged", globEscaper.Replace(r.prefix)+":usage-staged:*", func(key string) error {
		if b, ok := r.readBatch(ctx, key); ok {
			batches = append(batches, b)
		}
		return nil
	})
	return batches
}

// scanKeys calls fn for every key matching match, until the scan ends or fn
// fails. A scan failure is observed as op and returned.
func (r *RedisCounterBackend) scanKeys(ctx context.Context, op, match string, fn func(key string) error) error {
	var cursor uint64
	for {
		octx, cancel := context.WithTimeout(ctx, opTimeout)
		keys, next, err := r.client.Scan(octx, cursor, match, scanCount).Result()
		cancel()
		if err := r.observe(op, err); err != nil {
			return err
		}
		for _, key := range keys {
			if err := fn(key); err != nil {
				return err
			}
		}
		if next == 0 {
			return nil
		}
		cursor = next
	}
}

// readBatch reads one staged hash into a batch, reporting false when the key
// does not name a staged hash or the hash is already gone.
func (r *RedisCounterBackend) readBatch(ctx context.Context, key string) (core.UsageBatch, bool) {
	b, ok := r.parseStagedKey(key)
	if !ok {
		return b, false
	}
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	fields, err := r.client.HGetAll(ctx, key).Result()
	if r.observe("read_staged", err) != nil || len(fields) == 0 {
		return b, false
	}
	parsed := make(map[bucketKey]*core.Usage)
	r.addFields(ctx, fields, func(bucket time.Time, workload string) *core.Usage {
		k := bucketKey{bucket: bucket, workload: workload}
		if parsed[k] == nil {
			parsed[k] = new(core.Usage)
		}
		return parsed[k]
	})
	b.Buckets = make(core.BucketUsage)
	for k, u := range parsed {
		b.Buckets.Add(k.bucket, k.workload, *u)
	}
	return b, true
}

// addFields adds each field of a counter hash into usageFor(its bucket and
// workload), skipping a field usageFor returns nil for. A field that does not
// parse is logged and skipped.
func (r *RedisCounterBackend) addFields(ctx context.Context, fields map[string]string, usageFor func(bucket time.Time, workload string) *core.Usage) {
	for field, raw := range fields {
		v, err := strconv.ParseInt(raw, 10, 64)
		bucket, workload, name, ok := parseField(field)
		if err != nil || !ok {
			r.log.WarnContext(ctx, "discarding unparseable usage counter",
				slog.String("field", field), slog.String("value", raw))
			continue
		}
		u := usageFor(bucket, workload)
		if u == nil {
			continue
		}
		switch name {
		case fieldAPIRequests:
			u.APIRequests += v
		case fieldEgressBytes:
			u.EgressBytes += v
		case fieldIngressBytes:
			u.IngressBytes += v
		default:
			if pool, ok := strings.CutPrefix(name, poolFieldPrefix); ok {
				u.AddPool(pool, v)
			}
		}
	}
}

// -------------------------------------------------------------------------
// FALLBACK
// -------------------------------------------------------------------------

// inFallback reports whether the backend is serving from local counters,
// which it does while the breaker is open.
func (r *RedisCounterBackend) inFallback() bool {
	return !r.cb.IsHealthy()
}

// onStateChange moves the gauge and the log with the breaker: open means
// local counters, closed means Redis again. The charges kept locally
// meanwhile need nothing here: the usage flush writes them to the store.
func (r *RedisCounterBackend) onStateChange(info breaker.StateChangeInfo) {
	if info.To == breaker.StateClosed {
		telemetry.RedisFallbackActive.Set(0)
		r.log.InfoContext(context.Background(), "Redis counter backend recovered")
		return
	}
	telemetry.RedisFallbackActive.Set(1)
	r.log.WarnContext(context.Background(), "Redis counter backend entering fallback to local counters")
}

// observe records one Redis operation's outcome on the operations metric and
// the circuit breaker, and returns err so the caller can fall back on it.
// redis.Nil, a key that does not exist, counts as success.
func (r *RedisCounterBackend) observe(op string, err error) error {
	if errors.Is(err, redis.Nil) {
		err = nil
	}
	status := "success"
	if err != nil {
		status = "error"
	}
	telemetry.RedisOperationsTotal.WithLabelValues(op, status).Inc()
	_ = r.cb.PostCheck(err)
	return err
}

// -------------------------------------------------------------------------
// KEY HELPERS
// -------------------------------------------------------------------------

// hashKey returns the live counter hash for a backend, holding every bucket it
// has been charged in since its last flush.
func (r *RedisCounterBackend) hashKey(backend string) string {
	return fmt.Sprintf("%s:usage:%s", r.prefix, backend)
}

// stagedKey returns the key a backend's hash is staged under for one flush,
// renamed from the live hash so later charges start a fresh one.
func (r *RedisCounterBackend) stagedKey(backend, flushID string) string {
	return fmt.Sprintf("%s:usage-staged:%s:%s", r.prefix, backend, flushID)
}

// parseStagedKey recovers the batch a staged key names. The flush ID holds no
// colon, so a backend name containing one still parses.
func (r *RedisCounterBackend) parseStagedKey(key string) (core.UsageBatch, bool) {
	rest, ok := strings.CutPrefix(key, r.prefix+":usage-staged:")
	sep := strings.LastIndexByte(rest, ':')
	if !ok || sep <= 0 || sep == len(rest)-1 {
		return core.UsageBatch{}, false
	}
	return core.UsageBatch{ID: rest[sep+1:], Backend: rest[:sep]}, true
}

// sharedKey returns the Redis key for a named piece of shared state.
func (r *RedisCounterBackend) sharedKey(name string) string {
	return fmt.Sprintf("%s:shared:%s", r.prefix, name)
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
	return r.observe("set", r.client.Set(ctx, r.sharedKey(name), value, ttl).Err())
}

// GetShared returns the value stored under name, or nil when there is none.
func (r *RedisCounterBackend) GetShared(ctx context.Context, name string) ([]byte, error) {
	if r.inFallback() {
		return nil, ErrSharedStateUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	val, err := r.client.Get(ctx, r.sharedKey(name)).Bytes()
	if err := r.observe("get", err); err != nil {
		return nil, err
	}
	return val, nil
}

// ListShared returns every value stored under a name starting with prefix,
// keyed by its full name. A value that expires between the scan and the read
// is left out.
func (r *RedisCounterBackend) ListShared(ctx context.Context, prefix string) (map[string][]byte, error) {
	if r.inFallback() {
		return nil, ErrSharedStateUnavailable
	}
	out := make(map[string][]byte)
	err := r.scanKeys(ctx, "scan_shared", globEscaper.Replace(r.sharedKey(prefix))+"*", func(key string) error {
		name := strings.TrimPrefix(key, r.sharedKey(""))
		val, err := r.GetShared(ctx, name)
		if val != nil {
			out[name] = val
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PublishShared sends payload to every instance watching channel. An instance
// that is not subscribed at that moment never receives it.
func (r *RedisCounterBackend) PublishShared(ctx context.Context, channel string, payload []byte) error {
	if r.inFallback() {
		return ErrSharedStateUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	return r.observe("publish", r.client.Publish(ctx, r.sharedKey(channel), payload).Err())
}

// WatchShared calls onMessage with the payload of every message on channel,
// and with a nil payload each time the subscription is established: at start
// and after every reconnect. Pub/sub keeps nothing for a subscriber that was
// away, so a nil payload tells the watcher it may have missed messages.
// Blocks until ctx is done.
func (r *RedisCounterBackend) WatchShared(ctx context.Context, channel string, onMessage func(ctx context.Context, payload []byte)) {
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

		switch m := msg.(type) {
		case *redis.Subscription:
			if m.Kind != "subscribe" {
				continue
			}
			if interrupted {
				r.log.InfoContext(ctx, "shared state subscription restored", "channel", channel)
				interrupted = false
			}
			onMessage(ctx, nil)
		case *redis.Message:
			onMessage(ctx, []byte(m.Payload))
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
		r.log.WarnContext(ctx, "shared state subscription lost, resubscribing",
			"channel", channel, logfmt.Err(err))
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(watchRetryDelay):
		return true
	}
}

// -------------------------------------------------------------------------
// LIFECYCLE
// -------------------------------------------------------------------------

// Close closes the Redis client.
func (r *RedisCounterBackend) Close() error {
	return r.client.Close()
}
