// -------------------------------------------------------------------------------
// Redis Pool Counter Tests
//
// Author: Alex Freidah
//
// Per-pool request counters live in one hash per backend and period, which is
// what lets a flush enumerate the pools actually charged without scanning the
// keyspace. These tests pin that layout and the fallback contract every other
// Redis path already follows: a failed command degrades to the local counters
// rather than dropping the charge.
// -------------------------------------------------------------------------------

package counter

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/redis/go-redis/v9"
	"go.uber.org/mock/gomock"
)

// newPoolBackend builds a Redis counter backend over the mock client, wired
// the way the pool paths need: a local fallback and a closed breaker.
func newPoolBackend(t *testing.T, mock *MockRedisClient) *RedisCounterBackend {
	t.Helper()
	return &RedisCounterBackend{
		client:    mock,
		prefix:    "test",
		local:     NewLocalCounterBackend([]string{"b1"}),
		backends:  []string{"b1"},
		cb:        newTestCB(),
		log:       slog.Default(),
		stopProbe: make(chan struct{}),
		probeDone: make(chan struct{}),
	}
}

// TestAddPools_PipelinesOneHashPerBackend pins the key layout: every pool for
// a backend is a field in one hash, so a flush can read them all at once.
func TestAddPools_PipelinesOneHashPerBackend(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockRedisClient(ctrl)
	pipe := &fakePipeliner{}
	mock.EXPECT().Pipeline().Return(pipe)

	r := newPoolBackend(t, mock)
	r.AddPools("b1", map[string]int64{"class_a": 3, "class_b": 1})

	if len(pipe.hIncrByKeys) != 2 {
		t.Fatalf("HIncrBy calls = %d, want 2", len(pipe.hIncrByKeys))
	}
	want := r.poolKey("b1")
	for i, key := range pipe.hIncrByKeys {
		if key != want {
			t.Errorf("HIncrBy[%d] key = %q, want %q", i, key, want)
		}
	}
	if len(pipe.expireKeys) != 1 || pipe.expireKeys[0] != want {
		t.Errorf("expire keys = %v, want one TTL on %q", pipe.expireKeys, want)
	}
}

// TestAddPools_EmptyDeltasTouchesRedis covers the common case of an operation
// that charges no pool: it must not open a pipeline at all. The mock fails the
// test if Pipeline is called, since no call is expected.
func TestAddPools_EmptyDeltasTouchesRedis(t *testing.T) {
	ctrl := gomock.NewController(t)
	r := newPoolBackend(t, NewMockRedisClient(ctrl))
	r.AddPools("b1", nil)
}

// TestAddPools_FallsBackToLocalOnError is the durability contract: a Redis
// failure must leave the charge in the local counters, where tryRecover
// replays it, rather than dropping it.
func TestAddPools_FallsBackToLocalOnError(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockRedisClient(ctrl)
	mock.EXPECT().Pipeline().Return(&fakePipeliner{execErr: errors.New("connection reset")})

	r := newPoolBackend(t, mock)
	r.AddPools("b1", map[string]int64{"class_a": 5})

	if got := r.local.LoadPool("b1", "class_a"); got != 5 {
		t.Errorf("local pool counter = %d, want 5 after the Redis write failed", got)
	}
}

// TestAddPools_UsesLocalWhileInFallback covers the outage path, where the
// pipeline is never attempted.
func TestAddPools_UsesLocalWhileInFallback(t *testing.T) {
	ctrl := gomock.NewController(t)
	r := newPoolBackend(t, NewMockRedisClient(ctrl))
	r.setFallback(true)

	r.AddPools("b1", map[string]int64{"class_a": 2})
	if got := r.local.LoadPool("b1", "class_a"); got != 2 {
		t.Errorf("local pool counter = %d, want 2 while in fallback", got)
	}
}

// TestLoadPool_ReadsTheHashField covers the admission read.
func TestLoadPool_ReadsTheHashField(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockRedisClient(ctrl)
	mock.EXPECT().HGet(gomock.Any(), gomock.Any(), "class_a").
		Return(redis.NewStringResult("42", nil))

	r := newPoolBackend(t, mock)
	if got := r.LoadPool("b1", "class_a"); got != 42 {
		t.Errorf("LoadPool = %d, want 42", got)
	}
}

// TestLoadPool_MissingFieldIsZero pins the first charge of a period: the field
// does not exist yet, and redis.Nil must read as zero rather than as an error.
func TestLoadPool_MissingFieldIsZero(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockRedisClient(ctrl)
	mock.EXPECT().HGet(gomock.Any(), gomock.Any(), "class_a").
		Return(redis.NewStringResult("", redis.Nil))

	r := newPoolBackend(t, mock)
	if got := r.LoadPool("b1", "class_a"); got != 0 {
		t.Errorf("LoadPool = %d, want 0 for a pool not yet charged", got)
	}
}

// TestLoadPool_FallsBackToLocalOnError keeps admission answerable during an
// outage: the local counters hold what this process has spent.
func TestLoadPool_FallsBackToLocalOnError(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockRedisClient(ctrl)
	mock.EXPECT().HGet(gomock.Any(), gomock.Any(), "class_a").
		Return(redis.NewStringResult("", errors.New("connection reset")))

	r := newPoolBackend(t, mock)
	r.local.AddPools("b1", map[string]int64{"class_a": 9})

	if got := r.LoadPool("b1", "class_a"); got != 9 {
		t.Errorf("LoadPool = %d, want the local value of 9", got)
	}
}

// TestLoadPool_UsesLocalWhileInFallback covers the short-circuit.
func TestLoadPool_UsesLocalWhileInFallback(t *testing.T) {
	ctrl := gomock.NewController(t)
	r := newPoolBackend(t, NewMockRedisClient(ctrl))
	r.setFallback(true)
	r.local.AddPools("b1", map[string]int64{"class_a": 4})

	if got := r.LoadPool("b1", "class_a"); got != 4 {
		t.Errorf("LoadPool = %d, want 4 while in fallback", got)
	}
}

// TestSwapPools_ReadsAndClearsTheHash covers the flush read. The read and the
// delete run in one transaction so a charge landing between them is not lost.
func TestSwapPools_ReadsAndClearsTheHash(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockRedisClient(ctrl)
	pipe := &fakePipeliner{hgetAll: map[string]string{"class_a": "12", "class_b": "7"}}
	mock.EXPECT().TxPipeline().Return(pipe)

	r := newPoolBackend(t, mock)
	got := r.SwapPools("b1")

	if got["class_a"] != 12 || got["class_b"] != 7 {
		t.Errorf("SwapPools = %v, want class_a 12 and class_b 7", got)
	}
	if len(pipe.delKeys) != 1 || pipe.delKeys[0] != r.poolKey("b1") {
		t.Errorf("deleted keys = %v, want the backend's pool hash", pipe.delKeys)
	}
}

// TestSwapPools_EmptyHashIsNil covers a period with no pool activity, which
// must not flush an empty map.
func TestSwapPools_EmptyHashIsNil(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockRedisClient(ctrl)
	mock.EXPECT().TxPipeline().Return(&fakePipeliner{hgetAll: map[string]string{}})

	r := newPoolBackend(t, mock)
	if got := r.SwapPools("b1"); got != nil {
		t.Errorf("SwapPools = %v, want nil for an untouched period", got)
	}
}

// TestSwapPools_DiscardsUnparseableValue keeps one corrupt field from failing
// the whole flush: the rest of the backend's pools still reach the database.
func TestSwapPools_DiscardsUnparseableValue(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockRedisClient(ctrl)
	mock.EXPECT().TxPipeline().Return(&fakePipeliner{
		hgetAll: map[string]string{"class_a": "5", "class_b": "not-a-number"},
	})

	r := newPoolBackend(t, mock)
	got := r.SwapPools("b1")

	if len(got) != 1 || got["class_a"] != 5 {
		t.Errorf("SwapPools = %v, want only the parseable pool", got)
	}
}

// TestSwapPools_FallsBackToLocalOnError covers the outage path on the flush
// side, where the local counters are the ones holding deltas.
func TestSwapPools_FallsBackToLocalOnError(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockRedisClient(ctrl)
	mock.EXPECT().TxPipeline().Return(&fakePipeliner{execErr: errors.New("connection reset")})

	r := newPoolBackend(t, mock)
	r.local.AddPools("b1", map[string]int64{"class_a": 3})

	got := r.SwapPools("b1")
	if got["class_a"] != 3 {
		t.Errorf("SwapPools = %v, want the local deltas", got)
	}
	if left := r.local.LoadPool("b1", "class_a"); left != 0 {
		t.Errorf("local pool counter = %d, want 0 after the local swap", left)
	}
}

// TestSwapPools_UsesLocalWhileInFallback covers the short-circuit.
func TestSwapPools_UsesLocalWhileInFallback(t *testing.T) {
	ctrl := gomock.NewController(t)
	r := newPoolBackend(t, NewMockRedisClient(ctrl))
	r.setFallback(true)
	r.local.AddPools("b1", map[string]int64{"class_a": 6})

	if got := r.SwapPools("b1"); got["class_a"] != 6 {
		t.Errorf("SwapPools = %v, want the local deltas while in fallback", got)
	}
}

// TestTryRecover_ReplaysPoolDeltas is the recovery contract for the pool
// counters: deltas buffered locally during an outage have to reach Redis as
// HINCRBYs, or a budget spent while Redis was down is spent again after it
// comes back.
func TestTryRecover_ReplaysPoolDeltas(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockRedisClient(ctrl)
	pipe := &fakePipeliner{}

	mock.EXPECT().Ping(gomock.Any()).Return(redis.NewStatusResult("PONG", nil))
	mock.EXPECT().Pipeline().Return(pipe)

	r := newPoolBackend(t, mock)
	r.setFallback(true)
	r.local.AddAll("b1", 4, 0, 0)
	r.local.AddPools("b1", map[string]int64{"class_a": 4})

	r.tryRecover()

	if len(pipe.hIncrByFields) != 1 || pipe.hIncrByFields[0] != "class_a" {
		t.Fatalf("replayed pool fields = %v, want [class_a]", pipe.hIncrByFields)
	}
	if pipe.hIncrByVals[0] != 4 {
		t.Errorf("replayed value = %d, want 4", pipe.hIncrByVals[0])
	}
	if got := r.local.LoadPool("b1", "class_a"); got != 0 {
		t.Errorf("local pool counter = %d, want 0 after replay", got)
	}
}
