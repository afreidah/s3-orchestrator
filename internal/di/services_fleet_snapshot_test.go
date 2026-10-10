// -------------------------------------------------------------------------------
// Usage Flush Fleet Snapshot Tests
//
// Author: Alex Freidah
//
// Covers the fleet snapshot across two instances sharing one Redis. The fleet
// snapshot service computes the fleet gauges and the replication status on
// one instance; every other instance must serve that result, not whatever it
// last computed itself, and must not recompute a snapshot that is still fresh.
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
	"github.com/afreidah/s3-orchestrator/internal/lifecycle/tickrunner"
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
	do.Provide(inj, ProvideUsageTracker)

	rt, err := ProvideBackendRuntime(inj)
	if err != nil {
		t.Fatalf("ProvideBackendRuntime: %v", err)
	}
	return rt
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// fleetTick runs one fleet snapshot service tick on rt under locker.
func fleetTick(rt *infra.BackendRuntime, locker tickrunner.AdvisoryLocker) {
	NewFleetSnapshotService(rt, locker, time.Minute).(*tickrunner.Service).Tick(context.Background())
}

// TestUsageFlushService_ServesPublishedFleetSnapshot computes the snapshot on
// one instance and runs a flush tick on another that loses the flush lock,
// both sharing a Redis. The second must then report the first's replication
// status. Serving its own instead is how the admin API, and every client
// behind a load balancer, saw the replication counts flip between instances.
func TestUsageFlushService_ServesPublishedFleetSnapshot(t *testing.T) {
	t.Parallel()
	shared := newMemoryRedis()
	computer := newRedisInstance(t, shared)
	other := newRedisInstance(t, shared)

	fleetTick(computer, acquiringLocker{})
	NewUsageFlushService(&UsageFlushDeps{
		Flusher: sharedCounterFlusher{},
		Tracker: other.Usage(),
		Fleet:   other,
		Drains:  noDrains{},
		Locker:  fakeLocker{},
	}).(*usageFlushService).flushTick(context.Background())

	want := computer.MetricsCollector().ReplicationSnapshot(context.Background())
	got := other.MetricsCollector().ReplicationSnapshot(context.Background())
	if !want.Ready {
		t.Fatalf("fleet tick computed no replication snapshot: %+v", want)
	}
	if !got.Ready || !got.ComputedAt.Equal(want.ComputedAt) {
		t.Errorf("flush tick serves %+v, want the published %+v", got, want)
	}
}

// TestFleetSnapshotService_FreshSnapshotIsNotRecomputed runs the fleet tick on
// two instances in turn. The second finds a snapshot younger than half the
// interval and applies it, so the ledger is scanned once per interval however
// many instances take the lock.
func TestFleetSnapshotService_FreshSnapshotIsNotRecomputed(t *testing.T) {
	t.Parallel()
	shared := newMemoryRedis()
	first := newRedisInstance(t, shared)
	second := newRedisInstance(t, shared)

	fleetTick(first, acquiringLocker{})
	published := first.MetricsCollector().ReplicationSnapshot(context.Background())
	fleetTick(second, acquiringLocker{})

	got := second.MetricsCollector().ReplicationSnapshot(context.Background())
	if !published.Ready || !got.ComputedAt.Equal(published.ComputedAt) {
		t.Errorf("second tick published %+v, want the fresh snapshot %+v left in place", got, published)
	}
}
