// -------------------------------------------------------------------------------
// Compression Fuzz Tests - Decode Entry Points and the Ranged Source
//
// Author: Alex Freidah
//
// The seekable format's own parser is fuzzed upstream with a committed corpus.
// What is fuzzed here is our side of that boundary: the two functions the read
// path calls to decode a stored object, and the ranged source feeding them.
//
// The property under test is that stored bytes the codec cannot make sense of
// produce an error rather than a panic or a short read. A backend can return
// fewer bytes than asked for, more, or the wrong ones entirely, and none of
// those may reach a client as a successful partial answer.
// -------------------------------------------------------------------------------

package compression

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

// fuzzMaxSource bounds the fixture a target builds per iteration. Fuzzing spends
// its budget on the decode paths, not on compressing large inputs.
const fuzzMaxSource = 1 << 16

// misbehavingFetcher serves ranges of a stored object after mangling the answer,
// standing in for a backend that truncates, over-delivers, or corrupts a range.
// mode selects which, and is taken from the fuzzer.
type misbehavingFetcher struct {
	data []byte
	mode int
	at   int
	n    int
}

// FetchRange implements RangeFetcher.
func (m *misbehavingFetcher) FetchRange(_ context.Context, start, end int64) ([]byte, error) {
	if start < 0 || end < start || end >= int64(len(m.data)) {
		return nil, errors.New("fetcher asked for a range outside the object")
	}
	m.n++
	out := bytes.Clone(m.data[start : end+1])

	// Only mangle one fetch, chosen by the fuzzer, so the failure has to be
	// caught on the strength of that single bad answer.
	if m.n != m.at {
		return out, nil
	}
	switch m.mode % 4 {
	case 0:
		if len(out) > 0 {
			out = out[:len(out)-1]
		}
	case 1:
		out = append(out, 0)
	case 2:
		for i := range out {
			out[i] ^= 0xFF
		}
	case 3:
		return nil, errors.New("backend unreachable")
	}
	return out, nil
}

// fuzzStore compresses src and returns the stored bytes.
func fuzzStore(t *testing.T, c *Codec, src []byte) []byte {
	t.Helper()
	var stored bytes.Buffer
	if _, err := c.Compress(&stored, bytes.NewReader(src)); err != nil {
		t.Fatalf("Compress: %v", err)
	}
	return stored.Bytes()
}

// wellClassified reports whether err names one of the codec's failure modes.
// An error escaping the codec unclassified is the bug this guards against: the
// read path has to tell corrupt bytes from a backend that did not deliver.
func wellClassified(err error) bool {
	return errors.Is(err, ErrCorruptObject) ||
		errors.Is(err, ErrShortRange) ||
		errors.Is(err, ErrFetchFailed) ||
		errors.Is(err, ErrRangeBounds)
}

// FuzzDecompressRanged feeds arbitrary bytes to the ranged decode entry point as
// though they were a stored object, which is what a corrupted backend copy looks
// like from here. Nothing may panic, and every failure must be classified.
func FuzzDecompressRanged(f *testing.F) {
	c := newTestCodec(f)

	var valid bytes.Buffer
	if _, err := c.Compress(&valid, bytes.NewReader(compressible(4096))); err != nil {
		f.Fatalf("Compress: %v", err)
	}
	f.Add(valid.Bytes(), int64(0), 64)
	f.Add(valid.Bytes()[:valid.Len()/2], int64(0), 64) // truncated mid-object
	f.Add([]byte{}, int64(0), 1)
	f.Add([]byte{0x00}, int64(0), 1)
	f.Add(make([]byte, 9), int64(0), 1) // footer-sized garbage
	f.Add([]byte("not a zstd stream at all"), int64(5), 32)

	f.Fuzz(func(t *testing.T, stored []byte, offset int64, readLen int) {
		if len(stored) == 0 || readLen <= 0 || readLen > fuzzMaxSource {
			return
		}
		r, err := c.DecompressRanged(t.Context(), &countingFetcher{data: stored}, int64(len(stored)))
		if err != nil {
			if !wellClassified(err) {
				t.Errorf("DecompressRanged error not classified: %v", err)
			}
			return
		}
		defer func() { _ = r.Close() }()

		buf := make([]byte, readLen)
		if _, err := r.ReadAt(buf, offset); err != nil && !errors.Is(err, io.EOF) && !wellClassified(err) {
			t.Errorf("ReadAt error not classified: %v", err)
		}
	})
}

// FuzzDecompress does the same for the whole-object entry point, which reads a
// seekable source directly rather than through a RangeFetcher.
func FuzzDecompress(f *testing.F) {
	c := newTestCodec(f)

	var valid bytes.Buffer
	if _, err := c.Compress(&valid, bytes.NewReader(compressible(4096))); err != nil {
		f.Fatalf("Compress: %v", err)
	}
	f.Add(valid.Bytes())
	f.Add(valid.Bytes()[:valid.Len()/2])
	f.Add([]byte{})
	f.Add(make([]byte, 9))
	f.Add([]byte("not a zstd stream at all"))

	f.Fuzz(func(t *testing.T, stored []byte) {
		r, err := c.Decompress(bytes.NewReader(stored))
		if err != nil {
			if !wellClassified(err) {
				t.Errorf("Decompress error not classified: %v", err)
			}
			return
		}
		defer func() { _ = r.Close() }()

		// Bounded because a crafted seek table can describe a great deal of
		// output, and this target is about how a decode fails rather than how
		// much it can produce.
		if _, err := io.Copy(io.Discard, io.LimitReader(r, fuzzMaxSource)); err != nil && !wellClassified(err) {
			t.Errorf("read error not classified: %v", err)
		}
	})
}

// FuzzRangedSeekRead drives Seek and Read over a valid object with fuzzed
// offsets, covering seeks past the end and backwards seeks. Bytes that come back
// must be the source bytes at that offset: a wrong-but-plausible answer is worse
// than an error, because nothing downstream would notice.
func FuzzRangedSeekRead(f *testing.F) {
	c := newTestCodec(f)

	f.Add(4096, int64(0), 0, 128)
	f.Add(4096, int64(-1), 0, 16)              // negative absolute offset
	f.Add(4096, int64(1<<40), 0, 16)           // far past the end
	f.Add(testChunk*3, int64(testChunk), 0, 8) // exactly on a frame boundary
	f.Add(testChunk*2, int64(-16), 2, 16)      // relative to the end
	f.Add(0, int64(0), 0, 1)

	f.Fuzz(func(t *testing.T, srcLen int, offset int64, whence, readLen int) {
		if srcLen < 0 || srcLen > fuzzMaxSource || readLen <= 0 || readLen > fuzzMaxSource {
			return
		}
		src := compressible(srcLen)
		stored := fuzzStore(t, c, src)

		r, err := c.DecompressRanged(t.Context(), &countingFetcher{data: stored}, int64(len(stored)))
		if err != nil {
			t.Fatalf("DecompressRanged over a valid object: %v", err)
		}
		defer func() { _ = r.Close() }()

		assertSeekReadMatchesSource(t, r, src, offset, ((whence%3)+3)%3, readLen)
	})
}

// assertSeekReadMatchesSource seeks r and checks that whatever comes back is the
// source at that position, and that a position past the end yields nothing.
func assertSeekReadMatchesSource(t *testing.T, r RangedReader, src []byte, offset int64, whence, readLen int) {
	t.Helper()
	pos, err := r.Seek(offset, whence)
	if err != nil {
		return
	}
	if pos < 0 {
		t.Fatalf("Seek returned negative position %d", pos)
	}

	buf := make([]byte, readLen)
	n, err := io.ReadFull(r, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read after seek to %d: %v", pos, err)
	}
	if pos >= int64(len(src)) {
		if n != 0 {
			t.Errorf("read %d bytes at offset %d, past the end of a %d byte object", n, pos, len(src))
		}
		return
	}
	if !bytes.Equal(buf[:n], src[pos:pos+int64(n)]) {
		t.Errorf("read %d bytes at offset %d that do not match the source", n, pos)
	}
}

// FuzzRangedFetcherMisbehaves corrupts one fetch of an otherwise valid object.
// A read may fail, but it may not succeed with bytes that are not the source's:
// serving a plausible wrong answer is the failure mode this whole seam exists to
// prevent.
func FuzzRangedFetcherMisbehaves(f *testing.F) {
	c := newTestCodec(f)

	f.Add(testChunk*2, 0, 1, int64(0), 256)
	f.Add(testChunk*2, 1, 1, int64(0), 256)
	f.Add(testChunk*2, 2, 2, int64(testChunk), 256)
	f.Add(testChunk*2, 3, 1, int64(0), 64)
	f.Add(4096, 2, 1, int64(0), 4096)

	f.Fuzz(func(t *testing.T, srcLen, mode, at int, offset int64, readLen int) {
		if srcLen <= 0 || srcLen > fuzzMaxSource || readLen <= 0 || readLen > fuzzMaxSource {
			return
		}
		if mode < 0 || at < 1 || at > 64 || offset < 0 {
			return
		}
		src := compressible(srcLen)
		stored := fuzzStore(t, c, src)

		r, err := c.DecompressRanged(t.Context(), &misbehavingFetcher{data: stored, mode: mode, at: at}, int64(len(stored)))
		if err != nil {
			if !wellClassified(err) {
				t.Errorf("DecompressRanged error not classified: %v", err)
			}
			return
		}
		defer func() { _ = r.Close() }()

		assertReadAtIsSourceOrError(t, r, src, offset, readLen)
	})
}

// assertReadAtIsSourceOrError checks the one outcome a misbehaving backend may
// never produce: a successful read of bytes that are not the source's. Failing
// is allowed, as long as the failure says which kind it was.
func assertReadAtIsSourceOrError(t *testing.T, r RangedReader, src []byte, offset int64, readLen int) {
	t.Helper()
	buf := make([]byte, readLen)
	n, err := r.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		if !wellClassified(err) {
			t.Errorf("ReadAt error not classified: %v", err)
		}
		return
	}
	if offset+int64(n) > int64(len(src)) {
		t.Fatalf("ReadAt returned %d bytes at offset %d, past the end of a %d byte object", n, offset, len(src))
	}
	if !bytes.Equal(buf[:n], src[offset:offset+int64(n)]) {
		t.Errorf("ReadAt succeeded with %d bytes at offset %d that do not match the source", n, offset)
	}
}
