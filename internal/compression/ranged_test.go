// -------------------------------------------------------------------------------
// Ranged Decompression Tests
//
// Author: Alex Freidah
//
// The property that justifies this whole path is that reading a small range
// costs a small number of bytes off the backend, so the assertions are on bytes
// fetched rather than on wall time. The rest covers what a backend can do to a
// ranged read that a local file cannot: answer short, answer long, or fail.
// -------------------------------------------------------------------------------

package compression

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"sync"
	"testing"
)

// countingFetcher serves ranges out of an in-memory object and tallies what was
// asked of it, which is the unit the proportionality assertions are stated in.
// short, long and err perturb the answer, standing in for a backend that
// truncates, over-delivers, or fails mid-read.
type countingFetcher struct {
	data []byte

	mu     sync.Mutex
	bytes  int64
	calls  int
	ranges [][2]int64

	short, long bool
	err         error
}

// FetchRange implements RangeFetcher.
func (f *countingFetcher) FetchRange(_ context.Context, start, end int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if start < 0 || end < start || end >= int64(len(f.data)) {
		return nil, fmt.Errorf("fetcher asked for %d-%d of %d bytes", start, end, len(f.data))
	}
	f.calls++
	f.bytes += end - start + 1
	f.ranges = append(f.ranges, [2]int64{start, end})

	out := bytes.Clone(f.data[start : end+1])
	switch {
	case f.short && len(out) > 1:
		out = out[:len(out)-1]
	case f.long:
		out = append(out, 0)
	}
	return out, nil
}

// stats returns the tally without racing the reader.
func (f *countingFetcher) stats() (calls int, fetched int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.bytes
}

// storeCompressed compresses src and returns the stored bytes with a fetcher
// over them.
func storeCompressed(t *testing.T, c *Codec, src []byte) *countingFetcher {
	t.Helper()
	var stored bytes.Buffer
	if _, err := c.Compress(&stored, bytes.NewReader(src)); err != nil {
		t.Fatalf("Compress: %v", err)
	}
	return &countingFetcher{data: stored.Bytes()}
}

// incompressible returns n bytes zstd cannot shrink, so the stored object is
// large enough that the tail prefetch has not already covered all of it.
func incompressible(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	rnd := rand.New(rand.NewSource(99)) //nolint:gosec // G404: test fixture, not security material
	if _, err := rnd.Read(b); err != nil {
		t.Fatalf("seed random: %v", err)
	}
	return b
}

// openRanged opens a ranged reader over f and closes it with the test.
func openRanged(t *testing.T, c *Codec, f *countingFetcher) RangedReader {
	t.Helper()
	r, err := c.DecompressRanged(context.Background(), f, int64(len(f.data)))
	if err != nil {
		t.Fatalf("DecompressRanged: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// TestDecompressRanged_RoundTrip verifies that a whole-object read through the
// ranged path returns the same bytes the ReadSeeker path does.
func TestDecompressRanged_RoundTrip(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)
	src := compressible(testChunk*4 + 111)
	r := openRanged(t, c, storeCompressed(t, c, src))

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, src) {
		t.Errorf("round trip returned %d bytes, want %d", len(got), len(src))
	}
}

// TestDecompressRanged_FetchesOnlyTouchedFrames is the acceptance criterion: a
// range confined to one chunk must not pull the whole object. The budget is the
// tail prefetch plus the two frames a read can straddle.
func TestDecompressRanged_FetchesOnlyTouchedFrames(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)
	src := incompressible(t, testChunk*16)
	f := storeCompressed(t, c, src)
	r := openRanged(t, c, f)

	const readLen = 1024
	off := int64(testChunk * 9)
	buf := make([]byte, readLen)
	if _, err := r.ReadAt(buf, off); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(buf, src[off:off+readLen]) {
		t.Error("ReadAt returned the wrong bytes")
	}

	_, fetched := f.stats()
	budget := int64(tailPrefetchSize + 2*testChunk)
	if fetched > budget {
		t.Errorf("fetched %d bytes for a %d byte range of a %d byte object, budget %d",
			fetched, readLen, len(f.data), budget)
	}
}

// TestDecompressRanged_ReadsSeekTableInOneFetch checks the tail prefetch: the
// footer read and the seek table read are the same bytes, so they must not cost
// two round trips.
func TestDecompressRanged_ReadsSeekTableInOneFetch(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)
	f := storeCompressed(t, c, compressible(testChunk*4))

	r, err := c.DecompressRanged(context.Background(), f, int64(len(f.data)))
	if err != nil {
		t.Fatalf("DecompressRanged: %v", err)
	}
	defer func() { _ = r.Close() }()

	if calls, _ := f.stats(); calls != 1 {
		t.Errorf("opening the reader took %d fetches, want 1", calls)
	}
}

// TestDecompressRanged_RandomRanges compares random ranges against the source.
func TestDecompressRanged_RandomRanges(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)
	src := compressible(testChunk*5 + 777)
	r := openRanged(t, c, storeCompressed(t, c, src))

	rnd := rand.New(rand.NewSource(1249)) //nolint:gosec // G404: deterministic test input, not a secret
	for range 200 {
		off := rnd.Int63n(int64(len(src)))
		n := rnd.Int63n(int64(len(src))-off) + 1
		buf := make([]byte, n)
		if _, err := r.ReadAt(buf, off); err != nil {
			t.Fatalf("ReadAt(%d, %d): %v", n, off, err)
		}
		if !bytes.Equal(buf, src[off:off+n]) {
			t.Fatalf("ReadAt(%d, %d) returned the wrong bytes", n, off)
		}
	}
}

// TestDecompressRanged_SeekThenRead covers the reader's stateful path, which is
// how a caller serves a client range: seek to the start, read the length.
func TestDecompressRanged_SeekThenRead(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)
	src := compressible(testChunk*3 + 42)
	r := openRanged(t, c, storeCompressed(t, c, src))

	off := int64(testChunk + 100)
	if _, err := r.Seek(off, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got, err := io.ReadAll(io.LimitReader(r, 2048))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, src[off:off+2048]) {
		t.Error("seek then read returned the wrong bytes")
	}
}

// TestDecompressRanged_RejectsWrongLengthFetch covers a backend answering with
// the wrong number of bytes, which must surface as a transport fault rather
// than reach the parser and read as data corruption.
func TestDecompressRanged_RejectsWrongLengthFetch(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)
	src := compressible(testChunk * 2)

	for _, tc := range []struct {
		name        string
		short, long bool
	}{
		{name: "short", short: true},
		{name: "long", long: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := storeCompressed(t, c, src)
			f.short, f.long = tc.short, tc.long
			_, err := c.DecompressRanged(context.Background(), f, int64(len(f.data)))
			if !errors.Is(err, ErrShortRange) {
				t.Errorf("DecompressRanged err = %v, want ErrShortRange", err)
			}
		})
	}
}

// TestDecompressRanged_PropagatesFetchError checks that a backend failure
// reaches the caller instead of being reported as a malformed object.
func TestDecompressRanged_PropagatesFetchError(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)
	sentinel := errors.New("backend unreachable")
	f := storeCompressed(t, c, compressible(testChunk*2))
	f.err = sentinel

	if _, err := c.DecompressRanged(context.Background(), f, int64(len(f.data))); !errors.Is(err, sentinel) {
		t.Errorf("DecompressRanged err = %v, want %v", err, sentinel)
	}
}

// TestDecompressRanged_FetchErrorMidRead covers a backend that fails after the
// seek table is already parsed, which is the case a fuzz target reaches.
func TestDecompressRanged_FetchErrorMidRead(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)
	sentinel := errors.New("backend reset")
	f := storeCompressed(t, c, incompressible(t, testChunk*16))
	r := openRanged(t, c, f)

	f.mu.Lock()
	f.err = sentinel
	f.mu.Unlock()

	buf := make([]byte, 64)
	if _, err := r.ReadAt(buf, testChunk*2); !errors.Is(err, sentinel) {
		t.Errorf("ReadAt err = %v, want %v", err, sentinel)
	}
}

// TestDecompressRanged_RejectsNonPositiveSize covers a metadata row claiming a
// stored object no seek table could fit in.
func TestDecompressRanged_RejectsNonPositiveSize(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)
	f := storeCompressed(t, c, compressible(testChunk))

	for _, size := range []int64{0, -1} {
		if _, err := c.DecompressRanged(context.Background(), f, size); !errors.Is(err, ErrRangeBounds) {
			t.Errorf("DecompressRanged(size=%d) err = %v, want ErrRangeBounds", size, err)
		}
	}
}

// TestDecompressRanged_ConcurrentReadAt exercises the concurrency the library
// contract allows, since a fetcher backed by HTTP will actually see it.
func TestDecompressRanged_ConcurrentReadAt(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)
	src := compressible(testChunk * 8)
	r := openRanged(t, c, storeCompressed(t, c, src))

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			off := int64(i * testChunk)
			buf := make([]byte, 512)
			if _, err := r.ReadAt(buf, off); err != nil {
				t.Errorf("ReadAt(%d): %v", off, err)
				return
			}
			if !bytes.Equal(buf, src[off:off+512]) {
				t.Errorf("ReadAt(%d) returned the wrong bytes", off)
			}
		})
	}
	wg.Wait()
}

// TestDecompressRanged_SmallObjectServedFromTail checks that an object whose
// frames sit inside the prefetched tail is never fetched a second time.
func TestDecompressRanged_SmallObjectServedFromTail(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)
	src := compressible(testChunk)
	f := storeCompressed(t, c, src)
	if len(f.data) >= tailPrefetchSize {
		t.Skipf("stored object is %d bytes, too large for this test's premise", len(f.data))
	}
	r := openRanged(t, c, f)

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, src) {
		t.Error("read returned the wrong bytes")
	}
	if calls, _ := f.stats(); calls != 1 {
		t.Errorf("reading a wholly prefetched object took %d fetches, want 1", calls)
	}
}
