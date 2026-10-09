// -------------------------------------------------------------------------------
// Ranged Decompression - Frame Reads over a Byte-Range Source
//
// Author: Alex Freidah
//
// Every decode reads frames through here. The seekable library exports
// ReaderEnvironment, which states each read as explicit byte bounds, so this
// file adapts it to a RangeFetcher: a backend serves ranges directly, and a
// local ReadSeeker is wrapped as one. Either way each frame the seek table names
// is checked against the stream's real length before it is read, which the
// library's own ReadSeeker environment does not do - it allocates whatever size
// the table claims.
// -------------------------------------------------------------------------------

package compression

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	seekable "github.com/SaveTheRbtz/zstd-seekable-format-go/pkg"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// tailPrefetchSize is how much of an object's tail one speculative fetch pulls.
// Reading the seek table is a footer read followed by a read of the whole table,
// so a fetch sized to hold both turns two round trips into one. At 12 bytes per
// entry this covers a 680 MB object at the default chunk size; a larger table
// costs one extra fetch. It is a fixed cost on every read, hence this small.
const tailPrefetchSize = 8 << 10

// Failure modes a ranged read distinguishes, so a caller can tell wrong stored
// bytes (try another copy and scrub) from a failed delivery (retry).
// ErrRangeBounds reports caller metadata the object cannot match,
// ErrShortRange a fetch that returned the wrong bytes, and ErrFetchFailed a
// fetch that did not answer.
var (
	ErrRangeBounds = errors.New("range outside object")
	ErrShortRange  = errors.New("range fetch returned wrong length")
	ErrFetchFailed = errors.New("range fetch failed")
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// RangeFetcher supplies arbitrary byte ranges of one stored compressed object.
// FetchRange returns exactly the bytes at [start, end] inclusive, and may be
// called concurrently. It is declared here because compress-before-encrypt
// means this package cannot import the backend or encryption packages.
type RangeFetcher interface {
	FetchRange(ctx context.Context, start, end int64) ([]byte, error)
}

// RangedReader is random access over an object's logical bytes, so a caller
// seeks to the range a client asked for without knowing where the frames fell.
type RangedReader interface {
	io.ReadSeekCloser
	io.ReaderAt
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// DecompressRanged returns a reader over the logical bytes of a stored object,
// pulling only the frames a read touches through f.
//
// compressedSize is the size of the compressed stream (the pre-encryption size
// for an encrypted object), needed to locate the trailing seek table. The
// reader caches one decoded frame.
func (c *Codec) DecompressRanged(ctx context.Context, f RangeFetcher, compressedSize int64) (RangedReader, error) {
	if compressedSize <= 0 {
		return nil, fmt.Errorf("%w: compressed size %d", ErrRangeBounds, compressedSize)
	}
	env := &rangeEnv{
		fetch: func(start, end int64) ([]byte, error) { return f.FetchRange(ctx, start, end) },
		size:  compressedSize,
	}
	r, err := seekable.NewReader(nil, c.dec,
		seekable.WithReaderEnvironment(env),
		seekable.WithReaderLogger(c.log))
	if err != nil {
		return nil, classifyDecode(fmt.Errorf("open ranged seekable reader: %w", err))
	}
	return &decodeGuard{inner: r}, nil
}

// InspectStored reports whether stored bytes are in the seekable format this
// codec writes and, if so, the logical size their seek table declares. The
// zstd frame magic alone cannot tell this codec's output from a client's .zst
// upload; the trailing seek table can, since plain zstd never writes one.
func (c *Codec) InspectStored(ctx context.Context, f RangeFetcher, storedSize int64) (int64, bool) {
	r, err := c.DecompressRanged(ctx, f, storedSize)
	if err != nil {
		return 0, false
	}
	defer func() { _ = r.Close() }()

	logicalSize, err := r.Seek(0, io.SeekEnd)
	if err != nil || logicalSize <= 0 {
		return 0, false
	}
	return logicalSize, true
}

// seekerFetcher serves byte ranges out of a local ReadSeeker, so Decompress
// shares the bounded frame reads of the ranged path. The lock keeps a seek and
// its read together when ranges are fetched concurrently.
type seekerFetcher struct {
	mu sync.Mutex
	rs io.ReadSeeker
}

// FetchRange reads the bytes at [start, end] inclusive.
func (f *seekerFetcher) FetchRange(_ context.Context, start, end int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.rs.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, end-start+1)
	if _, err := io.ReadFull(f.rs, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// rangeEnv adapts a RangeFetcher to seekable.ReaderEnvironment. fetch has the
// request context bound, since ReaderEnvironment's methods take none.
//
// tail holds the last bytes of the stream, filled by ReadFooter. The library
// reads the seek table only while building a Reader, before any concurrent
// frame read, so the frame path reads tail without a lock.
type rangeEnv struct {
	fetch func(start, end int64) ([]byte, error)
	size  int64
	tail  []byte
}

// ReadFooter returns a tail of the stream whose last nine bytes are the seek
// table footer. Returning more is deliberate: the parser reads the footer off
// the end of whatever it is handed, so this fetch also covers the table frame
// ReadSkipFrame asks for next.
func (e *rangeEnv) ReadFooter() ([]byte, error) {
	n := min(int64(tailPrefetchSize), e.size)
	buf, err := e.fetchAt(e.size-n, n)
	if err != nil {
		return nil, err
	}
	e.tail = buf
	return buf, nil
}

// ReadSkipFrame returns the last skippableFrameOffset bytes of the stream. The
// length has to be exact here - the parser checks the frame's declared size
// against it - so a table larger than the prefetch costs a second fetch.
func (e *rangeEnv) ReadSkipFrame(skippableFrameOffset int64) ([]byte, error) {
	if n := int64(len(e.tail)); skippableFrameOffset > 0 && skippableFrameOffset <= n {
		return e.tail[n-skippableFrameOffset:], nil
	}
	return e.fetchAt(e.size-skippableFrameOffset, skippableFrameOffset)
}

// GetFrameByIndex returns one complete compressed frame. The prefetched tail
// usually holds the last frame of a small object, so it is checked first.
func (e *rangeEnv) GetFrameByIndex(index seekable.FrameOffsetEntry) ([]byte, error) {
	size := uint64(e.size) //nolint:gosec // G115: size is positive, checked in DecompressRanged
	if index.CompressedOffset > size || uint64(index.CompressedSize) > size-index.CompressedOffset {
		return nil, fmt.Errorf("%w: frame %d of %d bytes at %d in a %d byte object",
			ErrRangeBounds, index.ID, index.CompressedSize, index.CompressedOffset, e.size)
	}
	//nolint:gosec // G115: both bounded by size, checked above
	off, n := int64(index.CompressedOffset), int64(index.CompressedSize)
	if buf, ok := e.fromTail(off, n); ok {
		return buf, nil
	}
	return e.fetchAt(off, n)
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// fetchAt pulls exactly n bytes at off. A wrong-length answer is rejected here
// so it surfaces as a transport fault, not frame corruption. An out-of-bounds
// request means the seek table disagrees with the caller's size and is
// reported as corruption.
func (e *rangeEnv) fetchAt(off, n int64) ([]byte, error) {
	if off < 0 || n <= 0 || off > e.size-n {
		return nil, fmt.Errorf("%w: seek table asks for %d bytes at %d in a %d byte object",
			ErrCorruptObject, n, off, e.size)
	}
	buf, err := e.fetch(off, off+n-1)
	if err != nil {
		return nil, fmt.Errorf("%w: %d bytes at %d: %w", ErrFetchFailed, n, off, err)
	}
	if int64(len(buf)) != n {
		return nil, fmt.Errorf("%w: asked %d bytes at %d, got %d", ErrShortRange, n, off, len(buf))
	}
	return buf, nil
}

// classifyDecode names the failure behind an error leaving the seekable reader.
// Anything the codec did not raise itself came from parsing or decoding the
// stored bytes, so corruption is the answer by elimination - the library fails
// only on bytes it cannot make sense of.
func classifyDecode(err error) error {
	switch {
	case err == nil, errors.Is(err, io.EOF):
		return err
	case errors.Is(err, ErrCorruptObject), errors.Is(err, ErrShortRange),
		errors.Is(err, ErrFetchFailed), errors.Is(err, ErrRangeBounds):
		return err
	default:
		return fmt.Errorf("%w: %w", ErrCorruptObject, err)
	}
}

// decodeGuard classifies the errors the seekable reader raises while decoding,
// so a decode failure reaches the read path as ErrCorruptObject rather than as
// whatever shape the library happened to produce.
type decodeGuard struct {
	inner RangedReader
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Read implements io.Reader.
func (g *decodeGuard) Read(p []byte) (int, error) {
	n, err := g.inner.Read(p)
	return n, classifyDecode(err)
}

// ReadAt implements io.ReaderAt.
func (g *decodeGuard) ReadAt(p []byte, off int64) (int, error) {
	n, err := g.inner.ReadAt(p, off)
	return n, classifyDecode(err)
}

// Seek implements io.Seeker. Its errors pass through unclassified: seeking
// moves an offset against the parsed seek table and never decodes a frame, so a
// failure here is a bad argument rather than a bad object.
func (g *decodeGuard) Seek(offset int64, whence int) (int64, error) {
	return g.inner.Seek(offset, whence)
}

// Close implements io.Closer.
func (g *decodeGuard) Close() error { return g.inner.Close() }

// fromTail serves n bytes at off out of the prefetched tail when it covers
// them. The caller has already bounded off and n against the object size.
func (e *rangeEnv) fromTail(off, n int64) ([]byte, bool) {
	start := e.size - int64(len(e.tail))
	if len(e.tail) == 0 || off < start {
		return nil, false
	}
	return e.tail[off-start : off-start+n], true
}
