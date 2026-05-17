// -------------------------------------------------------------------------------
// CopyObject Materialization Benchmarks
//
// Author: Alex Freidah
//
// Measures the per-copy overhead of the materializer that backs
// CopyObject. The 32 MiB memory-vs-tempfile threshold is the load-
// bearing trade-off  -  in-memory copies pay GC pressure but win on
// throughput; tempfile copies pay disk I/O but bound memory. Running
// this around the threshold pins both branches against a baseline so
// a future tuning change (different threshold, different buffer
// strategy) can be evaluated.
//
// Each iteration covers the full materialize cycle a CopyObject
// invocation runs against one replica: create the sink, write the
// payload through it, seek to zero, drain via io.Copy, and run the
// cleanup. That mirrors PutObject's read-through pattern, so the
// numbers reflect real-end-to-end materialize cost, not just sink
// construction.
// -------------------------------------------------------------------------------

package object

import (
	"bytes"
	"fmt"
	"io"
	"testing"
)

// runMaterializeCycle is the per-iteration body of
// BenchmarkCopyMaterializeSink. Hoisted to a helper so the benchmark
// body stays a flat (size -> sub-bench) dispatch and the
// construct/write/seek/drain/cleanup sequence reads as one named unit.
func runMaterializeCycle(b *testing.B, payload []byte, size int64) {
	b.Helper()
	sink, cleanup, err := newCopyMaterializeSink(size)
	if err != nil {
		b.Fatalf("newCopyMaterializeSink: %v", err)
	}
	defer cleanup()
	if _, err := io.Copy(sink.writer(), bytes.NewReader(payload)); err != nil {
		b.Fatalf("write: %v", err)
	}
	body, err := sink.seekableBody()
	if err != nil {
		b.Fatalf("seekableBody: %v", err)
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
		b.Fatalf("drain: %v", err)
	}
}

// BenchmarkCopyMaterializeSink measures one full materialize cycle at
// payload sizes that straddle the memory-vs-tempfile threshold.
// Sub-threshold sizes exercise the bytes.Buffer branch; super-
// threshold sizes exercise the self-unlinking tempfile branch.
func BenchmarkCopyMaterializeSink(b *testing.B) {
	sizes := []struct {
		name string
		size int
	}{
		{"4KB_memory", 4 * 1024},
		{"1MB_memory", 1 * 1024 * 1024},
		{"32MB_memory_at_threshold", copyMaterializeMemThreshold},
		{"64MB_tempfile", 2 * copyMaterializeMemThreshold},
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

// BenchmarkCopyMaterializeSink_Reset isolates the per-call sink
// construction + cleanup cost from the payload I/O. Useful when
// evaluating allocator-side tuning (pooled buffers, reusable
// tempfile handles) where the construction overhead matters more
// than throughput.
func BenchmarkCopyMaterializeSink_Reset(b *testing.B) {
	for _, size := range []int{
		1 * 1024 * 1024,
		2 * copyMaterializeMemThreshold,
	} {
		b.Run(fmt.Sprintf("%dKB", size/1024), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, cleanup, err := newCopyMaterializeSink(int64(size))
				if err != nil {
					b.Fatalf("newCopyMaterializeSink: %v", err)
				}
				cleanup()
			}
		})
	}
}
