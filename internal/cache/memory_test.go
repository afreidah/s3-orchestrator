// -------------------------------------------------------------------------------
// Memory Cache Tests
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package cache

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

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

func TestMemoryCache_PutGet(t *testing.T) {
	c := newTestCache(t, 1024, 512, time.Minute)

	data := []byte("hello world")
	if err := c.Put("key1", bytes.NewReader(data), EntryMeta{ContentType: "text/plain", ETag: "abc"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

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

func TestMemoryCache_Miss(t *testing.T) {
	c := newTestCache(t, 1024, 512, time.Minute)

	_, ok := c.Get("nonexistent")
	if ok {
		t.Fatal("expected cache miss for nonexistent key")
	}
}

func TestMemoryCache_TTLExpiry(t *testing.T) {
	c := newTestCache(t, 1024, 512, 10*time.Millisecond)

	if err := c.Put("key1", bytes.NewReader([]byte("data")), EntryMeta{}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Should be present immediately
	if _, ok := c.Get("key1"); !ok {
		t.Fatal("expected hit before TTL expires")
	}

	time.Sleep(20 * time.Millisecond)

	// Should be expired
	if _, ok := c.Get("key1"); ok {
		t.Fatal("expected miss after TTL expires")
	}

	// Stats should reflect removal
	stats := c.Stats()
	if stats.Entries != 0 {
		t.Errorf("entries = %d after TTL expiry, want 0", stats.Entries)
	}
}

func TestMemoryCache_MaxObjectSize(t *testing.T) {
	c := newTestCache(t, 1024, 100, time.Minute)

	// Object larger than max_object_size should be silently rejected
	big := bytes.Repeat([]byte("X"), 200)
	if err := c.Put("big", bytes.NewReader(big), EntryMeta{}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if _, ok := c.Get("big"); ok {
		t.Fatal("expected miss for oversized object")
	}

	// Stats should be empty
	if stats := c.Stats(); stats.Entries != 0 {
		t.Errorf("entries = %d, want 0", stats.Entries)
	}
}

func TestMemoryCache_LRUEviction(t *testing.T) {
	// Cache can hold ~200 bytes. Each entry is ~100 bytes of data.
	c := newTestCache(t, 250, 250, time.Minute)

	if err := c.Put("first", bytes.NewReader(bytes.Repeat([]byte("A"), 100)), EntryMeta{}); err != nil {
		t.Fatalf("Put first: %v", err)
	}
	if err := c.Put("second", bytes.NewReader(bytes.Repeat([]byte("B"), 100)), EntryMeta{}); err != nil {
		t.Fatalf("Put second: %v", err)
	}

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

	if err := c.Put("third", bytes.NewReader(bytes.Repeat([]byte("C"), 100)), EntryMeta{}); err != nil {
		t.Fatalf("Put third: %v", err)
	}

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

func TestMemoryCache_Invalidate(t *testing.T) {
	c := newTestCache(t, 1024, 512, time.Minute)

	if err := c.Put("key1", bytes.NewReader([]byte("data")), EntryMeta{}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	c.Invalidate("key1")

	if _, ok := c.Get("key1"); ok {
		t.Fatal("expected miss after invalidation")
	}

	// Invalidating a non-existent key should not panic
	c.Invalidate("nonexistent")
}

func TestMemoryCache_OverwriteExisting(t *testing.T) {
	c := newTestCache(t, 1024, 512, time.Minute)

	if err := c.Put("key1", bytes.NewReader([]byte("v1")), EntryMeta{ETag: "e1"}); err != nil {
		t.Fatalf("Put v1: %v", err)
	}
	if err := c.Put("key1", bytes.NewReader([]byte("v2")), EntryMeta{ETag: "e2"}); err != nil {
		t.Fatalf("Put v2: %v", err)
	}

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

func TestMemoryCache_Stats(t *testing.T) {
	c := newTestCache(t, 1024, 512, time.Minute)

	stats := c.Stats()
	if stats.Entries != 0 || stats.SizeBytes != 0 || stats.MaxBytes != 1024 {
		t.Errorf("empty cache stats = %+v", stats)
	}

	data := []byte("some data here")
	if err := c.Put("key1", bytes.NewReader(data), EntryMeta{}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	stats = c.Stats()
	if stats.Entries != 1 {
		t.Errorf("entries = %d, want 1", stats.Entries)
	}
	if stats.SizeBytes <= 0 {
		t.Errorf("size_bytes = %d, want > 0", stats.SizeBytes)
	}
}

func TestMemoryCache_ConcurrentAccess(t *testing.T) {
	c := newTestCache(t, 10240, 1024, time.Minute)

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := string(rune('A' + i%26))
			data := bytes.Repeat([]byte{byte(i)}, 50)
			_ = c.Put(key, bytes.NewReader(data), EntryMeta{})
			c.Get(key)
			c.Invalidate(key)
		}(i)
	}
	wg.Wait()

	// Should not panic or corrupt state
	stats := c.Stats()
	if stats.SizeBytes < 0 {
		t.Errorf("negative size_bytes: %d", stats.SizeBytes)
	}
}

func TestNewMemoryCache_Validation(t *testing.T) {
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

func TestMemoryCache_EntrySize(t *testing.T) {
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
