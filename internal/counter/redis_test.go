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
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
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

func (f *fakePipeliner) IncrBy(_ context.Context, key string, val int64) *redis.IntCmd {
	f.incrByKeys = append(f.incrByKeys, key)
	f.incrByVals = append(f.incrByVals, val)
	return redis.NewIntCmd(context.Background())
}

func (f *fakePipeliner) Expire(_ context.Context, key string, _ time.Duration) *redis.BoolCmd {
	f.expireKeys = append(f.expireKeys, key)
	return redis.NewBoolCmd(context.Background())
}

func (f *fakePipeliner) Exec(_ context.Context) ([]redis.Cmder, error) {
	return nil, f.execErr
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
	// Trip the circuit so tryRecover's PostCheck(nil) can close it.
	_ = cb.PostCheck(errors.New("trigger"))
	return cb
}
