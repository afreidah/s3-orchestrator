// -------------------------------------------------------------------------------
// Chunked Zstandard Codec
//
// Author: Alex Freidah
//
// Codec encodes an object as one zstd frame per fixed-size logical chunk and
// decodes it back. The frame layout and trailing seek table are handled by the
// seekable library; what lives here is the chunk boundary, which that library
// does not own - it emits one frame per Write, so Compress batches the source
// into chunk-sized writes.
//
// One encoder and one decoder are built per Codec and shared across every
// request: klauspost documents EncodeAll and DecodeAll as safe for concurrent
// use, so neither needs pooling and no upload allocates a codec. The staging
// buffer does get pooled, being chunk-sized and otherwise discarded per object.
// -------------------------------------------------------------------------------

package compression

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	seekable "github.com/SaveTheRbtz/zstd-seekable-format-go/pkg"
	"github.com/klauspost/compress/zstd"

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// Chunk size bounds and the default. Splitting into independently decodable
// frames costs 2.5% ratio at 1 MiB against 17% at 64 KiB. The lower bound keeps
// the seek table from dwarfing small objects; the upper bound limits how much a
// small ranged read pulls from the backend.
const (
	DefaultChunkSize = 1 << 20 // 1 MiB
	MinChunkSize     = 1 << 14 // 16 KiB
	MaxChunkSize     = 1 << 26 // 64 MiB
)

// Algorithm and FormatVersion record how a stored object was encoded.
// Algorithm is the compression_algorithm metadata value; an empty one means the
// bytes are verbatim. FormatVersion changes only when readers must branch on
// the on-disk layout; chunk size is per object, in its seek table.
const (
	Algorithm     = "zstd"
	FormatVersion = 1
)

// DefaultLevel is zstd level 3. Measured against levels 1, 7 and 11, it sits at
// the point where compression speed has not yet collapsed (177 MB/s against
// 22 MB/s at level 11) for a ratio within 3% of the best. Decompression speed
// does not degrade with level, so the trade is entirely on the write side.
const DefaultLevel = 3

// decoderMaxMemory caps what a single decode may allocate, so a corrupt or
// hostile frame declaring an enormous window cannot exhaust the process.
const decoderMaxMemory = 64 << 20 // 64 MiB

// ErrChunkSizeRange reports a chunk size outside the supported bounds.
// ErrUnknownLevel reports a level name zstd does not recognize.
var (
	ErrChunkSizeRange = errors.New("chunk size out of range")
	ErrUnknownLevel   = errors.New("unknown compression level")
)

// ErrCorruptObject reports stored bytes the codec could not decode: a seek table
// that will not parse, one describing frames the object does not contain, or a
// frame zstd rejects. It is the codec's answer to "these bytes are not what the
// metadata says they are", and it is never the result of a read that merely
// failed to arrive.
var ErrCorruptObject = errors.New("corrupt compressed object")

// Codec compresses and decompresses objects in the chunked seekable format.
// Safe for concurrent use. bufPool reuses the chunk-sized staging buffer, the
// largest single allocation on the write path.
type Codec struct {
	enc       *zstd.Encoder
	dec       *zstd.Decoder
	chunkSize int
	log       *slog.Logger

	bufPool sync.Pool // chunk-sized staging buffers
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// NewCodec builds a codec at the given zstd level and chunk size. zstd
// collapses levels into four buckets (below 3, 3 to 5, 6 to 9, 10 and above).
// A chunk size change affects new objects only.
func NewCodec(level, chunkSize int) (*Codec, error) {
	return newCodec(zstd.EncoderLevelFromZstd(level), chunkSize)
}

// NewCodecForLevel builds a codec from the level name the config exposes.
// Naming the levels is the only honest way to offer the choice: zstd collapses
// its numeric range into four buckets, so a config taking 1 to 19 would present
// fifteen settings that change nothing.
func NewCodecForLevel(name string, chunkSize int) (*Codec, error) {
	ok, level := zstd.EncoderLevelFromString(name)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownLevel, name)
	}
	return newCodec(level, chunkSize)
}

// newCodec is the shared constructor body behind both level forms.
func newCodec(level zstd.EncoderLevel, chunkSize int) (*Codec, error) {
	if chunkSize < MinChunkSize || chunkSize > MaxChunkSize {
		return nil, fmt.Errorf("%w: %d not in [%d, %d]",
			ErrChunkSizeRange, chunkSize, MinChunkSize, MaxChunkSize)
	}

	// Concurrency 1: a server encoding many objects at once should spend its
	// cores on requests rather than fanning one object across all of them.
	enc, err := zstd.NewWriter(
		nil,
		zstd.WithEncoderLevel(level),
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		return nil, fmt.Errorf("build zstd encoder: %w", err)
	}

	dec, err := zstd.NewReader(
		nil,
		zstd.WithDecoderMaxMemory(decoderMaxMemory),
		zstd.WithDecoderConcurrency(1),
	)
	if err != nil {
		enc.Close()
		return nil, fmt.Errorf("build zstd decoder: %w", err)
	}

	c := &Codec{
		enc:       enc,
		dec:       dec,
		chunkSize: chunkSize,
		log:       slog.Default().With(logfmt.Component("compression")),
	}
	c.bufPool.New = func() any {
		b := make([]byte, chunkSize)
		return &b
	}
	return c, nil
}

// frameMagic is the four bytes every Zstandard data frame starts with.
var frameMagic = []byte{0x28, 0xB5, 0x2F, 0xFD}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// HasFrameMagic reports whether bytes begin a Zstandard frame. A client's .zst
// file matches too, so this only filters out plaintext before InspectStored
// fetches the tail.
func HasFrameMagic(b []byte) bool {
	return len(b) >= len(frameMagic) && bytes.Equal(b[:len(frameMagic)], frameMagic)
}

// WorthStoring reports whether an encoded object shrank enough to be stored in
// place of the original. It judges the finished encoding, not a sample, because
// entropy varies across an object and a wrong sample costs bytes for its life.
func WorthStoring(logicalSize, encodedSize int64, minRatio float64) bool {
	if logicalSize <= 0 {
		return false
	}
	return float64(encodedSize) <= float64(logicalSize)*minRatio
}

// ChunkSize reports the logical chunk size new objects are written at.
func (c *Codec) ChunkSize() int { return c.chunkSize }

// Close releases the encoder and decoder. A Codec is process-lifetime, so this
// exists for tests and for a clean shutdown rather than per-request use.
func (c *Codec) Close() {
	c.enc.Close()
	c.dec.Close()
}

// Compress encodes src into dst and reports the physical bytes written. The
// seekable writer emits one frame per Write, so a full chunk is accumulated
// before each call; only io.EOF ends the input, never a short read.
func (c *Codec) Compress(dst io.Writer, src io.Reader) (int64, error) {
	counter := &countingWriter{w: dst}
	w, err := seekable.NewWriter(counter, c.enc, seekable.WithWriterLogger(c.log))
	if err != nil {
		return 0, fmt.Errorf("open seekable writer: %w", err)
	}

	bufp := c.bufPool.Get().(*[]byte)
	defer c.bufPool.Put(bufp)
	buf := *bufp

	for {
		n, readErr := io.ReadFull(src, buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				_ = w.Close()
				return counter.n, fmt.Errorf("write chunk: %w", err)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
				break
			}
			_ = w.Close()
			return counter.n, fmt.Errorf("read source: %w", readErr)
		}
	}

	// Close writes the seek table, so its error is not incidental: without the
	// table the object is still valid zstd but no longer seekable.
	if err := w.Close(); err != nil {
		return counter.n, fmt.Errorf("finalize seek table: %w", err)
	}
	return counter.n, nil
}

// Decompress returns a reader over the logical bytes of a stored object. It
// takes a ReadSeeker because the seek table sits at the end of the stream.
func (c *Codec) Decompress(rs io.ReadSeeker) (io.ReadCloser, error) {
	r, err := seekable.NewReader(rs, c.dec, seekable.WithReaderLogger(c.log))
	if err != nil {
		return nil, classifyDecode(fmt.Errorf("open seekable reader: %w", err))
	}
	return &decodeGuard{inner: r}, nil
}

// DecompressStream decodes a stored object front to back without the seek
// table, for whole-object work like scrubbing that only has a stream. Unlike
// the rest of the Codec it allocates a decoder per call.
func (c *Codec) DecompressStream(r io.Reader) (io.ReadCloser, error) {
	dec, err := zstd.NewReader(r,
		zstd.WithDecoderMaxMemory(decoderMaxMemory),
		zstd.WithDecoderConcurrency(1),
	)
	if err != nil {
		return nil, classifyDecode(fmt.Errorf("open zstd reader: %w", err))
	}
	return &streamGuard{inner: dec.IOReadCloser()}, nil
}

// streamGuard classifies decode errors from a front-to-back read the way
// decodeGuard does for a seekable one. Separate because a stream has no ReadAt
// or Seek to carry.
type streamGuard struct {
	inner io.ReadCloser
}

// Read implements io.Reader.
func (g *streamGuard) Read(p []byte) (int, error) {
	n, err := g.inner.Read(p)
	return n, classifyDecode(err)
}

// Close implements io.Closer.
func (g *streamGuard) Close() error { return g.inner.Close() }

// countingWriter totals the bytes written through it, which is the object's
// physical size: what the backend stores and what quota is charged for.
type countingWriter struct {
	w io.Writer
	n int64
}

// Write implements io.Writer.
func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
