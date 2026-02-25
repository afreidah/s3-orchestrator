// -------------------------------------------------------------------------------
// LocationCache - TTL-based Key-to-Backend Mapping Cache
//
// Author: Alex Freidah
//
// In-memory cache mapping object keys to backend names with time-based expiry.
// Used during degraded mode (DB unavailable) to avoid broadcasting reads to all
// backends. A background goroutine periodically evicts expired entries.
// -------------------------------------------------------------------------------

package storage

import (
	"sync"
	"time"
)

// locationCacheEntry holds a cached key-to-backend mapping with TTL.
type locationCacheEntry struct {
	backendName string
	expiry      time.Time
}

// LocationCache is a TTL-based cache mapping object keys to backend names.
type LocationCache struct {
	entries  map[string]locationCacheEntry
	mu       sync.RWMutex
	ttl      time.Duration
	stop     chan struct{}
	closeOnce sync.Once
}

// NewLocationCache creates a location cache with the given TTL. If ttl > 0,
// a background goroutine periodically evicts expired entries.
func NewLocationCache(ttl time.Duration) *LocationCache {
	c := &LocationCache{
		entries: make(map[string]locationCacheEntry),
		ttl:     ttl,
		stop:    make(chan struct{}),
	}

	if ttl > 0 {
		go func() {
			ticker := time.NewTicker(ttl)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					c.evict()
				case <-c.stop:
					return
				}
			}
		}()
	}

	return c
}

// Get returns the cached backend for a key, or false if not cached or expired.
func (c *LocationCache) Get(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[key]
	if !ok || time.Now().After(entry.expiry) {
		return "", false
	}
	return entry.backendName, true
}

// Set stores a key-to-backend mapping with the configured TTL.
func (c *LocationCache) Set(key, backend string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = locationCacheEntry{
		backendName: backend,
		expiry:      time.Now().Add(c.ttl),
	}
}

// Delete removes a single key from the cache.
func (c *LocationCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// Clear removes all entries from the cache.
func (c *LocationCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]locationCacheEntry)
}

// Close stops the background eviction goroutine. Safe to call multiple times.
func (c *LocationCache) Close() {
	c.closeOnce.Do(func() {
		close(c.stop)
	})
}

// evict removes expired entries from the cache.
func (c *LocationCache) evict() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for key, entry := range c.entries {
		if now.After(entry.expiry) {
			delete(c.entries, key)
		}
	}
}
