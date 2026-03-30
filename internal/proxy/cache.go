// -------------------------------------------------------------------------------
// LocationCache - TTL-based Key-to-Backend Mapping Cache
//
// Author: Alex Freidah
//
// In-memory cache mapping object keys to backend names with time-based expiry.
// Used during degraded mode (DB unavailable) to avoid broadcasting reads to all
// backends. Wraps syncutil.TTLCache with jitter on Set to prevent synchronized
// cache expiry storms across entries.
// -------------------------------------------------------------------------------

package proxy

import (
	"math/rand/v2"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// LocationCache is a TTL-based cache mapping object keys to backend names.
// It delegates storage and eviction to a generic TTLCache and applies random
// jitter (+/-20%) on each Set to stagger expiry times.
type LocationCache struct {
	cache *syncutil.TTLCache[string, string]
	ttl   time.Duration
}

// NewLocationCache creates a location cache with the given TTL. If ttl > 0,
// a background goroutine periodically evicts expired entries.
func NewLocationCache(ttl time.Duration) *LocationCache {
	return &LocationCache{
		cache: syncutil.NewTTLCache[string, string](ttl),
		ttl:   ttl,
	}
}

// Get returns the cached backend for a key, or false if not cached or expired.
func (c *LocationCache) Get(key string) (string, bool) {
	return c.cache.Get(key)
}

// Set stores a key-to-backend mapping with the configured TTL. A random
// jitter of +/-20% is applied to prevent synchronized cache expiry storms.
func (c *LocationCache) Set(key, backend string) {
	jitter := 0.8 + rand.Float64()*0.4 //nolint:gosec // G404: cache jitter does not require crypto-strength randomness
	c.cache.SetWithTTL(key, backend, time.Duration(float64(c.ttl)*jitter))
}

// Delete removes a single key from the cache.
func (c *LocationCache) Delete(key string) {
	c.cache.Delete(key)
}

// Clear removes all entries from the cache.
func (c *LocationCache) Clear() {
	c.cache.Clear()
}

// Len returns the number of entries in the cache, including expired entries
// not yet swept by the background eviction goroutine.
func (c *LocationCache) Len() int {
	return c.cache.Len()
}

// Close stops the background eviction goroutine. Safe to call multiple times.
func (c *LocationCache) Close() {
	c.cache.Close()
}
