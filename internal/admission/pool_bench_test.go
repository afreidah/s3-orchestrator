// -------------------------------------------------------------------------------
// Admission Pool Benchmarks - Slot Acquire and Release
//
// Author: Alex Freidah
//
// Measures what every admitted request pays on a pool with room: one slot taken
// and given back. Covers one request flow on its own and several sharing the
// pool from every core.
// -------------------------------------------------------------------------------

package admission

import (
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkPool_AcquireRelease measures a slot taken and given back by one
// request flow on an idle pool.
func BenchmarkPool_AcquireRelease(b *testing.B) {
	p, _ := newPool(1000)
	ctx := b.Context()
	for b.Loop() {
		slot, _ := p.Acquire(ctx, "bucket", time.Second)
		slot.Release()
	}
}

// BenchmarkPool_AcquireRelease_Concurrent measures slots taken and given back
// from every core, spread over four request flows.
func BenchmarkPool_AcquireRelease_Concurrent(b *testing.B) {
	p, _ := newPool(1000)
	ctx := b.Context()
	names := []string{"a", "b", "c", "d"}
	var next atomic.Int64
	b.RunParallel(func(pb *testing.PB) {
		name := names[next.Add(1)%int64(len(names))]
		for pb.Next() {
			slot, _ := p.Acquire(ctx, name, time.Second)
			slot.Release()
		}
	})
}
