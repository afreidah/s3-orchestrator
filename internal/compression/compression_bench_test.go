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
	"fmt"
	"io"
	"testing"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

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

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

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

// BenchmarkCompressIncompressible and BenchmarkCompressLogLike are the evidence
// behind min_ratio deciding on a finished encoding rather than on a sample of
// one. Both report the ratio they achieved alongside throughput.
//
// The comparison to draw is between them: data zstd cannot shrink is the
// cheapest input it has, because it detects unshrinkable blocks and stores them
// raw instead of searching for matches. Encoding an object that turns out to be
// incompressible therefore costs less than encoding one that compresses, which
// is the cost the write path pays willingly - so paying it to reach an exact
// answer is cheaper than sampling to reach an approximate one.
func BenchmarkCompressIncompressible(b *testing.B) {
	benchRatio(b, incompressible(b, DefaultChunkSize*4))
}

// BenchmarkCompressLogLike measures realistic structured text, the compressible
// case the repeating fixture in BenchmarkCompress is too uniform to represent.
func BenchmarkCompressLogLike(b *testing.B) {
	benchRatio(b, logLike(DefaultChunkSize*4))
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// benchRatio encodes src and reports both throughput and the ratio reached, so
// a run states what it compressed as well as how fast.
func benchRatio(b *testing.B, src []byte) {
	b.Helper()
	c := benchCodec(b)
	var encoded int64

	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		n, err := c.Compress(discardWriter{}, bytes.NewReader(src))
		if err != nil {
			b.Fatalf("Compress: %v", err)
		}
		encoded = n
	}
	b.StopTimer()
	b.ReportMetric(float64(encoded)/float64(len(src)), "ratio")
}

// logLike returns n bytes of structured text with varying field values, which
// compresses like the JSON logs and source files real deployments store rather
// than like a single repeated line.
//
// The values come from an inline xorshift rather than math/rand so the fixture
// is deterministic across runs without pulling in a generator the security
// linter flags.
func logLike(n int) []byte {
	levels := []string{"INFO", "WARN", "ERROR", "DEBUG"}
	events := []string{"put.completed", "get.served", "replica.created", "scrub.verified"}
	state := uint64(0x9E3779B97F4A7C15)
	next := func() uint64 {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		return state
	}

	var out bytes.Buffer
	out.Grow(n)
	for out.Len() < n {
		r := next()
		fmt.Fprintf(&out,
			"2026-08-21T%02d:%02d:%02dZ %s event=%s backend=b%d key=bucket/obj-%d size=%d dur_ms=%d\n",
			r%24, (r>>8)%60, (r>>16)%60,
			levels[r%uint64(len(levels))], events[(r>>24)%uint64(len(events))],
			r%8, r%100000, r%10485760, r%2000)
	}
	return out.Bytes()[:n]
}

// discardWriter counts nothing and keeps nothing, so the benchmark measures the
// codec rather than the destination.
type discardWriter struct{}

// Write implements io.Writer.
func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

var _ io.Writer = discardWriter{}
