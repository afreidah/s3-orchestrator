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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	seekable "github.com/SaveTheRbtz/zstd-seekable-format-go/pkg"
	"github.com/klauspost/compress/zstd"

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
)

// Chunk size bounds and the default. The default comes from measuring the ratio
// cost of splitting an object into independently decodable frames: at 1 MiB it
// is 2.5% on Go source and negative on JSON logs, with throughput unchanged,
// while 64 KiB costs 17%. The lower bound keeps the seek table from dwarfing
// small objects; the upper bound keeps a single-chunk read from pulling an
// unreasonable amount of a backend object for a small range.
const (
	DefaultChunkSize = 1 << 20 // 1 MiB
	MinChunkSize     = 1 << 14 // 16 KiB
	MaxChunkSize     = 1 << 26 // 64 MiB
)

// Algorithm and FormatVersion are what a stored object records about how it was
// encoded. Algorithm is the value a metadata row carries in
// compression_algorithm, and an empty one there means the bytes are verbatim.
// FormatVersion moves only if the on-disk layout changes in a way a reader has
// to branch on; the chunk size does not, since each object carries its own in
// its seek table.
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
// Safe for concurrent use: the encoder and decoder it holds are, and it carries
// no per-stream state.
type Codec struct {
	enc       *zstd.Encoder
	dec       *zstd.Decoder
	chunkSize int
	log       *slog.Logger

	// bufPool holds chunk-sized staging buffers. Without it every upload
	// allocates chunkSize bytes it uses once and drops, which at the 1 MiB
	// default is the largest single allocation on the write path. Mirrors
	// Encryptor.bufPool.
	bufPool sync.Pool
}

// NewCodec builds a codec at the given zstd level and chunk size.
//
// Note that the level is coarser than it looks: zstd collapses the numeric
// range into four buckets (below 3, 3 to 5, 6 to 9, 10 and above), so levels 10
// and 19 produce byte-identical output.
//
// The chunk size is fixed for the lifetime of the data it writes. Changing it
// affects new objects only; existing ones carry their own layout in their seek
// table and stay readable.
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

// WorthStoring reports whether an encoded object shrank enough to be stored in
// place of the original. Both write paths ask this, so the rule lives with the
// codec rather than with either of them.
//
// The decision is made on the finished encoding rather than a sample of it:
// entropy is not uniform across an object, and a sample is wrong in the
// direction that costs bytes for the life of the object, while encoding an
// object that turns out to be incompressible costs only the encode itself -
// the cheapest case the encoder has, since it detects unshrinkable blocks and
// stores them raw.
//
// A logical size of zero cannot shrink, and the ratio is meaningless there.
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

// Compress encodes src into dst and reports the physical bytes written.
//
// The chunk boundary is enforced here: the seekable writer emits one frame per
// Write, so a full chunk is accumulated before each call. A short read from src
// is not treated as end of input - only io.EOF is - because an io.Reader may
// legally return fewer bytes than asked for at any point, and taking that as
// the end would silently truncate the object into undersized frames.
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

// Decompress returns a reader over the logical bytes of a stored object.
//
// The source is an io.ReadSeeker because the seek table sits at the end of the
// stream, so the reader seeks to find it before serving anything. Reading a
// whole object still walks the frames in order; ranged access over a backend is
// a matter of what ReadSeeker gets passed in.
func (c *Codec) Decompress(rs io.ReadSeeker) (io.ReadCloser, error) {
	r, err := seekable.NewReader(rs, c.dec, seekable.WithReaderLogger(c.log))
	if err != nil {
		return nil, classifyDecode(fmt.Errorf("open seekable reader: %w", err))
	}
	return &decodeGuard{inner: r}, nil
}

// DecompressStream returns a reader over the logical bytes of a stored object
// read front to back, for a caller that has a stream rather than something it
// can seek.
//
// The seek table is not consulted: a stored object is a plain sequence of zstd
// frames followed by a skippable one, so decoding it in order needs no index.
// That makes this the right shape for whole-object work like scrubbing, which
// would otherwise have to buffer an entire object locally just to hand
// Decompress something seekable.
//
// Unlike the rest of the Codec this allocates, since a streaming decoder holds
// per-stream state and the shared one cannot be rebound while other callers are
// using it. Whole-object reads are rare and already dominated by the network, so
// the allocation is not worth pooling around.
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
