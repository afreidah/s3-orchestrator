// -------------------------------------------------------------------------------
// TTLCache - Generic In-Memory Cache with Time-Based Expiry
//
// Author: Alex Freidah
//
// Thread-safe generic cache mapping keys to values with per-entry TTL expiry.
// A background goroutine periodically sweeps expired entries. The default TTL
// is applied uniformly without jitter; callers that need jitter should use
// SetWithTTL to supply a custom duration per entry.
// -------------------------------------------------------------------------------

package syncutil

import (
	"sync"
	"time"
)

// cacheEntry holds a cached value and its absolute expiration time.
type cacheEntry[V any] struct {
	value  V
	expiry time.Time
}

// TTLCache is a generic in-memory cache with time-based expiry and background
// eviction. The zero value is not usable; create instances with NewTTLCache.
type TTLCache[K comparable, V any] struct {
	entries   map[K]cacheEntry[V]
	mu        sync.RWMutex
	ttl       time.Duration
	stop      chan struct{}
	closeOnce sync.Once
}

// NewTTLCache creates a cache with the given default TTL. If ttl > 0, a
// background goroutine sweeps expired entries at the TTL interval.
func NewTTLCache[K comparable, V any](ttl time.Duration) *TTLCache[K, V] {
	c := &TTLCache[K, V]{
		entries: make(map[K]cacheEntry[V]),
		ttl:     ttl,
		stop:    make(chan struct{}),
	}
	if ttl > 0 {
		go c.evictLoop(ttl)
	}
	return c
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Get returns the cached value for the key, or the zero value and false if
// the key is missing or expired.
func (c *TTLCache[K, V]) Get(key K) (V, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[key]
	if !ok || time.Now().After(entry.expiry) {
		var zero V
		return zero, false
	}
	return entry.value, true
}

// Set stores a key-value pair with the cache's default TTL.
func (c *TTLCache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = cacheEntry[V]{
		value:  value,
		expiry: time.Now().Add(c.ttl),
	}
}

// SetWithTTL stores a key-value pair with a custom TTL. Use this when
// callers need per-entry TTL variation (e.g. jitter to prevent expiry storms).
func (c *TTLCache[K, V]) SetWithTTL(key K, value V, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = cacheEntry[V]{
		value:  value,
		expiry: time.Now().Add(ttl),
	}
}

// Delete removes a single key from the cache.
func (c *TTLCache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// Clear removes all entries from the cache.
func (c *TTLCache[K, V]) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[K]cacheEntry[V])
}

// Len returns the number of entries in the cache, including expired entries
// that have not yet been swept by the background eviction goroutine.
func (c *TTLCache[K, V]) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// Close stops the background eviction goroutine. Safe to call multiple times.
func (c *TTLCache[K, V]) Close() {
	c.closeOnce.Do(func() {
		close(c.stop)
	})
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// evictLoop runs a ticker that sweeps expired entries at the given interval.
func (c *TTLCache[K, V]) evictLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.evict()
		case <-c.stop:
			return
		}
	}
}

// evict removes all entries whose expiry has passed.
func (c *TTLCache[K, V]) evict() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for key, entry := range c.entries {
		if now.After(entry.expiry) {
			delete(c.entries, key)
		}
	}
}
