// -------------------------------------------------------------------------------
// Integration Tests - Redis Shared Counters
//
// Author: Alex Freidah
//
// Integration tests for the RedisCounterBackend against a real Redis instance.
// Covers shared counter visibility, circuit breaker fallback to local counters,
// and recovery with local delta sync.
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
	"github.com/afreidah/s3-orchestrator/internal/storage"
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
func newTestRedisBackend(t *testing.T, backends []string) (*storage.RedisCounterBackend, *redis.Client) {
	t.Helper()
	client := newRedisClient(t)

	prefix := fmt.Sprintf("test_%s_%d", t.Name(), time.Now().UnixNano())
	cfg := &config.RedisConfig{
		Address:          redisAddr(),
		KeyPrefix:        prefix,
		FailureThreshold: 3,
		OpenTimeout:      500 * time.Millisecond,
	}

	rb, err := storage.NewRedisCounterBackend(client, cfg, backends)
	if err != nil {
		t.Fatalf("NewRedisCounterBackend: %v", err)
	}
	t.Cleanup(func() { rb.Close() })

	return rb, client
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

func TestRedis_AddAndLoad(t *testing.T) {
	backends := []string{"b1", "b2"}
	rb, _ := newTestRedisBackend(t, backends)

	rb.Add("b1", storage.FieldAPIRequests, 10)
	rb.Add("b1", storage.FieldEgressBytes, 200)
	rb.Add("b2", storage.FieldIngressBytes, 300)

	if got := rb.Load("b1", storage.FieldAPIRequests); got != 10 {
		t.Errorf("b1 api_requests = %d, want 10", got)
	}
	if got := rb.Load("b1", storage.FieldEgressBytes); got != 200 {
		t.Errorf("b1 egress_bytes = %d, want 200", got)
	}
	if got := rb.Load("b2", storage.FieldIngressBytes); got != 300 {
		t.Errorf("b2 ingress_bytes = %d, want 300", got)
	}
}

func TestRedis_AddAllAndLoadAll(t *testing.T) {
	backends := []string{"b1"}
	rb, _ := newTestRedisBackend(t, backends)

	rb.AddAll("b1", 5, 100, 200)
	rb.AddAll("b1", 3, 50, 0)

	result := rb.LoadAll("b1")
	if result.APIRequests != 8 {
		t.Errorf("api_requests = %d, want 8", result.APIRequests)
	}
	if result.EgressBytes != 150 {
		t.Errorf("egress_bytes = %d, want 150", result.EgressBytes)
	}
	if result.IngressBytes != 200 {
		t.Errorf("ingress_bytes = %d, want 200", result.IngressBytes)
	}
}

func TestRedis_Swap(t *testing.T) {
	backends := []string{"b1"}
	rb, _ := newTestRedisBackend(t, backends)

	rb.Add("b1", storage.FieldAPIRequests, 42)

	got := rb.Swap("b1", storage.FieldAPIRequests)
	if got != 42 {
		t.Errorf("Swap returned %d, want 42", got)
	}

	// After swap, counter should be 0
	if v := rb.Load("b1", storage.FieldAPIRequests); v != 0 {
		t.Errorf("after Swap, Load = %d, want 0", v)
	}
}

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

	rb1, err := storage.NewRedisCounterBackend(client, cfg, backends)
	if err != nil {
		t.Fatalf("rb1: %v", err)
	}
	defer rb1.Close()

	// Second instance needs its own client to simulate a separate process,
	// but shares the same prefix.
	client2 := newRedisClient(t)
	rb2, err := storage.NewRedisCounterBackend(client2, cfg, backends)
	if err != nil {
		t.Fatalf("rb2: %v", err)
	}
	defer rb2.Close()

	// Instance 1 writes
	rb1.Add("b1", storage.FieldAPIRequests, 10)
	// Instance 2 writes
	rb2.Add("b1", storage.FieldAPIRequests, 20)

	// Both should see the combined value
	if got := rb1.Load("b1", storage.FieldAPIRequests); got != 30 {
		t.Errorf("rb1.Load = %d, want 30", got)
	}
	if got := rb2.Load("b1", storage.FieldAPIRequests); got != 30 {
		t.Errorf("rb2.Load = %d, want 30", got)
	}
}

func TestRedis_Backends(t *testing.T) {
	backends := []string{"alpha", "beta", "gamma"}
	rb, _ := newTestRedisBackend(t, backends)

	got := rb.Backends()
	if len(got) != 3 {
		t.Fatalf("Backends() returned %d, want 3", len(got))
	}
}

func TestRedis_IsHealthy(t *testing.T) {
	backends := []string{"b1"}
	rb, _ := newTestRedisBackend(t, backends)

	if !rb.IsHealthy() {
		t.Error("expected IsHealthy = true after successful init")
	}
	if rb.InFallbackMode() {
		t.Error("expected InFallbackMode = false after successful init")
	}
}
