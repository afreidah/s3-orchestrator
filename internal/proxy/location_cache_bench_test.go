package proxy

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func BenchmarkLocationCache_Get_Hit(b *testing.B) {
	c := NewLocationCache(time.Minute)
	defer c.Close()
	c.Set("key", "backend-a")

	for b.Loop() {
		c.Get("key")
	}
}

func BenchmarkLocationCache_Get_Miss(b *testing.B) {
	c := NewLocationCache(time.Minute)
	defer c.Close()

	for b.Loop() {
		c.Get("nonexistent")
	}
}

func BenchmarkLocationCache_Set(b *testing.B) {
	c := NewLocationCache(time.Minute)
	defer c.Close()

	for b.Loop() {
		c.Set("key", "backend-a")
	}
}

func BenchmarkLocationCache_Concurrent_ReadHeavy(b *testing.B) {
	c := NewLocationCache(time.Minute)
	defer c.Close()

	// Pre-populate 1000 entries
	for i := range 1000 {
		c.Set(fmt.Sprintf("key-%d", i), "backend-a")
	}

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("key-%d", i%1000)
			if i%10 == 0 {
				c.Set(key, "backend-b") // 10% writes
			} else {
				c.Get(key) // 90% reads
			}
			i++
		}
	})
}

func BenchmarkLocationCache_Concurrent_WriteHeavy(b *testing.B) {
	c := NewLocationCache(time.Minute)
	defer c.Close()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("key-%d", i%1000)
			if i%2 == 0 {
				c.Set(key, "backend-a") // 50% writes
			} else {
				c.Get(key) // 50% reads
			}
			i++
		}
	})
}

func BenchmarkLocationCache_Eviction(b *testing.B) {
	sizes := []int{100, 1000, 10000}

	for _, n := range sizes {
		b.Run(fmt.Sprintf("%d_entries", n), func(b *testing.B) {
			c := &LocationCache{
				entries: make(map[string]locationCacheEntry, n),
				ttl:     time.Minute,
				stop:    make(chan struct{}),
			}
			defer close(c.stop)

			// All entries expired
			expired := time.Now().Add(-time.Second)
			for i := range n {
				c.entries[fmt.Sprintf("key-%d", i)] = locationCacheEntry{
					backendName: "b1",
					expiry:      expired,
				}
			}

			b.ResetTimer()
			for b.Loop() {
				c.evict()
				// Refill for next iteration
				b.StopTimer()
				for i := range n {
					c.entries[fmt.Sprintf("key-%d", i)] = locationCacheEntry{
						backendName: "b1",
						expiry:      expired,
					}
				}
				b.StartTimer()
			}
		})
	}
}

func BenchmarkLocationCache_Contention_GetSetDelete(b *testing.B) {
	c := NewLocationCache(time.Minute)
	defer c.Close()

	// Pre-populate
	for i := range 500 {
		c.Set(fmt.Sprintf("key-%d", i), "backend-a")
	}

	var wg sync.WaitGroup
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		wg.Add(1)
		defer wg.Done()
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("key-%d", i%500)
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
