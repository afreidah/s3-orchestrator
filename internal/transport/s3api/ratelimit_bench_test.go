// -------------------------------------------------------------------------------
// Rate Limiter Benchmarks - Per-IP Token Bucket Throughput
//
// Author: Alex Freidah
//
// Measures the per-request cost of rate limit checks. The Allow method is
// called on every HTTP request, so its overhead directly impacts P99 latency.
// Includes single-IP, multi-IP, and concurrent contention scenarios.
// -------------------------------------------------------------------------------

package s3api

import (
	"fmt"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// newBenchRateLimiter creates a rate limiter with high limits for benchmarking.
func newBenchRateLimiter() *RateLimiter {
	return NewRateLimiter(config.RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 10000,
		Burst:          20000,
	})
}

// BenchmarkRateLimiter_Allow_SingleIP measures the cost of Allow for a
// single IP that already has a limiter entry (steady-state hot path).
func BenchmarkRateLimiter_Allow_SingleIP(b *testing.B) {
	rl := newBenchRateLimiter()
	defer rl.Close()

	rl.Allow("192.168.1.1")

	for b.Loop() {
		rl.Allow("192.168.1.1")
	}
}

// BenchmarkRateLimiter_Allow_MultiIP measures Allow throughput when requests
// come from many distinct IPs (map growth + new limiter allocation).
func BenchmarkRateLimiter_Allow_MultiIP(b *testing.B) {
	rl := newBenchRateLimiter()
	defer rl.Close()

	const n = 10000
	ips := make([]string, n)
	for i := range n {
		ips[i] = fmt.Sprintf("10.0.%d.%d", i/256, i%256)
	}

	b.ResetTimer()
	for b.Loop() {
		rl.Allow(ips[b.N%n])
	}
}

// BenchmarkRateLimiter_Allow_Concurrent measures Allow throughput under
// concurrent request load from multiple goroutines.
func BenchmarkRateLimiter_Allow_Concurrent(b *testing.B) {
	rl := newBenchRateLimiter()
	defer rl.Close()

	const n = 100
	ips := make([]string, n)
	for i := range n {
		ips[i] = fmt.Sprintf("10.0.0.%d", i)
		rl.Allow(ips[i])
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			rl.Allow(ips[i%n])
			i++
		}
	})
}
