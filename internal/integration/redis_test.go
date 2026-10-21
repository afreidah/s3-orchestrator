// -------------------------------------------------------------------------------
// Integration Tests - Redis Shared Counters
//
// Author: Alex Freidah
//
// Integration tests for the RedisCounterBackend against a real Redis instance.
// Covers charging and reading the shared hashes, visibility across instances,
// and staging a hash for a flush until it is settled.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// redisAddr returns the Redis address from the environment or the default.
func redisAddr() string {
	return envOrDefault("REDIS_ADDR", "localhost:16379")
}

// newRedisClient creates a go-redis client pointed at the integration Redis.
func newRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: redisAddr()})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Skipf("Redis unavailable at %s: %v", redisAddr(), err)
	}
	return client
}

// newTestRedisBackend creates a RedisCounterBackend with a unique key prefix
// to isolate test runs.
func newTestRedisBackend(t *testing.T, backends []string) (*counter.RedisCounterBackend, *redis.Client) {
	t.Helper()
	client := newRedisClient(t)

	prefix := fmt.Sprintf("test_%s_%d", t.Name(), time.Now().UnixNano())
	cfg := &config.RedisConfig{
		Address:          redisAddr(),
		KeyPrefix:        prefix,
		FailureThreshold: 3,
		OpenTimeout:      500 * time.Millisecond,
	}

	rb := counter.NewRedisCounterBackend(client, cfg, backends)
	t.Cleanup(func() { rb.Close() })

	return rb, client
}

// loadUsage returns a backend's unflushed counters as a fresh value, for the
// assertions that read them.
func loadUsage(b counter.Backend, name string) core.Usage {
	var u core.Usage
	b.Load(name, &u)
	return u
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestRedis_ChargeAndLoad verifies charges land in each backend's hash,
// totals and pools alike, and read back through Load.
func TestRedis_ChargeAndLoad(t *testing.T) {
	rb, _ := newTestRedisBackend(t, []string{"b1", "b2"})

	rb.Charge("b1", counter.Charge{APIRequests: 5, EgressBytes: 100, Pools: []core.RequestPool{{Name: "reads"}}, PoolN: 5})
	rb.Charge("b1", counter.Charge{APIRequests: 3, EgressBytes: 50, Pools: []core.RequestPool{{Name: "reads"}}, PoolN: 3})
	rb.Charge("b2", counter.Charge{APIRequests: 1, IngressBytes: 300})

	b1 := loadUsage(rb, "b1")
	if b1.APIRequests != 8 || b1.EgressBytes != 150 || b1.Pools["reads"] != 8 {
		t.Errorf("b1 = %+v, want 8 requests, 150 egress, 8 on reads", b1)
	}
	if got := loadUsage(rb, "b2").IngressBytes; got != 300 {
		t.Errorf("b2 ingress_bytes = %d, want 300", got)
	}
}

// TestRedis_TakeSharedStagesAndSettles verifies a flush takes the live hash
// under a flush ID, finds it again until it is settled as applied, and that a
// charge made after the take starts a fresh hash.
func TestRedis_TakeSharedStagesAndSettles(t *testing.T) {
	ctx := context.Background()
	rb, _ := newTestRedisBackend(t, []string{"b1"})
	rb.Charge("b1", counter.Charge{APIRequests: 4})

	batches := rb.TakeShared(ctx)
	if len(batches) != 1 || batches[0].ID == "" || batches[0].Backend != "b1" || batches[0].Workloads.Total().APIRequests != 4 {
		t.Fatalf("batches = %+v, want one staged b1 batch of 4 requests", batches)
	}
	if got := loadUsage(rb, "b1").APIRequests; got != 0 {
		t.Errorf("live requests after the take = %d, want 0", got)
	}
	rb.Charge("b1", counter.Charge{APIRequests: 2})

	rb.Settle(ctx, &batches[0], false)
	again := rb.TakeShared(ctx)
	if len(again) != 2 {
		t.Fatalf("second take = %+v, want the unsettled batch and the new charge", again)
	}
	for i := range again {
		rb.Settle(ctx, &again[i], true)
	}
	if left := rb.TakeShared(ctx); len(left) != 0 {
		t.Errorf("batches after settling = %+v, want none", left)
	}
}

// TestRedis_SharedVisibility verifies the redis shared visibility contract.
// Asserts that rb1:.
func TestRedis_SharedVisibility(t *testing.T) {
	// Two RedisCounterBackend instances sharing the same prefix simulate
	// two orchestrator instances seeing the same counters.
	client := newRedisClient(t)
	prefix := fmt.Sprintf("test_shared_%d", time.Now().UnixNano())
	backends := []string{"b1"}
	cfg := &config.RedisConfig{
		Address:          redisAddr(),
		KeyPrefix:        prefix,
		FailureThreshold: 3,
		OpenTimeout:      500 * time.Millisecond,
	}

	rb1 := counter.NewRedisCounterBackend(client, cfg, backends)
	defer rb1.Close()

	// Second instance needs its own client to simulate a separate process,
	// but shares the same prefix.
	client2 := newRedisClient(t)
	rb2 := counter.NewRedisCounterBackend(client2, cfg, backends)
	defer rb2.Close()

	rb1.Charge("b1", counter.Charge{APIRequests: 10})
	rb2.Charge("b1", counter.Charge{APIRequests: 20})

	// Both should see the combined value
	if got := loadUsage(rb1, "b1").APIRequests; got != 30 {
		t.Errorf("rb1.Load = %d, want 30", got)
	}
	if got := loadUsage(rb2, "b1").APIRequests; got != 30 {
		t.Errorf("rb2.Load = %d, want 30", got)
	}
}

// TestRedis_Backends verifies the redis backends contract.
// Asserts that Backends() returned , want 3.
func TestRedis_Backends(t *testing.T) {
	backends := []string{"alpha", "beta", "gamma"}
	rb, _ := newTestRedisBackend(t, backends)

	got := rb.Backends()
	if len(got) != 3 {
		t.Fatalf("Backends() returned %d, want 3", len(got))
	}
}

// TestRedis_IsHealthy verifies the redis is healthy path by exercising rb.IsHealthy.
func TestRedis_IsHealthy(t *testing.T) {
	backends := []string{"b1"}
	rb, _ := newTestRedisBackend(t, backends)

	if !rb.IsHealthy() {
		t.Error("expected IsHealthy = true after successful init")
	}
}
