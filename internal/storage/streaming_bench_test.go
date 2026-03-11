// -------------------------------------------------------------------------------
// Streaming Benchmarks - io.Copy and streamCopy Throughput
//
// Author: Alex Freidah
//
// Measures baseline io.Copy throughput at various payload sizes and end-to-end
// streaming throughput through backendCore.streamCopy with mock backends. These
// benchmarks isolate copy overhead from real I/O and serve as the baseline for
// buffer pooling and transport tuning improvements.
// -------------------------------------------------------------------------------

package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
	"time"
)

// -------------------------------------------------------------------------
// RAW io.Copy THROUGHPUT
// -------------------------------------------------------------------------

// BenchmarkIOCopy measures the baseline cost of io.Copy at various payload
// sizes. This is the pattern used on every GET/PUT proxy path and in
// streamCopy for rebalancer/replicator copies.
func BenchmarkIOCopy(b *testing.B) {
	sizes := []int{
		4 * 1024,         // 4KB - small metadata/config
		64 * 1024,        // 64KB - typical small object
		1024 * 1024,      // 1MB - medium object
		16 * 1024 * 1024, // 16MB - large object
	}

	for _, size := range sizes {
		data := make([]byte, size)
		b.Run(fmt.Sprintf("%dKB", size/1024), func(b *testing.B) {
			b.SetBytes(int64(size))
			for b.Loop() {
				src := bytes.NewReader(data)
				_, _ = io.Copy(io.Discard, src)
			}
		})
	}
}

// -------------------------------------------------------------------------
// streamCopy THROUGHPUT (MOCK BACKENDS)
// -------------------------------------------------------------------------

// BenchmarkStreamCopy measures end-to-end streaming throughput through the
// backendCore.streamCopy path used by the rebalancer and replicator. Uses
// in-memory mock backends to isolate the copy overhead from real I/O.
func BenchmarkStreamCopy(b *testing.B) {
	sizes := []struct {
		name string
		size int
	}{
		{"64KB", 64 * 1024},
		{"1MB", 1024 * 1024},
		{"16MB", 16 * 1024 * 1024},
	}

	for _, tc := range sizes {
		b.Run(tc.name, func(b *testing.B) {
			src := newMockBackend()
			dst := newMockBackend()

			data := make([]byte, tc.size)
			_, _ = src.PutObject(context.Background(), "bench-key", bytes.NewReader(data), int64(tc.size), "application/octet-stream", nil)

			core := &backendCore{
				backendTimeout: 30 * time.Second,
			}

			b.SetBytes(int64(tc.size))
			b.ResetTimer()
			for b.Loop() {
				_ = core.streamCopy(context.Background(), src, dst, "bench-key")
			}
		})
	}
}
