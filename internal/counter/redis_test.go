// -------------------------------------------------------------------------------
// Redis Counter Recovery Tests
//
// Author: Alex Freidah
//
// Tests for the tryRecover path: fallback to local counters during Redis
// outage, atomic swap of all local deltas, additive INCRBY pipeline on
// recovery, and delta restoration on pipeline failure.
// -------------------------------------------------------------------------------

package counter

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
)

// fakePipeliner collects pipeline commands without executing them against
// a real Redis. Exec returns a configurable error.
type fakePipeliner struct {
	redis.Pipeliner
	incrByKeys []string
	incrByVals []int64
	expireKeys []string
	execErr    error
}

// IncrBy satisfies the redis pipeline interface for the fake
// pipeliner used in this test; records the call for later
// assertions.
func (f *fakePipeliner) IncrBy(_ context.Context, key string, val int64) *redis.IntCmd {
	f.incrByKeys = append(f.incrByKeys, key)
	f.incrByVals = append(f.incrByVals, val)
	return redis.NewIntCmd(context.Background())
}

// Expire satisfies the redis pipeline interface for the fake
// pipeliner; records the requested TTL so the test can assert it.
func (f *fakePipeliner) Expire(_ context.Context, key string, _ time.Duration) *redis.BoolCmd {
	f.expireKeys = append(f.expireKeys, key)
	return redis.NewBoolCmd(context.Background())
}

// Exec satisfies the redis pipeline interface for the fake
// pipeliner; returns the test-configured error and slice of cmder
// results so error-path assertions are deterministic.
func (f *fakePipeliner) Exec(_ context.Context) ([]redis.Cmder, error) {
	return nil, f.execErr
}

// TestRedisCounterBackend_LoggerFallback pins the nil-safe behaviour
// of logger(): tests construct *RedisCounterBackend directly without
// setting log, and the helper must return slog.Default() so the Redis
// code path never dereferences a nil logger.
func TestRedisCounterBackend_LoggerFallback(t *testing.T) {
	if (&RedisCounterBackend{}).logger() == nil {
		t.Fatal("logger() returned nil for zero-value RedisCounterBackend")
	}
}

// TestRedisCounterBackend_LoggerReturnsCustomLog covers the non-nil
// branch of logger(): when the backend was constructed by
// NewRedisCounterBackend the log field is populated and the helper
// returns it unchanged.
func TestRedisCounterBackend_LoggerReturnsCustomLog(t *testing.T) {
	custom := slog.Default().With("scope", "test")
	r := &RedisCounterBackend{log: custom}
	if r.logger() != custom {
		t.Fatal("logger() did not return the assigned log field")
	}
}

// TestNewRedisCounterBackend_HappyPath drives the production
// constructor so the log-field assignment, the "initialized" info log,
// and the background probe spawn each run under coverage. The mock
// Redis client makes Ping succeed; the probe goroutine exits on Close.
func TestNewRedisCounterBackend_HappyPath(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockRedisClient(ctrl)
	mock.EXPECT().Ping(gomock.Any()).Return(redis.NewStatusResult("PONG", nil))
	mock.EXPECT().Close().Return(nil)

	cfg := &config.RedisConfig{
		Address:          "127.0.0.1:6379",
		KeyPrefix:        "test",
		FailureThreshold: 3,
		OpenTimeout:      time.Second,
	}
	r, err := NewRedisCounterBackend(mock, cfg, []string{"b1"})
	if err != nil {
		t.Fatalf("NewRedisCounterBackend: %v", err)
	}
	if r == nil {
		t.Fatal("NewRedisCounterBackend returned nil")
	}
	if r.log == nil {
		t.Fatal("log field left nil")
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestRecordFailure_LogsFallbackAndPostCheckError drives both warning
// branches that the previous fallback contract left untested: when the
// circuit breaker opens, PostCheck returns the sentinel (covering the
// "Redis circuit breaker PostCheck reported error" log), and recordFailure
// transitions to fallback (covering the "entering fallback to local
// counters" log).
func TestRecordFailure_LogsFallbackAndPostCheckError(t *testing.T) {
	sentinel := errors.New("redis unavailable")
	cb := breaker.NewCircuitBreaker("redis", 1, time.Second,
		func(error) bool { return true }, sentinel)

	r := &RedisCounterBackend{
		cb:    cb,
		local: NewLocalCounterBackend([]string{"b1"}),
		log:   slog.Default(),
	}
	r.recordFailure(errors.New("boom"))
	if !r.inFallback() {
		t.Fatal("expected fallback after breaker opened")
	}
}

// TestTryRecover_SyncsLocalDeltasToRedis verifies that tryRecover swaps all
// local deltas atomically and sends them to Redis via INCRBY (no DEL).
func TestTryRecover_SyncsLocalDeltasToRedis(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockRedisClient(ctrl)
	pipe := &fakePipeliner{}

	mock.EXPECT().Ping(gomock.Any()).Return(redis.NewStatusResult("PONG", nil))
	mock.EXPECT().Pipeline().Return(pipe)

	r := &RedisCounterBackend{
		client:    mock,
		prefix:    "test",
		local:     NewLocalCounterBackend([]string{"b1", "b2"}),
		backends:  []string{"b1", "b2"},
		fallback:  true,
		cb:        newTestCB(),
		stopProbe: make(chan struct{}),
		probeDone: make(chan struct{}),
	}

	// Accumulate local deltas during "outage"
	r.local.AddAll("b1", 100, 1024, 2048)
	r.local.AddAll("b2", 50, 512, 0)

	r.tryRecover()

	// Local counters should be zeroed.
	if got := r.local.Load("b1", FieldAPIRequests); got != 0 {
		t.Errorf("b1 apiRequests after recovery = %d, want 0", got)
	}
	if got := r.local.Load("b2", FieldAPIRequests); got != 0 {
		t.Errorf("b2 apiRequests after recovery = %d, want 0", got)
	}

	// Pipeline should have INCRBY calls (no DEL).
	if len(pipe.incrByKeys) == 0 {
		t.Fatal("expected INCRBY pipeline commands")
	}

	// Should no longer be in fallback.
	if r.inFallback() {
		t.Error("expected fallback to be cleared after recovery")
	}
}

// TestTryRecover_PipelineFailure_RestoresDeltas verifies that if the Redis
// pipeline fails, local deltas are restored so they can be retried.
func TestTryRecover_PipelineFailure_RestoresDeltas(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockRedisClient(ctrl)
	pipe := &fakePipeliner{execErr: errors.New("connection reset")}

	mock.EXPECT().Ping(gomock.Any()).Return(redis.NewStatusResult("PONG", nil))
	mock.EXPECT().Pipeline().Return(pipe)

	r := &RedisCounterBackend{
		client:    mock,
		prefix:    "test",
		local:     NewLocalCounterBackend([]string{"b1"}),
		backends:  []string{"b1"},
		fallback:  true,
		cb:        newTestCB(),
		stopProbe: make(chan struct{}),
		probeDone: make(chan struct{}),
	}

	r.local.AddAll("b1", 100, 1024, 2048)

	r.tryRecover()

	// Deltas should be restored to local counters.
	if got := r.local.Load("b1", FieldAPIRequests); got != 100 {
		t.Errorf("b1 apiRequests after failed recovery = %d, want 100", got)
	}
	if got := r.local.Load("b1", FieldEgressBytes); got != 1024 {
		t.Errorf("b1 egressBytes after failed recovery = %d, want 1024", got)
	}

	// Should still be in fallback.
	if !r.inFallback() {
		t.Error("expected fallback to remain active after pipeline failure")
	}
}

// TestTryRecover_PingFailure_NoOp verifies that tryRecover does nothing when
// Redis is still unreachable (Ping fails).
func TestTryRecover_PingFailure_NoOp(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockRedisClient(ctrl)

	mock.EXPECT().Ping(gomock.Any()).Return(redis.NewStatusResult("", errors.New("connection refused")))

	r := &RedisCounterBackend{
		client:    mock,
		prefix:    "test",
		local:     NewLocalCounterBackend([]string{"b1"}),
		backends:  []string{"b1"},
		fallback:  true,
		cb:        newTestCB(),
		stopProbe: make(chan struct{}),
		probeDone: make(chan struct{}),
	}

	r.local.AddAll("b1", 50, 0, 0)

	r.tryRecover()

	// Local deltas should be untouched.
	if got := r.local.Load("b1", FieldAPIRequests); got != 50 {
		t.Errorf("b1 apiRequests = %d, want 50 (unchanged)", got)
	}

	// Should still be in fallback.
	if !r.inFallback() {
		t.Error("expected fallback to remain active when ping fails")
	}
}

// TestTryRecover_NoDEL verifies that the recovery pipeline does not contain
// any DEL commands (INCRBY only, safe for multi-instance).
func TestTryRecover_NoDEL(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockRedisClient(ctrl)
	pipe := &fakePipeliner{}

	mock.EXPECT().Ping(gomock.Any()).Return(redis.NewStatusResult("PONG", nil))
	mock.EXPECT().Pipeline().Return(pipe)
	// Explicitly expect NO Del calls on the mock client.
	// (Del would be called on the pipeline, not the client, but verify
	// the pipeline has no Del method calls by checking only INCRBY keys.)

	r := &RedisCounterBackend{
		client:    mock,
		prefix:    "test",
		local:     NewLocalCounterBackend([]string{"b1"}),
		backends:  []string{"b1"},
		fallback:  true,
		cb:        newTestCB(),
		stopProbe: make(chan struct{}),
		probeDone: make(chan struct{}),
	}

	r.local.AddAll("b1", 10, 20, 30)

	r.tryRecover()

	// All pipeline commands should be INCRBY (tracked via incrByKeys).
	// If DEL were present, it would go through a different method not tracked.
	if len(pipe.incrByKeys) != 3 {
		t.Errorf("expected 3 INCRBY commands (api, egress, ingress), got %d", len(pipe.incrByKeys))
	}
}

// newTestCB creates a circuit breaker in the open state for recovery tests.
func newTestCB() *breaker.CircuitBreaker {
	cb := breaker.NewCircuitBreaker("test-redis", 1, time.Millisecond,
		func(error) bool { return true }, errors.New("redis unavailable"))
	// Trip the circuit so tryRecover's recovery transition can close it.
	_ = cb.PostCheck(errors.New("trigger"))
	return cb
}

// TestTryRecover_ClosesCircuitBreaker pins the contract that tryRecover
// transitions the breaker out of Open back to a healthy Closed state.
// The redis counter hot-path methods bypass PreCheck (they branch on
// inFallback()), so the breaker never reaches HalfOpen on its own. If
// tryRecover does not actively close the breaker, IsHealthy() stays
// false forever and recordFailure flips the system back to fallback on
// the very next transient error.
func TestTryRecover_ClosesCircuitBreaker(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockRedisClient(ctrl)
	pipe := &fakePipeliner{}

	mock.EXPECT().Ping(gomock.Any()).Return(redis.NewStatusResult("PONG", nil))
	mock.EXPECT().Pipeline().Return(pipe)

	r := &RedisCounterBackend{
		client:    mock,
		prefix:    "test",
		local:     NewLocalCounterBackend([]string{"b1"}),
		backends:  []string{"b1"},
		fallback:  true,
		cb:        newTestCB(),
		stopProbe: make(chan struct{}),
		probeDone: make(chan struct{}),
	}

	if r.cb.IsHealthy() {
		t.Fatal("setup: breaker should be open before tryRecover")
	}

	r.tryRecover()

	if !r.cb.IsHealthy() {
		t.Errorf("breaker still unhealthy after tryRecover; recovery probe must close it cleanly, not via PostCheck(nil)")
	}
	if r.inFallback() {
		t.Error("fallback flag should be cleared after recovery")
	}
}

// TestTryRecover_TolerantOfTransientErrorAfterRecovery pins the contract
// that the failure counter is reset by recovery so the breaker tolerates
// new transient errors up to its threshold. Without a clean recovery
// transition the failure counter would still equal the threshold; the
// next error would re-trip the breaker immediately.
func TestTryRecover_TolerantOfTransientErrorAfterRecovery(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockRedisClient(ctrl)
	pipe := &fakePipeliner{}

	mock.EXPECT().Ping(gomock.Any()).Return(redis.NewStatusResult("PONG", nil))
	mock.EXPECT().Pipeline().Return(pipe)

	// Use a threshold of 3 so the test can prove the failure counter
	// was reset (one post-recovery error must not trip a 3-strike
	// breaker).
	cb := breaker.NewCircuitBreaker("test-redis", 3, time.Millisecond,
		func(error) bool { return true }, errors.New("redis unavailable"))
	for range 3 {
		_ = cb.PostCheck(errors.New("outage"))
	}

	r := &RedisCounterBackend{
		client:    mock,
		prefix:    "test",
		local:     NewLocalCounterBackend([]string{"b1"}),
		backends:  []string{"b1"},
		fallback:  true,
		cb:        cb,
		stopProbe: make(chan struct{}),
		probeDone: make(chan struct{}),
	}

	r.tryRecover()

	if !r.cb.IsHealthy() {
		t.Fatal("setup: tryRecover should have closed the breaker")
	}

	// One transient error must NOT immediately re-open the breaker;
	// the failure counter must have been zeroed by recovery so the
	// breaker tolerates the configured threshold (3) of new failures.
	_ = r.cb.PostCheck(errors.New("transient"))
	if !r.cb.IsHealthy() {
		t.Errorf("a single failure post-recovery re-opened the breaker; failure counter not reset by recovery")
	}
}
