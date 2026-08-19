// -------------------------------------------------------------------------------
// Chunked Zstandard Codec Benchmarks
//
// Author: Alex Freidah
//
// The number to watch is allocations per object. One encoder and one decoder
// are shared across every request, so encoding a second object must not
// allocate a second codec - if it did, a busy server would build one per
// upload. Run with -benchmem.
// -------------------------------------------------------------------------------

package compression

import (
	"bytes"
	"io"
	"testing"
)

// benchCodec builds a codec at the production default chunk size.
func benchCodec(b *testing.B) *Codec {
	b.Helper()
	c, err := NewCodec(DefaultLevel, DefaultChunkSize)
	if err != nil {
		b.Fatalf("NewCodec: %v", err)
	}
	b.Cleanup(c.Close)
	return c
}

// BenchmarkCompress measures one multi-chunk object through the encoder.
func BenchmarkCompress(b *testing.B) {
	c := benchCodec(b)
	src := compressible(DefaultChunkSize * 4)
	dst := &discardWriter{}

	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := c.Compress(dst, bytes.NewReader(src)); err != nil {
			b.Fatalf("Compress: %v", err)
		}
	}
}

// BenchmarkDecompress measures reading one multi-chunk object back.
func BenchmarkDecompress(b *testing.B) {
	c := benchCodec(b)
	src := compressible(DefaultChunkSize * 4)

	var stored bytes.Buffer
	if _, err := c.Compress(&stored, bytes.NewReader(src)); err != nil {
		b.Fatalf("seed Compress: %v", err)
	}
	blob := stored.Bytes()
	scratch := make([]byte, 64<<10)

	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		r, err := c.Decompress(bytes.NewReader(blob))
		if err != nil {
			b.Fatalf("Decompress: %v", err)
		}
		for {
			if _, err := r.Read(scratch); err != nil {
				break
			}
		}
		r.Close()
	}
}

// discardWriter counts nothing and keeps nothing, so the benchmark measures the
// codec rather than the destination.
type discardWriter struct{}

// Write implements io.Writer.
func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

var _ io.Writer = discardWriter{}
