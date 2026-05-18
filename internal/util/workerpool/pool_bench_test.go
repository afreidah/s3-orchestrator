// -------------------------------------------------------------------------------
// Worker Pool Benchmarks
//
// Author: Alex Freidah
//
// Measures the cost of Run across batch sizes and unit-of-work shapes. The
// pre-#861 implementation spawned one goroutine per item under a semaphore;
// the post-#861 implementation spawns at most min(concurrency, len(items))
// goroutines and feeds them via a jobs channel. The benchmarks pin both
// dimensions: dispatch overhead (negligible fn) and amortized cost when
// the fn itself does meaningful work.
// -------------------------------------------------------------------------------

package workerpool

import (
	"context"
	"runtime"
	"testing"
)

// noopWork is the cheapest possible fn so the bench measures dispatch
// overhead rather than the work itself.
func noopWork(_ context.Context, _ int) {}

// cpuWork burns a fixed amount of work per item so the bench reflects
// goroutine scheduling under a realistic-ish payload rather than pure
// dispatch.
func cpuWork(_ context.Context, _ int) {
	x := 0
	for i := range 1024 {
		x += i
	}
	runtime.KeepAlive(x)
}

// BenchmarkRun_Dispatch sweeps batch size with a no-op fn. Numbers
// reflect goroutine creation + channel handoff overhead, isolated from
// any per-item work cost. The 16-chunk and 256-chunk variants are
// representative of cleanup/replication tick batch sizes.
func BenchmarkRun_Dispatch(b *testing.B) {
	concurrency := runtime.GOMAXPROCS(0)
	for _, n := range []int{1, 16, 64, 256, 1024} {
		items := make([]int, n)
		b.Run(itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				Run(context.Background(), concurrency, items, noopWork)
			}
		})
	}
}

// BenchmarkRun_CPU sweeps batch size with a small CPU payload per item
// so the dispatch overhead is amortized across real work. Useful for
// confirming the refactor does not regress throughput on payloads
// large enough to hide the per-item cost.
func BenchmarkRun_CPU(b *testing.B) {
	concurrency := runtime.GOMAXPROCS(0)
	for _, n := range []int{16, 256, 1024} {
		items := make([]int, n)
		b.Run(itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				Run(context.Background(), concurrency, items, cpuWork)
			}
		})
	}
}

// BenchmarkRun_SingleItem isolates the small-batch fast path. The
// pre-#861 implementation paid for goroutine + semaphore even for
// one item; the post-#861 implementation calls fn inline.
func BenchmarkRun_SingleItem(b *testing.B) {
	items := []int{0}
	b.ReportAllocs()
	for b.Loop() {
		Run(context.Background(), 4, items, noopWork)
	}
}

// itoa is a small inline helper to avoid pulling in strconv just for
// sub-bench names.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
