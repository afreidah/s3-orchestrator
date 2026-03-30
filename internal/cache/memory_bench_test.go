// -------------------------------------------------------------------------------
// Memory Cache Benchmarks - LRU Get, Put, and Eviction Throughput
//
// Author: Alex Freidah
//
// Measures per-operation cost of the in-memory object cache on the hot read
// path. Includes hit, miss, put, eviction, and concurrent contention scenarios.
// -------------------------------------------------------------------------------

package cache

import (
	"bytes"
	"fmt"
	"testing"
	"time"
)

// BenchmarkMemoryCache_Get_Hit measures the cost of a cache hit with LRU
// promotion. This is the per-GET cost when the object is cached.
func BenchmarkMemoryCache_Get_Hit(b *testing.B) {
	c, _ := NewMemoryCache(MemoryConfig{
		MaxSize:       64 << 20,
		MaxObjectSize: 1 << 20,
		TTL:           5 * time.Minute,
	})
	_ = c.Put("hit-key", bytes.NewReader(make([]byte, 1024)), EntryMeta{ContentType: "application/octet-stream"})

	for b.Loop() {
		entry, ok := c.Get("hit-key")
		if !ok {
			b.Fatal("expected cache hit")
		}
		_ = entry
	}
}

// BenchmarkMemoryCache_Get_Miss measures the cost of a cache miss (map lookup
// returning false).
func BenchmarkMemoryCache_Get_Miss(b *testing.B) {
	c, _ := NewMemoryCache(MemoryConfig{
		MaxSize:       64 << 20,
		MaxObjectSize: 1 << 20,
		TTL:           5 * time.Minute,
	})

	for b.Loop() {
		c.Get("nonexistent")
	}
}

// BenchmarkMemoryCache_Put measures the cost of inserting an object into the
// cache without triggering eviction.
func BenchmarkMemoryCache_Put(b *testing.B) {
	c, _ := NewMemoryCache(MemoryConfig{
		MaxSize:       1 << 30,
		MaxObjectSize: 1 << 20,
		TTL:           5 * time.Minute,
	})
	data := make([]byte, 4096)

	for b.Loop() {
		_ = c.Put("put-key", bytes.NewReader(data), EntryMeta{})
	}
}

// BenchmarkMemoryCache_Put_Eviction measures put throughput when every insert
// triggers LRU eviction (cache is at capacity).
func BenchmarkMemoryCache_Put_Eviction(b *testing.B) {
	objSize := 4096
	// Cache fits exactly 10 objects, so each new insert evicts one.
	c, _ := NewMemoryCache(MemoryConfig{
		MaxSize:       int64(objSize * 10),
		MaxObjectSize: int64(objSize),
		TTL:           5 * time.Minute,
	})
	data := make([]byte, objSize)
	for i := range 10 {
		_ = c.Put(fmt.Sprintf("seed-%d", i), bytes.NewReader(data), EntryMeta{})
	}

	b.ResetTimer()
	for b.Loop() {
		_ = c.Put("evict-key", bytes.NewReader(data), EntryMeta{})
	}
}

// BenchmarkMemoryCache_Concurrent_ReadWrite measures cache performance under
// realistic concurrent access (90% reads, 10% writes).
func BenchmarkMemoryCache_Concurrent_ReadWrite(b *testing.B) {
	c, _ := NewMemoryCache(MemoryConfig{
		MaxSize:       64 << 20,
		MaxObjectSize: 1 << 20,
		TTL:           5 * time.Minute,
	})

	const n = 100
	keys := make([]string, n)
	data := make([]byte, 1024)
	for i := range n {
		keys[i] = fmt.Sprintf("key-%d", i)
		_ = c.Put(keys[i], bytes.NewReader(data), EntryMeta{})
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := keys[i%n]
			if i%10 == 0 {
				_ = c.Put(key, bytes.NewReader(data), EntryMeta{})
			} else {
				c.Get(key)
			}
			i++
		}
	})
}
