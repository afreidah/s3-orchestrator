// -------------------------------------------------------------------------------
// LocationCache Tests
//
// Author: Alex Freidah
//
// Tests for TTL-based key-to-backend mapping cache: basic CRUD, expiry,
// concurrent access, and eviction cleanup.
// -------------------------------------------------------------------------------

package proxy

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestLocationCache_SetGet(t *testing.T) {
	c := NewLocationCache(5 * time.Second)
	defer c.Close()

	c.Set("key1", "backend-a")

	got, ok := c.Get("key1")
	if !ok || got != "backend-a" {
		t.Errorf("Get(key1) = (%q, %v), want (backend-a, true)", got, ok)
	}
}

func TestLocationCache_GetMiss(t *testing.T) {
	c := NewLocationCache(5 * time.Second)
	defer c.Close()

	_, ok := c.Get("nonexistent")
	if ok {
		t.Error("Get on missing key should return false")
	}
}

func TestLocationCache_Delete(t *testing.T) {
	c := NewLocationCache(5 * time.Second)
	defer c.Close()

	c.Set("key1", "backend-a")
	c.Delete("key1")

	_, ok := c.Get("key1")
	if ok {
		t.Error("Get after Delete should return false")
	}
}

func TestLocationCache_Clear(t *testing.T) {
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


func TestLocationCache_Eviction(t *testing.T) {
	c := NewLocationCache(50 * time.Millisecond)
	defer c.Close()

	c.Set("key1", "backend-a")

	// Wait for eviction goroutine to run (ticks every TTL)
	time.Sleep(150 * time.Millisecond)

	c.mu.RLock()
	count := len(c.entries)
	c.mu.RUnlock()

	if count != 0 {
		t.Errorf("entries after eviction = %d, want 0", count)
	}
}

func TestLocationCache_ZeroTTL_NoEvictionGoroutine(t *testing.T) {
	c := NewLocationCache(0)
	defer c.Close()

	// With TTL=0, Set still works but entries never expire via Get
	c.Set("key1", "backend-a")
	if _, ok := c.Get("key1"); ok {
		// TTL=0 means expiry is in the past, so Get should return false
		t.Error("expected expired with zero TTL")
	}
}

func TestLocationCache_ConcurrentAccess(t *testing.T) {
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

func TestLocationCache_CloseIdempotent(t *testing.T) {
	c := NewLocationCache(5 * time.Second)
	c.Close()
	c.Close() // should not panic
}
