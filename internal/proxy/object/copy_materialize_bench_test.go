// -------------------------------------------------------------------------------
// MaterializedBody Benchmarks
//
// Author: Alex Freidah
//
// Measures the per-call overhead of the materializer that backs
// PutObject and CopyObject source materialization. The 32 MiB memory-
// vs-tempfile threshold is the load-bearing trade-off  -  in-memory
// copies pay GC pressure but win on throughput; tempfile copies pay
// disk I/O but bound memory. Running this around the threshold pins
// both branches against a baseline so a future tuning change can be
// evaluated.
//
// Each iteration covers the full materialize cycle one PutObject or
// CopyObject invocation runs: create the sink, write the payload
// through it (optionally tee'd into a SHA-256), rewind, drain, and
// cleanup. That mirrors the real PutObject pattern with integrity
// hashing enabled, so the numbers reflect end-to-end materialize cost.
// -------------------------------------------------------------------------------

package object

import (
	"bytes"
	"fmt"
	"io"
	"testing"
)

// runMaterializeCycle is the per-iteration body of
// BenchmarkMaterializedBody. Hoisted to a helper so the benchmark body
// stays a flat (size -> sub-bench) dispatch and the
// construct/write/seek/drain/cleanup sequence reads as one named unit.
func runMaterializeCycle(b *testing.B, payload []byte, size int64) {
	b.Helper()
	mb, err := newMaterializedBody(bytes.NewReader(payload), size, newSHA256())
	if err != nil {
		b.Fatalf("newMaterializedBody: %v", err)
	}
	defer mb.Cleanup()
	body, err := mb.Reader()
	if err != nil {
		b.Fatalf("Reader: %v", err)
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
		b.Fatalf("drain: %v", err)
	}
}

// BenchmarkMaterializedBody measures one full materialize cycle at
// payload sizes that straddle the memory-vs-tempfile threshold.
// Sub-threshold sizes exercise the bytes.Buffer branch; super-
// threshold sizes exercise the self-unlinking tempfile branch.
func BenchmarkMaterializedBody(b *testing.B) {
	sizes := []struct {
		name string
		size int
	}{
		{"4KB_memory", 4 * 1024},
		{"1MB_memory", 1 * 1024 * 1024},
		{"32MB_memory_at_threshold", materializeMemThreshold},
		{"64MB_tempfile", 2 * materializeMemThreshold},
	}

	for _, tc := range sizes {
		b.Run(tc.name, func(b *testing.B) {
			payload := make([]byte, tc.size)
			b.SetBytes(int64(tc.size))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				runMaterializeCycle(b, payload, int64(tc.size))
			}
		})
	}
}

// BenchmarkMaterializedBody_Reset isolates the per-call sink
// construction + cleanup cost from the payload I/O. Useful when
// evaluating allocator-side tuning (pooled buffers, reusable tempfile
// handles) where the construction overhead matters more than
// throughput.
func BenchmarkMaterializedBody_Reset(b *testing.B) {
	for _, size := range []int{
		1 * 1024 * 1024,
		2 * materializeMemThreshold,
	} {
		b.Run(fmt.Sprintf("%dKB", size/1024), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				mb, err := emptyMaterializedBody(int64(size))
				if err != nil {
					b.Fatalf("emptyMaterializedBody: %v", err)
				}
				mb.Cleanup()
			}
		})
	}
}
