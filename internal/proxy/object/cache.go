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
	"errors"
	"io"
	"math/rand/v2"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

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

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

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

// cacheTeeBody copies bytes into a buffer pre-sized to expected as they flow
// to the consumer, and calls onComplete once, only when the read reaches EOF
// after exactly expected bytes. Any other ending (Close before EOF, a
// mid-stream error, a short read, an overshoot) drops the buffer, which is
// what a nil buf records.
type cacheTeeBody struct {
	io.ReadCloser
	buf        []byte
	expected   int64
	onComplete func(data []byte)
	finalized  bool
}

// newCacheTeeBody constructs a teeing wrapper sized to expected bytes, which
// the caller guarantees is positive. onComplete is invoked at most once, and
// the byte slice handed to it is owned by the callback.
func newCacheTeeBody(body io.ReadCloser, expected int64, onComplete func(data []byte)) *cacheTeeBody {
	return &cacheTeeBody{
		ReadCloser: body,
		buf:        make([]byte, 0, expected),
		expected:   expected,
		onComplete: onComplete,
	}
}

// Read forwards to the underlying body and copies returned bytes into the
// buffer. A backend overshooting its Content-Length abandons the copy without
// failing the read, since the consumer's stream is the source of truth and
// cache population is best-effort. Finalizes on clean EOF.
func (t *cacheTeeBody) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	if n > 0 && t.buf != nil {
		if int64(len(t.buf)+n) > t.expected {
			t.buf = nil
		} else {
			t.buf = append(t.buf, p[:n]...)
		}
	}
	if errors.Is(err, io.EOF) && t.buf != nil && !t.finalized && int64(len(t.buf)) == t.expected {
		t.onComplete(t.buf)
		t.finalized = true
	}
	return n, err
}

// Close forwards to the underlying body. If Close arrives before EOF the read
// is considered abandoned and the buffer is dropped.
func (t *cacheTeeBody) Close() error {
	if !t.finalized {
		t.buf = nil
	}
	return t.ReadCloser.Close()
}
