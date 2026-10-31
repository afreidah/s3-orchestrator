// -------------------------------------------------------------------------------
// Redis Counter Recovery Tests
//
// Author: Alex Freidah
//
// Tests for the constructor, shared state, the circuit breaker feed, and
// what the backend does when its breaker's prober recovers it.
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

// recoveringBackend builds a backend in fallback over the mock client, its
// breaker open the way an outage leaves it.
func recoveringBackend(mock *MockRedisClient) *RedisCounterBackend {
	r := &RedisCounterBackend{
		client:   mock,
		prefix:   testPrefix,
		local:    NewLocalCounterBackend([]string{backendB1}),
		backends: []string{backendB1},
		cb:       newTestCB(),
		log:      slog.Default(),
	}
	r.cb.AddOnStateChange(r.onStateChange)
	r.cb.Trip(errors.New("outage"))
	return r
}

// TestNewRedisCounterBackend_HappyPath drives the production constructor so
// the log-field assignment and the "initialized" info log run under coverage.
// The mock Redis client makes Ping succeed.
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
	r.Charge(backendB1, oldBucket, Charge{APIRequests: 5})
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

// TestObserve_FailureOpensFallback verifies that a failed operation reaching
// the breaker's threshold moves the backend into fallback, and that a
// redis.Nil answer counts as success.
func TestObserve_FailureOpensFallback(t *testing.T) {
	r := &RedisCounterBackend{cb: newTestCB(), local: NewLocalCounterBackend([]string{"b1"}), log: slog.Default()}
	if err := r.observe("get", redis.Nil); err != nil || r.inFallback() {
		t.Fatalf("observe(redis.Nil) = %v, fallback %v; want nil and no fallback", err, r.inFallback())
	}
	if err := r.observe("get", errors.New("boom")); err == nil || !r.inFallback() {
		t.Fatalf("observe(boom) = %v, fallback %v; want the error and fallback", err, r.inFallback())
	}
}

// TestRecovery_LeavesLocalChargesForTheFlush asserts recovery only leaves
// fallback: the charges kept locally stay put until this instance's flush
// takes them, and nothing is written to Redis.
func TestRecovery_LeavesLocalChargesForTheFlush(t *testing.T) {
	r := recoveringBackend(NewMockRedisClient(gomock.NewController(t)))
	r.Charge(backendB1, oldBucket, Charge{APIRequests: 100, EgressBytes: 1024})

	r.cb.Recover()

	if r.inFallback() {
		t.Error("expected fallback to be cleared after recovery")
	}
	batches := r.Take(context.Background())
	if len(batches) != 1 || totalOf(batches[0]).APIRequests != 100 || batches[0].ID != "" {
		t.Errorf("Take after recovery = %+v, want the local batch of 100 with no ID", batches)
	}
}

// newTestCB creates a closed circuit breaker that opens on the first failure.
func newTestCB() *breaker.CircuitBreaker {
	return breaker.NewCircuitBreaker(breaker.Config{Name: "test-redis", Threshold: 1, Timeout: time.Millisecond, IsError: func(err error) bool { return err != nil }, Sentinel: errors.New("redis unavailable")})
}
