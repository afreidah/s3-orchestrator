// -------------------------------------------------------------------------------
// LocationCache Tests
//
// Author: Alex Freidah
//
// Tests for TTL-based key-to-backend mapping cache: basic CRUD, expiry,
// concurrent access, and eviction cleanup.
// -------------------------------------------------------------------------------

package object

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestLocationCache_SetGet verifies the location cache set get contract.
// Asserts that Get(key1) = (, ), want (backend-a, true).
func TestLocationCache_SetGet(t *testing.T) {
	t.Parallel()
	c := NewLocationCache(5 * time.Second)
	defer c.Close()

	c.Set("key1", "backend-a")

	got, ok := c.Get("key1")
	if !ok || got != "backend-a" {
		t.Errorf("Get(key1) = (%q, %v), want (backend-a, true)", got, ok)
	}
}

// TestLocationCache_GetMiss verifies the location cache get miss path by exercising c.Close, c.Get.
func TestLocationCache_GetMiss(t *testing.T) {
	t.Parallel()
	c := NewLocationCache(5 * time.Second)
	defer c.Close()

	_, ok := c.Get("nonexistent")
	if ok {
		t.Error("Get on missing key should return false")
	}
}

// TestLocationCache_Delete verifies the location cache delete path by exercising c.Close, c.Set, c.Delete.
func TestLocationCache_Delete(t *testing.T) {
	t.Parallel()
	c := NewLocationCache(5 * time.Second)
	defer c.Close()

	c.Set("key1", "backend-a")
	c.Delete("key1")

	_, ok := c.Get("key1")
	if ok {
		t.Error("Get after Delete should return false")
	}
}

// TestLocationCache_Clear verifies the location cache clear path by exercising c.Close, c.Set, c.Clear.
func TestLocationCache_Clear(t *testing.T) {
	t.Parallel()
	c := NewLocationCache(5 * time.Second)
	defer c.Close()

	c.Set("key1", "backend-a")
	c.Set("key2", "backend-b")
	c.Clear()

	if _, ok := c.Get("key1"); ok {
		t.Error("key1 should be gone after Clear")
	}
	if _, ok := c.Get("key2"); ok {
		t.Error("key2 should be gone after Clear")
	}
}

// TestLocationCache_Eviction verifies the location cache eviction contract.
// Asserts that entries after eviction = , want 0.
func TestLocationCache_Eviction(t *testing.T) {
	t.Parallel()
	c := NewLocationCache(50 * time.Millisecond)
	defer c.Close()

	c.Set("key1", "backend-a")

	// Wait for eviction goroutine to run (ticks every TTL)
	time.Sleep(150 * time.Millisecond)

	if count := c.Len(); count != 0 {
		t.Errorf("entries after eviction = %d, want 0", count)
	}
}

// TestLocationCache_ZeroTTL_NoEvictionGoroutine verifies the location cache zero ttl no eviction goroutine path by exercising c.Close, c.Set, c.Get.
func TestLocationCache_ZeroTTL_NoEvictionGoroutine(t *testing.T) {
	t.Parallel()
	c := NewLocationCache(0)
	defer c.Close()

	// A zero TTL disables the cache outright rather than granting an entry a
	// lifetime so short that whether it is served depends on the clock.
	c.Set("key1", "backend-a")
	if _, ok := c.Get("key1"); ok {
		t.Error("Get served an entry from a zero-TTL cache")
	}
}

// TestLocationCache_ConcurrentAccess verifies the location cache concurrent access path by exercising c.Close, wg.Go, fmt.Sprintf.
func TestLocationCache_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	c := NewLocationCache(5 * time.Second)
	defer c.Close()

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			key := fmt.Sprintf("key-%d", i%10)
			c.Set(key, "backend")
			c.Get(key)
			c.Delete(key)
		})
	}
	wg.Wait()
}

// TestLocationCache_CloseIdempotent verifies the location cache close idempotent path by exercising c.Close.
func TestLocationCache_CloseIdempotent(t *testing.T) {
	t.Parallel()
	c := NewLocationCache(5 * time.Second)
	c.Close()
	c.Close() // should not panic
}
