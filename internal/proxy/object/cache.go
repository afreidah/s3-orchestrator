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

package object

import (
	"math/rand/v2"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// LocationCache is a TTL-based cache mapping object keys to backend names.
// The embedded TTLCache does storage and eviction; Set adds random jitter
// (+/-20%) to stagger expiry times.
type LocationCache struct {
	*syncutil.TTLCache[string, string]
	ttl time.Duration
}

// NewLocationCache creates a location cache with the given TTL. If ttl > 0,
// a background goroutine periodically evicts expired entries.
func NewLocationCache(ttl time.Duration) *LocationCache {
	return &LocationCache{TTLCache: syncutil.NewTTLCache[string, string](ttl), ttl: ttl}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Set stores a key-to-backend mapping with the configured TTL. A random
// jitter of +/-20% is applied to prevent synchronized cache expiry storms.
func (c *LocationCache) Set(key, backend string) {
	jitter := 0.8 + rand.Float64()*0.4 //nolint:gosec // G404: cache jitter does not require crypto-strength randomness
	c.SetWithTTL(key, backend, time.Duration(float64(c.ttl)*jitter))
}
