// -------------------------------------------------------------------------------
// LocationCache Benchmarks
//
// Author: Alex Freidah
//
// Measures the LocationCache hot path: cache hits, cache misses, and the
// concurrent read-heavy and write-heavy mixes the read failover path
// generates under load. Hits are sub-microsecond and dominate steady
// state; the parallel variants pin the lock-contention profile so any
// regression in the cache's sync primitive shows up as a measurable
// throughput drop.
// -------------------------------------------------------------------------------

package object

import (
	"fmt"
	"testing"
	"time"
)

// BenchmarkLocationCache_Get_Hit measures the location cache get hit path by exercising c.Close, c.Set, c.Get.
func BenchmarkLocationCache_Get_Hit(b *testing.B) {
	c := NewLocationCache(time.Minute)
	defer c.Close()
	c.Set("key", "backend-a")

	for b.Loop() {
		c.Get("key")
	}
}

// BenchmarkLocationCache_Get_Miss measures the location cache get miss path by exercising c.Close, c.Get.
func BenchmarkLocationCache_Get_Miss(b *testing.B) {
	c := NewLocationCache(time.Minute)
	defer c.Close()

	for b.Loop() {
		c.Get("nonexistent")
	}
}

// BenchmarkLocationCache_Set measures the location cache set path by exercising c.Close, c.Set.
func BenchmarkLocationCache_Set(b *testing.B) {
	c := NewLocationCache(time.Minute)
	defer c.Close()

	for b.Loop() {
		c.Set("key", "backend-a")
	}
}

// BenchmarkLocationCache_Concurrent_ReadHeavy measures the location cache concurrent read heavy path by exercising c.Close, fmt.Sprintf, c.Set.
func BenchmarkLocationCache_Concurrent_ReadHeavy(b *testing.B) {
	c := NewLocationCache(time.Minute)
	defer c.Close()

	// Pre-compute keys to keep fmt.Sprintf out of the measured loop.
	const n = 1000
	keys := make([]string, n)
	for i := range n {
		keys[i] = fmt.Sprintf("key-%d", i)
		c.Set(keys[i], "backend-a")
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := keys[i%n]
			if i%10 == 0 {
				c.Set(key, "backend-b") // 10% writes
			} else {
				c.Get(key) // 90% reads
			}
			i++
		}
	})
}

// BenchmarkLocationCache_Concurrent_WriteHeavy measures the location cache concurrent write heavy path by exercising c.Close, fmt.Sprintf, pb.Next.
func BenchmarkLocationCache_Concurrent_WriteHeavy(b *testing.B) {
	c := NewLocationCache(time.Minute)
	defer c.Close()

	const n = 1000
	keys := make([]string, n)
	for i := range n {
		keys[i] = fmt.Sprintf("key-%d", i)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := keys[i%n]
			if i%2 == 0 {
				c.Set(key, "backend-a") // 50% writes
			} else {
				c.Get(key) // 50% reads
			}
			i++
		}
	})
}

// BenchmarkTTLCache_Eviction is in internal/syncutil/ttlcache_test.go where
// white-box access to cache internals is available.

// BenchmarkLocationCache_Contention_GetSetDelete measures the location cache contention get set delete path by exercising c.Close, fmt.Sprintf, c.Set.
func BenchmarkLocationCache_Contention_GetSetDelete(b *testing.B) {
	c := NewLocationCache(time.Minute)
	defer c.Close()

	const n = 500
	keys := make([]string, n)
	for i := range n {
		keys[i] = fmt.Sprintf("key-%d", i)
		c.Set(keys[i], "backend-a")
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := keys[i%n]
			switch i % 3 {
			case 0:
				c.Get(key)
			case 1:
				c.Set(key, "backend-b")
			case 2:
				c.Delete(key)
			}
			i++
		}
	})
}
