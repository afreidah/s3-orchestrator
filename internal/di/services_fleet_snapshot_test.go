// -------------------------------------------------------------------------------
// Usage Flush Fleet Snapshot Tests
//
// Author: Alex Freidah
//
// Covers the fleet snapshot across two instances sharing one Redis. The
// instance holding the usage-flush lock computes the fleet gauges and the
// replication status; an instance that loses the lock must serve that result,
// not whatever it last computed itself.
// -------------------------------------------------------------------------------

package di

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/samber/do/v2"
	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// memoryRedis is a counter.RedisClient over one in-memory map. Two backends
// built on the same memoryRedis see each other's writes, the way two
// instances pointed at one Redis do. Only the plain key operations the fleet
// snapshot uses are implemented; the counter pipelines are never reached.
type memoryRedis struct {
	mu   sync.Mutex
	data map[string]string
}

func newMemoryRedis() *memoryRedis { return &memoryRedis{data: map[string]string{}} }

func (m *memoryRedis) Get(_ context.Context, key string) *redis.StringCmd {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[key]
	if !ok {
		return redis.NewStringResult("", redis.Nil)
	}
	return redis.NewStringResult(v, nil)
}

func (m *memoryRedis) Set(_ context.Context, key string, value any, _ time.Duration) *redis.StatusCmd {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch v := value.(type) {
	case []byte:
		m.data[key] = string(v)
	case string:
		m.data[key] = v
	}
	return redis.NewStatusResult("OK", nil)
}

func (m *memoryRedis) Publish(context.Context, string, any) *redis.IntCmd {
	return redis.NewIntResult(0, nil)
}

func (m *memoryRedis) Subscribe(context.Context, ...string) *redis.PubSub { return nil }

func (m *memoryRedis) IncrBy(context.Context, string, int64) *redis.IntCmd {
	return redis.NewIntResult(0, nil)
}

func (m *memoryRedis) GetSet(context.Context, string, any) *redis.StringCmd {
	return redis.NewStringResult("", redis.Nil)
}

func (m *memoryRedis) Del(context.Context, ...string) *redis.IntCmd {
	return redis.NewIntResult(0, nil)
}

func (m *memoryRedis) Expire(context.Context, string, time.Duration) *redis.BoolCmd {
	return redis.NewBoolResult(true, nil)
}

func (m *memoryRedis) HGet(context.Context, string, string) *redis.StringCmd {
	return redis.NewStringResult("", redis.Nil)
}

func (m *memoryRedis) Ping(context.Context) *redis.StatusCmd {
	return redis.NewStatusResult("PONG", nil)
}

func (m *memoryRedis) Pipeline() redis.Pipeliner   { return nil }
func (m *memoryRedis) TxPipeline() redis.Pipeliner { return nil }
func (m *memoryRedis) Close() error                { return nil }

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// newRedisInstance builds one instance's runtime through ProvideBackendRuntime,
// the provider production uses, with Redis configured and backed by shared.
func newRedisInstance(t *testing.T, shared *memoryRedis) *infra.BackendRuntime {
	t.Helper()
	cfg := &config.Config{Redis: &config.RedisConfig{
		Address:          "memory",
		KeyPrefix:        "fleet-test",
		FailureThreshold: 3,
		OpenTimeout:      time.Second,
	}}
	names := []string{"b1"}

	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	storetest.Permissive(store)

	rb := counter.NewRedisCounterBackend(shared, cfg.Redis, names)
	t.Cleanup(func() { _ = rb.Close() })

	inj := do.New()
	do.ProvideValue(inj, cfg)
	do.ProvideValue(inj, &BackendsResult{Backends: map[string]backend.ObjectBackend{}, Order: names})
	do.ProvideValue[metrics.Deps](inj, store)
	do.ProvideValue(inj, rb)

	rt, err := ProvideBackendRuntime(inj)
	if err != nil {
		t.Fatalf("ProvideBackendRuntime: %v", err)
	}
	return rt
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestUsageFlushService_LockLoserServesHolderFleetSnapshot runs one flush tick
// on the instance holding the lock and one on an instance that loses it, both
// sharing a Redis. The loser must then report the holder's replication status.
// Serving its own instead is how the admin API, and every client behind a load
// balancer, saw the replication counts flip between instances.
func TestUsageFlushService_LockLoserServesHolderFleetSnapshot(t *testing.T) {
	t.Parallel()
	shared := newMemoryRedis()
	holder := newRedisInstance(t, shared)
	loser := newRedisInstance(t, shared)

	tick := func(rt *infra.BackendRuntime, locker interface {
		WithAdvisoryLock(context.Context, int64, func(context.Context) error) (bool, error)
	}) {
		NewUsageFlushService(&UsageFlushDeps{
			Flusher: sharedCounterFlusher{},
			Tracker: rt.Usage(),
			Fleet:   rt,
			Locker:  locker,
		}).(*usageFlushService).flushTick(context.Background())
	}
	tick(holder, acquiringLocker{})
	tick(loser, fakeLocker{})

	want := holder.MetricsCollector().ReplicationSnapshot(context.Background())
	got := loser.MetricsCollector().ReplicationSnapshot(context.Background())
	if !want.Ready {
		t.Fatalf("holder computed no replication snapshot: %+v", want)
	}
	if !got.Ready || !got.ComputedAt.Equal(want.ComputedAt) {
		t.Errorf("lock loser serves %+v, want the holder's %+v", got, want)
	}
}
