// -------------------------------------------------------------------------------
// Memory Cache - Size-Aware LRU with TTL
//
// Author: Alex Freidah
//
// In-memory object cache using a size-aware LRU eviction policy with TTL-based
// expiry. Entries are evicted when the cache exceeds max_size (least recently
// used first) or when their TTL expires. Objects larger than max_object_size
// are rejected on admission. Thread-safe for concurrent use.
// -------------------------------------------------------------------------------

package cache

import (
	"container/list"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/telemetry"
)

// -------------------------------------------------------------------------
// CONFIGURATION
// -------------------------------------------------------------------------

// MemoryConfig holds tuning parameters for the in-memory cache.
type MemoryConfig struct {
	MaxSize       int64         // maximum total cache size in bytes
	MaxObjectSize int64         // maximum size of a single cached object in bytes
	TTL           time.Duration // time before an entry expires
}

// -------------------------------------------------------------------------
// IMPLEMENTATION
// -------------------------------------------------------------------------

// memoryEntry is a single cached item stored in the LRU list.
type memoryEntry struct {
	key       string
	entry     *Entry
	size      int64
	expiresAt time.Time
}

// MemoryCache is a size-aware LRU cache backed by an in-memory map and
// doubly-linked list. Evicts least-recently-used entries when the total
// size exceeds MaxSize. Entries expire after TTL.
type MemoryCache struct {
	mu            sync.Mutex
	items         map[string]*list.Element
	order         *list.List // front = most recently used
	sizeBytes     int64
	maxSize       int64
	maxObjectSize int64
	ttl           time.Duration
}

// NewMemoryCache creates a new in-memory LRU cache with the given configuration.
func NewMemoryCache(cfg MemoryConfig) (*MemoryCache, error) {
	if cfg.MaxSize <= 0 {
		return nil, fmt.Errorf("cache max_size must be positive, got %d", cfg.MaxSize)
	}
	if cfg.MaxObjectSize <= 0 {
		return nil, fmt.Errorf("cache max_object_size must be positive, got %d", cfg.MaxObjectSize)
	}
	if cfg.MaxObjectSize > cfg.MaxSize {
		return nil, fmt.Errorf("cache max_object_size (%d) cannot exceed max_size (%d)", cfg.MaxObjectSize, cfg.MaxSize)
	}
	if cfg.TTL <= 0 {
		return nil, fmt.Errorf("cache ttl must be positive, got %v", cfg.TTL)
	}
	return &MemoryCache{
		items:         make(map[string]*list.Element),
		order:         list.New(),
		maxSize:       cfg.MaxSize,
		maxObjectSize: cfg.MaxObjectSize,
		ttl:           cfg.TTL,
	}, nil
}

// Get returns the cached entry if present and not expired. On hit, the entry
// is promoted to the front of the LRU list.
func (c *MemoryCache) Get(key string) (*Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	elem, ok := c.items[key]
	if !ok {
		telemetry.CacheMissesTotal.Inc()
		return nil, false
	}

	me := elem.Value.(*memoryEntry)
	if time.Now().After(me.expiresAt) {
		c.removeLocked(elem)
		telemetry.CacheMissesTotal.Inc()
		return nil, false
	}

	c.order.MoveToFront(elem)
	telemetry.CacheHitsTotal.Inc()
	return me.entry, true
}

// Put reads all data from r and stores it in the cache. Returns an error if
// the data cannot be read. Objects exceeding max_object_size are silently
// rejected (not an error — admission control).
func (c *MemoryCache) Put(key string, r io.Reader, meta EntryMeta) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("cache read: %w", err)
	}

	entry := &Entry{
		Data:        data,
		ContentType: meta.ContentType,
		ETag:        meta.ETag,
		Metadata:    meta.Metadata,
	}
	entrySize := entry.Size()

	// Admission control: reject objects that are too large to cache.
	if entrySize > c.maxObjectSize {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// If the key already exists, remove the old entry first.
	if elem, ok := c.items[key]; ok {
		c.removeLocked(elem)
	}

	// Evict LRU entries until there is room for the new entry.
	for c.sizeBytes+entrySize > c.maxSize && c.order.Len() > 0 {
		c.evictLocked()
	}

	// If the entry still doesn't fit (larger than total capacity after
	// evicting everything), skip caching.
	if c.sizeBytes+entrySize > c.maxSize {
		return nil
	}

	me := &memoryEntry{
		key:       key,
		entry:     entry,
		size:      entrySize,
		expiresAt: time.Now().Add(c.ttl),
	}
	elem := c.order.PushFront(me)
	c.items[key] = elem
	c.sizeBytes += entrySize
	c.updateGauges()
	return nil
}

// Invalidate removes a single key from the cache.
func (c *MemoryCache) Invalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[key]; ok {
		c.removeLocked(elem)
	}
}

// Stats returns current cache utilization.
func (c *MemoryCache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{
		Entries:   len(c.items),
		SizeBytes: c.sizeBytes,
		MaxBytes:  c.maxSize,
	}
}

// -------------------------------------------------------------------------
// INTERNAL
// -------------------------------------------------------------------------

// removeLocked removes an element from the cache. Caller must hold c.mu.
func (c *MemoryCache) removeLocked(elem *list.Element) {
	me := c.order.Remove(elem).(*memoryEntry)
	delete(c.items, me.key)
	c.sizeBytes -= me.size
	c.updateGauges()
}

// evictLocked removes the least recently used entry. Caller must hold c.mu.
func (c *MemoryCache) evictLocked() {
	tail := c.order.Back()
	if tail == nil {
		return
	}
	c.removeLocked(tail)
	telemetry.CacheEvictionsTotal.Inc()
}

// updateGauges sets the current gauge values. Caller must hold c.mu.
func (c *MemoryCache) updateGauges() {
	telemetry.CacheSizeBytes.Set(float64(c.sizeBytes))
	telemetry.CacheEntries.Set(float64(len(c.items)))
}
