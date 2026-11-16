// -------------------------------------------------------------------------------
// Capacity Warning Tests
//
// Author: Alex Freidah
//
// Covers capacity warnings across two instances sharing one Redis. Every
// instance refreshes the gauges on its own flush tick, so without the shared
// marker each one would warn for a full backend on every tick; with it the
// fleet warns once per window. The tests capture the event emitter, so none
// run in parallel.
// -------------------------------------------------------------------------------

package di

import (
	"context"
	"errors"
	"path"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/samber/do/v2"
	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/instanceid"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// memoryRedis is a counter.RedisClient over one in-memory map. Two backends
// built on the same memoryRedis see each other's writes, the way two
// instances pointed at one Redis do. Only plain string keys are stored: a
// counter hash reads as empty, and a counter charge fails so the backend
// keeps it locally.
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

func (m *memoryRedis) HGetAll(context.Context, string) *redis.MapStringStringCmd {
	return redis.NewMapStringStringResult(map[string]string{}, nil)
}

func (m *memoryRedis) Rename(_ context.Context, key, newkey string) *redis.StatusCmd {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[key]
	if !ok {
		return redis.NewStatusResult("", errors.New("ERR no such key"))
	}
	delete(m.data, key)
	m.data[newkey] = v
	return redis.NewStatusResult("OK", nil)
}

func (m *memoryRedis) Scan(_ context.Context, _ uint64, match string, _ int64) *redis.ScanCmd {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []string
	for k := range m.data {
		if ok, _ := path.Match(match, k); ok {
			keys = append(keys, k)
		}
	}
	cmd := redis.NewScanCmd(context.Background(), nil)
	cmd.SetVal(keys, 0)
	return cmd
}

func (m *memoryRedis) Del(_ context.Context, keys ...string) *redis.IntCmd {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, k := range keys {
		if _, ok := m.data[k]; ok {
			delete(m.data, k)
			n++
		}
	}
	return redis.NewIntResult(n, nil)
}

func (m *memoryRedis) Ping(context.Context) *redis.StatusCmd {
	return redis.NewStatusResult("PONG", nil)
}

func (m *memoryRedis) TxPipeline() redis.Pipeliner { return failingPipeline{} }
func (m *memoryRedis) Close() error                { return nil }

// failingPipeline is a transaction whose Exec always fails. Only the calls a
// counter charge makes are implemented.
type failingPipeline struct{ redis.Pipeliner }

func (failingPipeline) HIncrBy(context.Context, string, string, int64) *redis.IntCmd {
	return redis.NewIntResult(0, nil)
}

func (failingPipeline) Expire(context.Context, string, time.Duration) *redis.BoolCmd {
	return redis.NewBoolResult(true, nil)
}

func (failingPipeline) Exec(context.Context) ([]redis.Cmder, error) {
	return nil, errors.New("memoryRedis does not store counter hashes")
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// newCollector builds one instance's collector through ProvideMetricsCollector,
// the provider production uses. With shared set, Redis is configured and
// backed by it; without, the instance runs alone.
func newCollector(t *testing.T, shared *memoryRedis) *metrics.Collector {
	t.Helper()
	cfg := &config.Config{}
	names := []string{"b1"}
	inj := do.New()
	if shared != nil {
		cfg.Redis = &config.RedisConfig{Address: "memory", KeyPrefix: "fleet-test", FailureThreshold: 3, OpenTimeout: time.Second}
		rb := counter.NewRedisCounterBackend(shared, cfg.Redis, names)
		t.Cleanup(func() { _ = rb.Close() })
		do.ProvideValue(inj, rb)
	}

	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	storetest.Permissive(store)

	do.ProvideValue(inj, cfg)
	do.ProvideValue(inj, config.NewLive(cfg))
	do.ProvideValue(inj, &BackendsResult{Backends: map[string]backend.ObjectBackend{}, Order: names})
	do.ProvideValue[metrics.Deps](inj, store)
	do.ProvideValue(inj, instanceid.ID(t.Name()))
	do.Provide(inj, ProvideSharedState)
	do.Provide(inj, ProvideUsageTracker)
	do.Provide(inj, ProvideBackendRuntime)

	mc, err := ProvideMetricsCollector(inj)
	if err != nil {
		t.Fatalf("ProvideMetricsCollector: %v", err)
	}
	return mc
}

// capacityWarnings counts the capacity warnings published while the test runs.
func capacityWarnings(t *testing.T) *int {
	t.Helper()
	var n int
	event.SetEmitter(func(ev event.Event) {
		if ev.Type == event.BackendCapacityWarning {
			n++
		}
	})
	t.Cleanup(func() { event.SetEmitter(nil) })
	return &n
}

// fullBackend is a quota read with b1 past the capacity threshold.
var fullBackend = map[string]core.QuotaStat{"b1": {BackendName: "b1", BytesUsed: 900, BytesLimit: 1000}}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestCapacityWarning_OncePerFleet refreshes the gauges on two instances
// sharing a Redis, twice each. The first refresh warns and sets the marker,
// and every other one finds it standing.
func TestCapacityWarning_OncePerFleet(t *testing.T) {
	warnings := capacityWarnings(t)
	shared := newMemoryRedis()
	instances := []*metrics.Collector{newCollector(t, shared), newCollector(t, shared)}

	for range 2 {
		for _, mc := range instances {
			if err := mc.RefreshGauges(context.Background(), fullBackend); err != nil {
				t.Fatalf("RefreshGauges: %v", err)
			}
		}
	}
	if *warnings != 1 {
		t.Errorf("capacity warnings = %d, want 1 across the fleet", *warnings)
	}
}

// TestCapacityWarning_SingleInstanceWarnsEachRefresh verifies an instance
// without shared state keeps no marker, and leaves spacing the events to the
// notifier's dampener.
func TestCapacityWarning_SingleInstanceWarnsEachRefresh(t *testing.T) {
	warnings := capacityWarnings(t)
	mc := newCollector(t, nil)

	for range 2 {
		if err := mc.RefreshGauges(context.Background(), fullBackend); err != nil {
			t.Fatalf("RefreshGauges: %v", err)
		}
	}
	if *warnings != 2 {
		t.Errorf("capacity warnings = %d, want one per refresh", *warnings)
	}
}
