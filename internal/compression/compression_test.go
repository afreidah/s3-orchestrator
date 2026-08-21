// -------------------------------------------------------------------------------
// Chunked Zstandard Codec Tests
//
// Author: Alex Freidah
//
// The properties that matter here are that the bytes survive a round trip at
// every size that straddles a chunk boundary, that the object stays a valid
// zstd stream a stock decoder can read without knowing about the seek table,
// and that the chunk boundary this package owns actually lands where it claims.
// -------------------------------------------------------------------------------

package compression

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"os/exec"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// testChunk is small enough to cross several boundaries without large fixtures.
const testChunk = MinChunkSize

// newTestCodec builds a codec at the test chunk size and closes it with the test.
func newTestCodec(t testing.TB) *Codec {
	t.Helper()
	c, err := NewCodec(DefaultLevel, testChunk)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// compressible returns n bytes that zstd can actually shrink, so a test
// asserting on stored size is not fighting incompressible input.
func compressible(n int) []byte {
	out := make([]byte, 0, n)
	line := []byte("the quick brown fox jumps over the lazy dog 0123456789\n")
	for len(out) < n {
		out = append(out, line...)
	}
	return out[:n]
}

// roundTrip compresses src and reads the whole object back.
func roundTrip(t *testing.T, c *Codec, src []byte) []byte {
	t.Helper()
	var stored bytes.Buffer
	n, err := c.Compress(&stored, bytes.NewReader(src))
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if n != int64(stored.Len()) {
		t.Errorf("Compress reported %d bytes, buffer holds %d", n, stored.Len())
	}

	r, err := c.Decompress(bytes.NewReader(stored.Bytes()))
	if err != nil {
		t.Fatalf("Decompress: %v", err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read decompressed: %v", err)
	}
	return got
}

// TestRoundTrip_SizeBoundaries walks the sizes where the chunk boundary is the
// thing most likely to be wrong: nothing, less than a chunk, exactly a chunk,
// one byte either side of it, and several chunks with a ragged last one.
func TestRoundTrip_SizeBoundaries(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)

	sizes := []int{
		0,
		1,
		testChunk - 1,
		testChunk,
		testChunk + 1,
		testChunk * 3,
		testChunk*3 + 17,
	}
	for _, size := range sizes {
		want := compressible(size)
		got := roundTrip(t, c, want)
		if !bytes.Equal(got, want) {
			t.Errorf("size %d: round trip returned %d bytes, want %d", size, len(got), len(want))
		}
	}
}

// TestRoundTrip_IncompressibleData asserts random input survives intact. It
// will not shrink - that is what motivates skipping it on the write path - but
// it must still decode to exactly what went in.
func TestRoundTrip_IncompressibleData(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)

	want := make([]byte, testChunk*2+5)
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // G404: test fixture, not security material
	if _, err := rng.Read(want); err != nil {
		t.Fatalf("seed random: %v", err)
	}

	if got := roundTrip(t, c, want); !bytes.Equal(got, want) {
		t.Error("incompressible round trip did not return the input")
	}
}

// TestCompress_EmitsOneFramePerChunk pins the boundary this package owns. The
// seekable library writes one frame per Write call and has no chunk-size
// option, so if Compress batched wrongly the object would still round trip
// while seeking at the wrong granularity - which only shows up as a cost.
func TestCompress_EmitsOneFramePerChunk(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)

	cases := []struct {
		size  int
		want  int
		label string
	}{
		{testChunk, 1, "exactly one chunk"},
		{testChunk + 1, 2, "one byte into the second"},
		{testChunk * 4, 4, "four whole chunks"},
		{testChunk*4 - 1, 4, "four chunks, last one short"},
	}

	for _, tc := range cases {
		var stored bytes.Buffer
		if _, err := c.Compress(&stored, bytes.NewReader(compressible(tc.size))); err != nil {
			t.Fatalf("%s: Compress: %v", tc.label, err)
		}
		if got := countDataFrames(t, stored.Bytes()); got != tc.want {
			t.Errorf("%s: %d frames, want %d", tc.label, got, tc.want)
		}
	}
}

// TestCompress_ShortReadsDoNotSplitChunks feeds the source through a reader
// that returns a little at a time. A chunk boundary must follow the configured
// size, not whatever the reader happened to hand over, or an object arriving
// over a slow network would be framed differently from the same bytes read
// from disk.
func TestCompress_ShortReadsDoNotSplitChunks(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)

	src := compressible(testChunk * 3)
	var stored bytes.Buffer
	if _, err := c.Compress(&stored, iotest_oneByteReader(bytes.NewReader(src))); err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if got := countDataFrames(t, stored.Bytes()); got != 3 {
		t.Errorf("%d frames from a dribbling reader, want 3", got)
	}
	if got := roundTrip(t, c, src); !bytes.Equal(got, src) {
		t.Error("dribbling reader changed the decoded bytes")
	}
}

// TestCompress_PropagatesSourceError asserts a failing source is reported
// rather than silently producing a truncated object.
func TestCompress_PropagatesSourceError(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)

	sentinel := errors.New("backend went away")
	src := io.MultiReader(bytes.NewReader(compressible(testChunk)), errReader{sentinel})

	var stored bytes.Buffer
	if _, err := c.Compress(&stored, src); !errors.Is(err, sentinel) {
		t.Errorf("Compress error = %v, want it to wrap the source error", err)
	}
}

// TestStoredObject_DecodesWithStockZstd is the interoperability claim. The
// seekable spec places the seek table in a skippable frame precisely so an
// ordinary decoder ignores it, but the spec stops short of guaranteeing the
// whole file stays readable, so this is what establishes it here.
func TestStoredObject_DecodesWithStockZstd(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)

	want := compressible(testChunk*3 + 11)
	var stored bytes.Buffer
	if _, err := c.Compress(&stored, bytes.NewReader(want)); err != nil {
		t.Fatalf("Compress: %v", err)
	}

	dec, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatalf("stock decoder: %v", err)
	}
	defer dec.Close()

	got, err := dec.DecodeAll(stored.Bytes(), nil)
	if err != nil {
		t.Fatalf("stock zstd could not decode the stored object: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Error("stock zstd decoded different bytes")
	}
}

// TestStoredObject_DecodesWithZstdCLI is the same claim against the real tool,
// which is what an operator would reach for to recover an object without the
// orchestrator. Skipped where zstd is not installed.
func TestStoredObject_DecodesWithZstdCLI(t *testing.T) {
	t.Parallel()
	bin, err := exec.LookPath("zstd")
	if err != nil {
		t.Skip("zstd CLI not installed")
	}
	c := newTestCodec(t)

	want := compressible(testChunk*2 + 3)
	var stored bytes.Buffer
	if _, err := c.Compress(&stored, bytes.NewReader(want)); err != nil {
		t.Fatalf("Compress: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), bin, "-d", "--stdout") //nolint:gosec // G204: path from LookPath, fixed args
	cmd.Stdin = bytes.NewReader(stored.Bytes())
	got, err := cmd.Output()
	if err != nil {
		t.Fatalf("zstd -d failed on the stored object: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("zstd CLI decoded %d bytes, want %d", len(got), len(want))
	}
}

// TestNewCodec_RejectsChunkSizeOutOfRange asserts the bounds are enforced at
// construction rather than surfacing later as a malformed object.
func TestNewCodec_RejectsChunkSizeOutOfRange(t *testing.T) {
	t.Parallel()
	for _, size := range []int{0, -1, MinChunkSize - 1, MaxChunkSize + 1} {
		if _, err := NewCodec(DefaultLevel, size); !errors.Is(err, ErrChunkSizeRange) {
			t.Errorf("NewCodec(chunk=%d) error = %v, want ErrChunkSizeRange", size, err)
		}
	}
	c, err := NewCodec(DefaultLevel, DefaultChunkSize)
	if err != nil {
		t.Fatalf("NewCodec at the default: %v", err)
	}
	c.Close()
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// zstd frame magic numbers. Data frames carry the standard magic; skippable
// frames - which is where the seek table lives - use the 0x184D2A5* range and
// are not counted as data.
const (
	zstdFrameMagic         = 0xFD2FB528
	zstdSkippableMagicLow  = 0x184D2A50
	zstdSkippableMagicHigh = 0x184D2A5F
)

// countDataFrames walks a stored object and counts its data frames, skipping
// over skippable ones by reading their declared length.
func countDataFrames(t *testing.T, b []byte) int {
	t.Helper()
	frames := 0
	for off := 0; off+8 <= len(b); {
		magic := uint32(b[off]) | uint32(b[off+1])<<8 | uint32(b[off+2])<<16 | uint32(b[off+3])<<24
		size := uint32(b[off+4]) | uint32(b[off+5])<<8 | uint32(b[off+6])<<16 | uint32(b[off+7])<<24

		if magic >= zstdSkippableMagicLow && magic <= zstdSkippableMagicHigh {
			off += 8 + int(size)
			continue
		}
		if magic != zstdFrameMagic {
			t.Fatalf("unexpected frame magic %#x at offset %d", magic, off)
		}

		// A data frame does not declare its own compressed length, so its
		// extent comes from the seek table rather than the frame header.
		// Counting is enough here: decode the rest and stop.
		frames++
		n := dataFrameLen(t, b[off:])
		off += n
	}
	return frames
}

// dataFrameLen returns the byte length of the single zstd data frame at the
// front of b, found by decoding it.
func dataFrameLen(t *testing.T, b []byte) int {
	t.Helper()
	dec, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatalf("frame decoder: %v", err)
	}
	defer dec.Close()

	// Grow a window until exactly one frame decodes, which is the frame's end.
	for end := 1; end <= len(b); end++ {
		if _, err := dec.DecodeAll(b[:end], nil); err == nil {
			return end
		}
	}
	t.Fatalf("no complete frame found in %d bytes", len(b))
	return 0
}

// errReader fails every read with a fixed error.
type errReader struct{ err error }

// Read implements io.Reader.
func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// iotest_oneByteReader returns a reader that yields a single byte per call,
// standing in for a source that never fills the buffer it is given.
func iotest_oneByteReader(r io.Reader) io.Reader { //nolint:revive,staticcheck // ST1003: mirrors iotest.OneByteReader
	return &oneByteReader{r: r}
}

type oneByteReader struct{ r io.Reader }

// Read implements io.Reader.
func (o *oneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}

// TestCompress_PropagatesDestinationError asserts a destination that fails
// mid-object surfaces the error rather than reporting a short success. This is
// a backend dying partway through an upload.
func TestCompress_PropagatesDestinationError(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)

	sentinel := errors.New("backend refused the write")
	_, err := c.Compress(errWriter{sentinel}, bytes.NewReader(compressible(testChunk*2)))
	if !errors.Is(err, sentinel) {
		t.Errorf("Compress error = %v, want it to wrap the destination error", err)
	}
}

// TestDecompress_RejectsGarbage asserts bytes that are not a seekable object
// fail at open rather than producing a reader that yields nonsense.
func TestDecompress_RejectsGarbage(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)

	if _, err := c.Decompress(bytes.NewReader([]byte("this is not a zstd stream"))); err == nil {
		t.Error("Decompress accepted garbage")
	}
}

// TestChunkSize_ReportsConfigured pins the accessor the write path uses to size
// its reads against what the codec was built with.
func TestChunkSize_ReportsConfigured(t *testing.T) {
	t.Parallel()
	if got := newTestCodec(t).ChunkSize(); got != testChunk {
		t.Errorf("ChunkSize() = %d, want %d", got, testChunk)
	}
}

// errWriter fails every write with a fixed error.
type errWriter struct{ err error }

// Write implements io.Writer.
func (w errWriter) Write([]byte) (int, error) { return 0, w.err }

// TestNewCodecForLevel_AcceptsEveryConfiguredName pins the mapping between the
// names the config exposes and the levels zstd actually implements. A name the
// encoder does not know would otherwise fall back to a default silently.
func TestNewCodecForLevel_AcceptsEveryConfiguredName(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"fastest", "default", "better", "best"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, err := NewCodecForLevel(name, testChunk)
			if err != nil {
				t.Fatalf("NewCodecForLevel(%q): %v", name, err)
			}
			defer c.Close()
			if got := roundTrip(t, c, compressible(testChunk+7)); !bytes.Equal(got, compressible(testChunk+7)) {
				t.Errorf("level %q did not round trip", name)
			}
		})
	}
}

// TestNewCodecForLevel_RejectsUnknownName covers the branch that keeps a
// mistyped level from silently becoming the default.
func TestNewCodecForLevel_RejectsUnknownName(t *testing.T) {
	t.Parallel()
	if _, err := NewCodecForLevel("turbo", testChunk); !errors.Is(err, ErrUnknownLevel) {
		t.Errorf("NewCodecForLevel(\"turbo\") err = %v, want ErrUnknownLevel", err)
	}
}

// TestNewCodecForLevel_RejectsChunkSizeOutOfRange checks the name form applies
// the same bounds the numeric form does.
func TestNewCodecForLevel_RejectsChunkSizeOutOfRange(t *testing.T) {
	t.Parallel()
	if _, err := NewCodecForLevel("default", MinChunkSize-1); !errors.Is(err, ErrChunkSizeRange) {
		t.Errorf("err = %v, want ErrChunkSizeRange", err)
	}
}
