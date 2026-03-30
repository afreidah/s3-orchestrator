// -------------------------------------------------------------------------------
// TTLCache Tests
//
// Author: Alex Freidah
//
// Unit tests and benchmarks for the generic TTLCache: basic CRUD, TTL expiry,
// background eviction, SetWithTTL, zero-TTL behavior, and concurrent access.
// The eviction benchmark is migrated from LocationCache benchmarks.
// -------------------------------------------------------------------------------

package syncutil

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestTTLCache_SetGet(t *testing.T) {
	t.Parallel()
	c := NewTTLCache[string, string](5 * time.Second)
	defer c.Close()

	c.Set("key1", "value1")

	got, ok := c.Get("key1")
	if !ok || got != "value1" {
		t.Errorf("Get(key1) = (%q, %v), want (value1, true)", got, ok)
	}
}

func TestTTLCache_GetMiss(t *testing.T) {
	t.Parallel()
	c := NewTTLCache[string, int](5 * time.Second)
	defer c.Close()

	got, ok := c.Get("nonexistent")
	if ok || got != 0 {
		t.Errorf("Get(nonexistent) = (%v, %v), want (0, false)", got, ok)
	}
}

func TestTTLCache_Delete(t *testing.T) {
	t.Parallel()
	c := NewTTLCache[string, string](5 * time.Second)
	defer c.Close()

	c.Set("key1", "value1")
	c.Delete("key1")

	_, ok := c.Get("key1")
	if ok {
		t.Error("Get after Delete should return false")
	}
}

func TestTTLCache_Clear(t *testing.T) {
	t.Parallel()
	c := NewTTLCache[string, string](5 * time.Second)
	defer c.Close()

	c.Set("key1", "value1")
	c.Set("key2", "value2")
	c.Clear()

	if _, ok := c.Get("key1"); ok {
		t.Error("key1 should be gone after Clear")
	}
	if _, ok := c.Get("key2"); ok {
		t.Error("key2 should be gone after Clear")
	}
}

func TestTTLCache_Expiry(t *testing.T) {
	t.Parallel()
	c := NewTTLCache[string, string](50 * time.Millisecond)
	defer c.Close()

	c.Set("key1", "value1")

	// Verify entry is live before expiry
	if _, ok := c.Get("key1"); !ok {
		t.Fatal("entry should be live immediately after Set")
	}

	time.Sleep(80 * time.Millisecond)

	if _, ok := c.Get("key1"); ok {
		t.Error("entry should be expired after TTL")
	}
}

func TestTTLCache_Eviction(t *testing.T) {
	t.Parallel()
	c := NewTTLCache[string, string](50 * time.Millisecond)
	defer c.Close()

	c.Set("key1", "value1")

	// Wait for the eviction goroutine to sweep (ticks every TTL)
	time.Sleep(150 * time.Millisecond)

	if count := c.Len(); count != 0 {
		t.Errorf("entries after eviction = %d, want 0", count)
	}
}

func TestTTLCache_SetWithTTL(t *testing.T) {
	t.Parallel()
	c := NewTTLCache[string, string](5 * time.Second)
	defer c.Close()

	// Set with a very short custom TTL
	c.SetWithTTL("key1", "value1", 50*time.Millisecond)

	if _, ok := c.Get("key1"); !ok {
		t.Fatal("entry should be live immediately after SetWithTTL")
	}

	time.Sleep(80 * time.Millisecond)

	if _, ok := c.Get("key1"); ok {
		t.Error("entry should be expired after custom TTL")
	}
}

func TestTTLCache_ZeroTTL(t *testing.T) {
	t.Parallel()
	c := NewTTLCache[string, string](0)
	defer c.Close()

	c.Set("key1", "value1")

	// TTL=0 means expiry is in the past, so Get should return false
	if _, ok := c.Get("key1"); ok {
		t.Error("expected expired with zero TTL")
	}
}

func TestTTLCache_Len(t *testing.T) {
	t.Parallel()
	c := NewTTLCache[string, string](5 * time.Second)
	defer c.Close()

	c.Set("a", "1")
	c.Set("b", "2")
	c.Set("c", "3")
	if got := c.Len(); got != 3 {
		t.Errorf("Len = %d, want 3", got)
	}

	c.Delete("b")
	if got := c.Len(); got != 2 {
		t.Errorf("Len after delete = %d, want 2", got)
	}
}

func TestTTLCache_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	c := NewTTLCache[string, string](5 * time.Second)
	defer c.Close()

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			key := fmt.Sprintf("key-%d", i%10)
			c.Set(key, "value")
			c.Get(key)
			c.Delete(key)
		})
	}
	wg.Wait()
}

func TestTTLCache_CloseIdempotent(t *testing.T) {
	t.Parallel()
	c := NewTTLCache[string, string](5 * time.Second)
	c.Close()
	c.Close() // should not panic
}

// -------------------------------------------------------------------------
// BENCHMARKS
// -------------------------------------------------------------------------

func BenchmarkTTLCache_Eviction(b *testing.B) {
	sizes := []int{100, 1000, 10000}

	for _, n := range sizes {
		b.Run(fmt.Sprintf("%d_entries", n), func(b *testing.B) {
			c := &TTLCache[string, string]{
				entries: make(map[string]cacheEntry[string], n),
				ttl:     time.Minute,
				stop:    make(chan struct{}),
			}
			defer close(c.stop)

			// All entries expired
			expired := time.Now().Add(-time.Second)
			for i := range n {
				c.entries[fmt.Sprintf("key-%d", i)] = cacheEntry[string]{
					value:  "b1",
					expiry: expired,
				}
			}

			b.ResetTimer()
			for b.Loop() {
				c.evict()
				// Refill for next iteration
				b.StopTimer()
				for i := range n {
					c.entries[fmt.Sprintf("key-%d", i)] = cacheEntry[string]{
						value:  "b1",
						expiry: expired,
					}
				}
				b.StartTimer()
			}
		})
	}
}
