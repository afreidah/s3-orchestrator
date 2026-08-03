// -------------------------------------------------------------------------------
// Memory Cache Tests
//
// Author: Alex Freidah
//
// Covers the in-process MemoryCache used as the optional read-side cache:
// basic Get/Set, TTL expiration, the eviction goroutine that runs only when
// a positive TTL is configured, and the size-bounded LRU behavior. Pinned
// because the cache sits in the read hot path and a leak in the eviction
// goroutine or LRU pointer stitching would surface as memory growth in
// production.
// -------------------------------------------------------------------------------

package cache

import (
	"bytes"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// newTestCache constructs a new test cache.
func newTestCache(t *testing.T, maxSize, maxObjSize int64, ttl time.Duration) *MemoryCache {
	t.Helper()
	c, err := NewMemoryCache(MemoryConfig{
		MaxSize:       maxSize,
		MaxObjectSize: maxObjSize,
		TTL:           ttl,
	})
	if err != nil {
		t.Fatalf("NewMemoryCache: %v", err)
	}
	return c
}

// admitAndPut is a test helper for the common admit-then-store sequence.
// Fails the test if Admit returns false; callers expecting rejection
// should call Admit directly and not use this helper.
func admitAndPut(t *testing.T, c *MemoryCache, key string, data []byte, meta EntryMeta) {
	t.Helper()
	if !c.Admit(int64(len(data))) {
		t.Fatalf("Admit(%d) = false, expected true for key %q", len(data), key)
	}
	c.PutBytes(key, data, meta)
}

// TestMemoryCache_PutGet verifies the memory cache put get contract.
func TestMemoryCache_PutGet(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, 1024, 512, time.Minute)

	data := []byte("hello world")
	admitAndPut(t, c, "key1", data, EntryMeta{ContentType: "text/plain", ETag: "abc"})

	entry, ok := c.Get("key1")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if !bytes.Equal(entry.Data, data) {
		t.Errorf("data = %q, want %q", entry.Data, data)
	}
	if entry.ContentType != "text/plain" {
		t.Errorf("content_type = %q, want %q", entry.ContentType, "text/plain")
	}
	if entry.ETag != "abc" {
		t.Errorf("etag = %q, want %q", entry.ETag, "abc")
	}
}

// TestMemoryCache_Miss verifies the memory cache miss path by exercising c.Get.
func TestMemoryCache_Miss(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, 1024, 512, time.Minute)

	_, ok := c.Get("nonexistent")
	if ok {
		t.Fatal("expected cache miss for nonexistent key")
	}
}

// TestMemoryCache_TTLExpiry verifies the memory cache ttlexpiry contract.
// Asserts that Put:.
func TestMemoryCache_TTLExpiry(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		c := newTestCache(t, 1024, 512, 10*time.Millisecond)

		admitAndPut(t, c, "key1", []byte("data"), EntryMeta{})

		// Should be present immediately
		if _, ok := c.Get("key1"); !ok {
			t.Fatal("expected hit before TTL expires")
		}

		time.Sleep(20 * time.Millisecond)
		synctest.Wait()

		// Should be expired
		if _, ok := c.Get("key1"); ok {
			t.Fatal("expected miss after TTL expires")
		}

		// Stats should reflect removal
		stats := c.Stats()
		if stats.Entries != 0 {
			t.Errorf("entries = %d after TTL expiry, want 0", stats.Entries)
		}
	})
}

// TestMemoryCache_AdmitRejectsOversized verifies that Admit returns false
// for objects larger than max_object_size, so callers never bother
// buffering or calling PutBytes for them.
func TestMemoryCache_AdmitRejectsOversized(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, 1024, 100, time.Minute)

	if c.Admit(200) {
		t.Fatal("Admit(200) = true, expected false (max_object_size = 100)")
	}
	if c.Admit(101) {
		t.Fatal("Admit(101) = true, expected false")
	}
	if !c.Admit(100) {
		t.Fatal("Admit(100) = false, expected true (boundary)")
	}
	if !c.Admit(1) {
		t.Fatal("Admit(1) = false, expected true")
	}
	if c.Admit(0) {
		t.Fatal("Admit(0) = true, expected false (zero-size never admitted)")
	}
}

// TestMemoryCache_LRUEviction verifies the memory cache lrueviction contract.
// Asserts that Put first:.
func TestMemoryCache_LRUEviction(t *testing.T) {
	t.Parallel()
	// Cache can hold ~200 bytes. Each entry is ~100 bytes of data.
	c := newTestCache(t, 250, 250, time.Minute)

	admitAndPut(t, c, "first", bytes.Repeat([]byte("A"), 100), EntryMeta{})
	admitAndPut(t, c, "second", bytes.Repeat([]byte("B"), 100), EntryMeta{})

	// Both should be present
	if _, ok := c.Get("first"); !ok {
		t.Fatal("expected hit for first")
	}
	if _, ok := c.Get("second"); !ok {
		t.Fatal("expected hit for second")
	}

	// Adding a third should evict the LRU (first was accessed most recently
	// by the Get above, so second is now LRU... wait, we accessed both.
	// Let's access first again to make second the LRU)
	c.Get("first") // promote first

	admitAndPut(t, c, "third", bytes.Repeat([]byte("C"), 100), EntryMeta{})

	// second should be evicted (LRU)
	if _, ok := c.Get("second"); ok {
		t.Fatal("expected miss for second (LRU evicted)")
	}
	// first should still be present (recently accessed)
	if _, ok := c.Get("first"); !ok {
		t.Fatal("expected hit for first (recently accessed)")
	}
	// third should be present (just added)
	if _, ok := c.Get("third"); !ok {
		t.Fatal("expected hit for third (just added)")
	}
}

// TestMemoryCache_Invalidate verifies the memory cache invalidate contract.
// Asserts that Put:.
func TestMemoryCache_Invalidate(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, 1024, 512, time.Minute)

	admitAndPut(t, c, "key1", []byte("data"), EntryMeta{})

	c.Invalidate("key1")

	if _, ok := c.Get("key1"); ok {
		t.Fatal("expected miss after invalidation")
	}

	// Invalidating a non-existent key should not panic
	c.Invalidate("nonexistent")
}

// TestMemoryCache_OverwriteExisting verifies the memory cache overwrite existing contract.
// Asserts that Put v1:.
func TestMemoryCache_OverwriteExisting(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, 1024, 512, time.Minute)

	admitAndPut(t, c, "key1", []byte("v1"), EntryMeta{ETag: "e1"})
	admitAndPut(t, c, "key1", []byte("v2"), EntryMeta{ETag: "e2"})

	entry, ok := c.Get("key1")
	if !ok {
		t.Fatal("expected hit")
	}
	if string(entry.Data) != "v2" {
		t.Errorf("data = %q, want %q", entry.Data, "v2")
	}
	if entry.ETag != "e2" {
		t.Errorf("etag = %q, want %q", entry.ETag, "e2")
	}

	// Size should account for v2 only, not v1+v2
	if stats := c.Stats(); stats.Entries != 1 {
		t.Errorf("entries = %d, want 1", stats.Entries)
	}
}

// TestMemoryCache_Stats verifies the memory cache stats contract.
// Asserts that empty cache stats = v.
func TestMemoryCache_Stats(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, 1024, 512, time.Minute)

	stats := c.Stats()
	if stats.Entries != 0 || stats.SizeBytes != 0 || stats.MaxBytes != 1024 {
		t.Errorf("empty cache stats = %+v", stats)
	}

	data := []byte("some data here")
	admitAndPut(t, c, "key1", data, EntryMeta{})

	stats = c.Stats()
	if stats.Entries != 1 {
		t.Errorf("entries = %d, want 1", stats.Entries)
	}
	if stats.SizeBytes <= 0 {
		t.Errorf("size_bytes = %d, want > 0", stats.SizeBytes)
	}
}

// TestMemoryCache_ConcurrentAccess verifies the memory cache concurrent access contract.
// Asserts that negative size_bytes:.
func TestMemoryCache_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, 10240, 1024, time.Minute)

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			key := string(rune('A' + i%26))
			data := bytes.Repeat([]byte{byte(i)}, 50) //nolint:gosec // G115: test loop index, always < 256
			if c.Admit(int64(len(data))) {
				c.PutBytes(key, data, EntryMeta{})
			}
			c.Get(key)
			c.Invalidate(key)
		})
	}
	wg.Wait()

	// Should not panic or corrupt state
	stats := c.Stats()
	if stats.SizeBytes < 0 {
		t.Errorf("negative size_bytes: %d", stats.SizeBytes)
	}
}

// TestNewMemoryCache_Validation verifies the new memory cache validation behaviour described by the test name.
func TestNewMemoryCache_Validation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  MemoryConfig
	}{
		{"zero max_size", MemoryConfig{MaxSize: 0, MaxObjectSize: 100, TTL: time.Minute}},
		{"zero max_object_size", MemoryConfig{MaxSize: 1024, MaxObjectSize: 0, TTL: time.Minute}},
		{"zero ttl", MemoryConfig{MaxSize: 1024, MaxObjectSize: 100, TTL: 0}},
		{"max_obj > max_size", MemoryConfig{MaxSize: 100, MaxObjectSize: 200, TTL: time.Minute}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewMemoryCache(tt.cfg)
			if err == nil {
				t.Error("expected error")
			}
		})
	}
}

// TestMemoryCache_InvalidatePrefix verifies prefix-matching invalidation
// drops every key under the prefix, leaves siblings intact, and returns
// the dropped-count.
func TestMemoryCache_InvalidatePrefix(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, 1024, 512, time.Minute)

	for _, k := range []string{"a/1", "a/2", "a/sub/3", "b/1", "c/1"} {
		admitAndPut(t, c, k, []byte("data-"+k), EntryMeta{})
	}

	if got := c.InvalidatePrefix("a/"); got != 3 {
		t.Errorf("InvalidatePrefix(a/) = %d, want 3", got)
	}

	for _, k := range []string{"a/1", "a/2", "a/sub/3"} {
		if _, ok := c.Get(k); ok {
			t.Errorf("expected miss for %q after prefix invalidation", k)
		}
	}
	for _, k := range []string{"b/1", "c/1"} {
		if _, ok := c.Get(k); !ok {
			t.Errorf("expected hit for %q (outside invalidated prefix)", k)
		}
	}

	if got := c.InvalidatePrefix("nonexistent/"); got != 0 {
		t.Errorf("InvalidatePrefix(nonexistent/) = %d, want 0", got)
	}
}

// TestMemoryCache_Clear verifies that Clear empties every entry, returns
// the prior count, and resets the size accounting so subsequent Stats
// reflect an empty cache.
func TestMemoryCache_Clear(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, 1024, 512, time.Minute)

	admitAndPut(t, c, "a", []byte("aaaa"), EntryMeta{})
	admitAndPut(t, c, "b", []byte("bbbb"), EntryMeta{})
	admitAndPut(t, c, "c", []byte("cccc"), EntryMeta{})

	if got := c.Clear(); got != 3 {
		t.Errorf("Clear() = %d, want 3", got)
	}

	if _, ok := c.Get("a"); ok {
		t.Error("expected miss for 'a' after Clear")
	}
	if _, ok := c.Get("b"); ok {
		t.Error("expected miss for 'b' after Clear")
	}

	stats := c.Stats()
	if stats.Entries != 0 {
		t.Errorf("entries = %d after Clear, want 0", stats.Entries)
	}
	if stats.SizeBytes != 0 {
		t.Errorf("size_bytes = %d after Clear, want 0", stats.SizeBytes)
	}

	// Clearing an empty cache returns 0 and does not panic.
	if got := c.Clear(); got != 0 {
		t.Errorf("Clear() on empty cache = %d, want 0", got)
	}

	// Cache stays usable after Clear.
	admitAndPut(t, c, "d", []byte("dddd"), EntryMeta{})
	if _, ok := c.Get("d"); !ok {
		t.Error("expected hit for 'd' after Clear + Put")
	}
}

// TestMemoryCache_EntrySize verifies the memory cache entry size contract.
// Asserts that entry.Size() = , want 26.
func TestMemoryCache_EntrySize(t *testing.T) {
	t.Parallel()
	entry := &Entry{
		Data:        []byte("hello"),
		ContentType: "text/plain",
		ETag:        "abc",
		Metadata:    map[string]string{"key": "value"},
	}

	size := entry.Size()
	// 5 (data) + 10 (content-type) + 3 (etag) + 3+5 (metadata) = 26
	if size != 26 {
		t.Errorf("entry.Size() = %d, want 26", size)
	}
}

// TestMemoryCache_StatsHitsAndMisses verifies lookups are counted, and that an
// expired entry counts as a miss rather than a hit: the entry existed, but the
// caller still had to go to the backend for it.
func TestMemoryCache_StatsHitsAndMisses(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		c := newTestCache(t, 1024, 512, time.Minute)
		admitAndPut(t, c, "key1", []byte("data"), EntryMeta{})

		c.Get("key1")   // hit
		c.Get("key1")   // hit
		c.Get("absent") // miss

		if s := c.Stats(); s.Hits != 2 || s.Misses != 1 {
			t.Errorf("stats = %+v, want 2 hits / 1 miss", s)
		}

		time.Sleep(2 * time.Minute)
		synctest.Wait()
		c.Get("key1") // expired, so a miss

		if s := c.Stats(); s.Hits != 2 || s.Misses != 2 {
			t.Errorf("after expiry stats = %+v, want 2 hits / 2 misses", s)
		}
	})
}
