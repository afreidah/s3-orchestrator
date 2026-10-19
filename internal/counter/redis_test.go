// -------------------------------------------------------------------------------
// Redis Counter Recovery Tests
//
// Author: Alex Freidah
//
// Tests for the constructor, shared state, the circuit breaker feed, and the
// tryRecover path that moves the backend off its local fallback once Redis
// answers again.
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

// recoveringBackend builds a backend in fallback over the mock client, the
// state tryRecover runs in.
func recoveringBackend(mock *MockRedisClient, cb *breaker.CircuitBreaker) *RedisCounterBackend {
	r := &RedisCounterBackend{
		client:    mock,
		prefix:    testPrefix,
		local:     NewLocalCounterBackend([]string{backendB1}),
		backends:  []string{backendB1},
		cb:        cb,
		stopProbe: make(chan struct{}),
		probeDone: make(chan struct{}),
	}
	r.setFallback(true)
	return r
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
	r := NewRedisCounterBackend(mock, cfg, []string{"b1"})
	if r.log == nil {
		t.Fatal("log field left nil")
	}
	if r.inFallback() {
		t.Error("backend started in fallback with Redis reachable")
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestNewRedisCounterBackend_UnreachableStartsInFallback pins the boot
// behaviour: Redis down at startup is the same condition as Redis failing
// while running, so the backend comes up on local counters rather than
// failing the service or being left out of it.
func TestNewRedisCounterBackend_UnreachableStartsInFallback(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockRedisClient(ctrl)
	mock.EXPECT().Ping(gomock.Any()).Return(redis.NewStatusResult("", errors.New("connection refused"))).AnyTimes()
	mock.EXPECT().Close().Return(nil)

	cfg := &config.RedisConfig{
		Address:          "127.0.0.1:6379",
		KeyPrefix:        "test",
		FailureThreshold: 3,
		OpenTimeout:      time.Second,
	}
	r := NewRedisCounterBackend(mock, cfg, []string{"b1"})
	defer func() { _ = r.Close() }()

	if !r.inFallback() {
		t.Fatal("backend not in fallback with Redis unreachable at startup")
	}
	r.Charge(backendB1, Charge{APIRequests: 5})
	if got := loaded(r, backendB1).APIRequests; got != 5 {
		t.Errorf("Load = %d from the local counters, want 5", got)
	}
	if got := r.TakeShared(context.Background()); got != nil {
		t.Errorf("TakeShared in fallback = %+v, want nil", got)
	}
	if _, err := r.GetShared(context.Background(), "x"); !errors.Is(err, ErrSharedStateUnavailable) {
		t.Errorf("GetShared err = %v, want ErrSharedStateUnavailable", err)
	}
}

// TestSharedState_PutAndGet covers the shared-state round trip, a missing
// key reading as nil, and a Redis error surfacing to the caller.
func TestSharedState_PutAndGet(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockRedisClient(ctrl)
	newBackend := func() *RedisCounterBackend {
		return &RedisCounterBackend{
			client: mock,
			prefix: "test",
			local:  NewLocalCounterBackend([]string{"b1"}),
			cb:     newTestCB(),
		}
	}
	r := newBackend()
	ctx := context.Background()

	mock.EXPECT().Set(gomock.Any(), "test:shared:snap", []byte("v1"), time.Minute).
		Return(redis.NewStatusResult("OK", nil))
	if err := r.PutShared(ctx, "snap", []byte("v1"), time.Minute); err != nil {
		t.Fatalf("PutShared: %v", err)
	}

	mock.EXPECT().Get(gomock.Any(), "test:shared:snap").Return(redis.NewStringResult("v1", nil))
	if got, err := r.GetShared(ctx, "snap"); err != nil || string(got) != "v1" {
		t.Errorf("GetShared = %q, %v; want v1", got, err)
	}

	mock.EXPECT().Get(gomock.Any(), "test:shared:missing").Return(redis.NewStringResult("", redis.Nil))
	if got, err := r.GetShared(ctx, "missing"); err != nil || got != nil {
		t.Errorf("GetShared(missing) = %q, %v; want nil, nil", got, err)
	}

	// A failure can trip the breaker, so each error case gets its own backend.
	boom := errors.New("redis down")
	mock.EXPECT().Get(gomock.Any(), "test:shared:snap").Return(redis.NewStringResult("", boom))
	if _, err := newBackend().GetShared(ctx, "snap"); !errors.Is(err, boom) {
		t.Errorf("GetShared err = %v, want %v", err, boom)
	}
	mock.EXPECT().Set(gomock.Any(), "test:shared:snap", []byte("v2"), time.Minute).
		Return(redis.NewStatusResult("", boom))
	if err := newBackend().PutShared(ctx, "snap", []byte("v2"), time.Minute); !errors.Is(err, boom) {
		t.Errorf("PutShared err = %v, want %v", err, boom)
	}
}

// TestRecordFailure_LogsFallbackAndPostCheckError verifies that a failure
// opening the breaker logs the PostCheck error and moves the backend into
// fallback.
func TestRecordFailure_LogsFallbackAndPostCheckError(t *testing.T) {
	sentinel := errors.New("redis unavailable")
	cb := breaker.NewCircuitBreaker(breaker.Config{Name: "redis", Threshold: 1, Timeout: time.Second, IsError: func(error) bool { return true }, Sentinel: sentinel})

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

// TestTryRecover_LeavesLocalChargesForTheFlush asserts recovery only leaves
// fallback: the charges kept locally stay put until this instance's flush
// takes them, and nothing is written to Redis.
func TestTryRecover_LeavesLocalChargesForTheFlush(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockRedisClient(ctrl)
	mock.EXPECT().Ping(gomock.Any()).Return(redis.NewStatusResult("PONG", nil))

	r := recoveringBackend(mock, newTestCB())
	r.Charge(backendB1, Charge{APIRequests: 100, EgressBytes: 1024})

	r.tryRecover()

	if r.inFallback() {
		t.Error("expected fallback to be cleared after recovery")
	}
	batches := r.Take(context.Background())
	if len(batches) != 1 || batches[0].APIRequests != 100 || batches[0].ID != "" {
		t.Errorf("Take after recovery = %+v, want the local batch of 100 with no ID", batches)
	}
}

// TestTryRecover_PingFailure_NoOp verifies that tryRecover does nothing when
// Redis is still unreachable (Ping fails).
func TestTryRecover_PingFailure_NoOp(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockRedisClient(ctrl)
	mock.EXPECT().Ping(gomock.Any()).Return(redis.NewStatusResult("", errors.New("connection refused")))

	r := recoveringBackend(mock, newTestCB())
	r.Charge(backendB1, Charge{APIRequests: 50})

	r.tryRecover()

	if got := loaded(r, backendB1).APIRequests; got != 50 {
		t.Errorf("b1 apiRequests = %d, want 50 (unchanged)", got)
	}
	if !r.inFallback() {
		t.Error("expected fallback to remain active when ping fails")
	}
}

// newTestCB creates a circuit breaker in the open state for recovery tests.
func newTestCB() *breaker.CircuitBreaker {
	cb := breaker.NewCircuitBreaker(breaker.Config{Name: "test-redis", Threshold: 1, Timeout: time.Millisecond, IsError: func(error) bool { return true }, Sentinel: errors.New("redis unavailable")})
	// Trip the circuit so tryRecover's recovery transition can close it.
	_ = cb.PostCheck(errors.New("trigger"))
	return cb
}

// TestTryRecover_ClosesCircuitBreaker verifies tryRecover closes an open
// breaker. The hot path bypasses PreCheck, so nothing else would close it.
func TestTryRecover_ClosesCircuitBreaker(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockRedisClient(ctrl)
	mock.EXPECT().Ping(gomock.Any()).Return(redis.NewStatusResult("PONG", nil))

	r := recoveringBackend(mock, newTestCB())

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
	mock.EXPECT().Ping(gomock.Any()).Return(redis.NewStatusResult("PONG", nil))

	// Use a threshold of 3 so the test can prove the failure counter
	// was reset (one post-recovery error must not trip a 3-strike
	// breaker).
	cb := breaker.NewCircuitBreaker(breaker.Config{Name: "test-redis", Threshold: 3, Timeout: time.Millisecond, IsError: func(error) bool { return true }, Sentinel: errors.New("redis unavailable")})
	for range 3 {
		_ = cb.PostCheck(errors.New("outage"))
	}

	r := recoveringBackend(mock, cb)

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
