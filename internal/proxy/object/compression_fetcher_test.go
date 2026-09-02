// -------------------------------------------------------------------------------
// Ranged Fetch Tests
//
// Author: Alex Freidah
//
// The fetcher is where the compressed domain meets the ciphertext domain, so
// the tests that matter run a real Encryptor over a real compressed object and
// read random ranges back through the codec. Testing the two layers separately
// never reaches that composition, which is the only place their offset math has
// to agree.
// -------------------------------------------------------------------------------

package object

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/proxy/accounting"
	"github.com/afreidah/s3-orchestrator/internal/proxy/readpath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// fetchTestKey is the object key every case in this file stores under.
const fetchTestKey = "compressed/object"

// fetchTestChunk is the compression chunk size these tests write at: small
// enough to produce several frames from a modest fixture.
const fetchTestChunk = compression.MinChunkSize

// fakeRangeRuntime satisfies RangeFetchRuntime over a real usage tracker and
// recorder, so per-fetch charges land on the counters production reads.
type fakeRangeRuntime struct {
	usage *counter.UsageTracker
	acct  *accounting.Recorder

	mu     sync.Mutex
	ranges []string
}

// newFakeRangeRuntime builds a runtime whose backend carries the given limits.
// A zero UsageLimits means unlimited.
func newFakeRangeRuntime(beName string, limits core.UsageLimits) *fakeRangeRuntime {
	usage := counter.NewUsageTracker(
		counter.NewLocalCounterBackend([]string{beName}),
		map[string]core.UsageLimits{beName: limits},
	)
	return &fakeRangeRuntime{
		usage: usage,
		acct:  accounting.New(usage, func(string, string, time.Time, error) {}),
	}
}

// GetWithTimeout forwards to the backend and records the Range header asked
// for, which is what the ciphertext translation is asserted on.
func (f *fakeRangeRuntime) GetWithTimeout(ctx context.Context, be backend.ObjectBackend, key, rangeHeader string) (*backend.GetObjectResult, context.CancelFunc, error) {
	f.mu.Lock()
	f.ranges = append(f.ranges, rangeHeader)
	f.mu.Unlock()

	r, err := be.GetObject(ctx, key, rangeHeader)
	if err != nil {
		return nil, nil, err
	}
	return r, func() {}, nil
}

// Usage returns the tracker the per-fetch limit checks run against.
func (f *fakeRangeRuntime) Usage() *counter.UsageTracker { return f.usage }

// Acct returns the recorder the per-fetch charges land on.
func (f *fakeRangeRuntime) Acct() *accounting.Recorder { return f.acct }

// fetchCount reports how many backend GETs the fetcher issued.
func (f *fakeRangeRuntime) fetchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.ranges)
}

// storedObject is one object as it sits on a backend, alongside the metadata
// row a read needs to interpret it.
type storedObject struct {
	rt  *fakeRangeRuntime
	be  *backendtest.InMemory
	enc *encryption.Encryptor
	loc *core.ObjectLocation
}

// fetcher builds the fetcher under test for this object.
func (s *storedObject) fetcher() *storedRangeFetcher {
	return newStoredRangeFetcher(s.rt, s.be, s.enc, s.loc, fetchTestKey, "be1")
}

// newTestCodec builds a codec at the test chunk size and closes it with the
// test.
func newTestCodec(t *testing.T) *compression.Codec {
	t.Helper()
	c, err := compression.NewCodec(compression.DefaultLevel, fetchTestChunk)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// storeCompressed compresses src onto an in-memory backend, encrypting it first
// when encrypt is set. The location row carries the three sizes a read has to
// keep apart: what the backend holds, the compressed stream inside it, and the
// original object.
func storeCompressed(t *testing.T, c *compression.Codec, src []byte, encrypt bool) *storedObject {
	t.Helper()
	ctx := context.Background()

	var compressed bytes.Buffer
	if _, err := c.Compress(&compressed, bytes.NewReader(src)); err != nil {
		t.Fatalf("Compress: %v", err)
	}

	loc := &core.ObjectLocation{
		ObjectKey:                fetchTestKey,
		BackendName:              "be1",
		PlaintextSize:            int64(compressed.Len()),
		LogicalSize:              int64(len(src)),
		CompressionAlgorithm:     "zstd",
		CompressionFormatVersion: 1,
	}
	stored := compressed.Bytes()

	var enc *encryption.Encryptor
	if encrypt {
		enc = newTestEncryptor(t)
		res, err := enc.Encrypt(ctx, bytes.NewReader(stored), int64(len(stored)))
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		if stored, err = io.ReadAll(res.Body); err != nil {
			t.Fatalf("read ciphertext: %v", err)
		}
		loc.Encrypted = true
		loc.EncryptionKey = encryption.PackKeyData(res.BaseNonce, res.WrappedDEK)
		loc.KeyID = res.KeyID
	}
	loc.SizeBytes = int64(len(stored))

	be := backendtest.NewInMemory()
	if _, err := be.PutObject(ctx, fetchTestKey, bytes.NewReader(stored), int64(len(stored)), "application/octet-stream", nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	return &storedObject{rt: newFakeRangeRuntime("be1", core.UsageLimits{}), be: be, enc: enc, loc: loc}
}

// TestStoredRangeFetcher_ReturnsCompressedBytes checks both stored forms
// against the compressed stream directly: exactly the bytes asked for.
func TestStoredRangeFetcher_ReturnsCompressedBytes(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)
	src := incompressibleFixture(t, fetchTestChunk*2)

	var want bytes.Buffer
	if _, err := c.Compress(&want, bytes.NewReader(src)); err != nil {
		t.Fatalf("Compress: %v", err)
	}

	for _, encrypt := range []bool{false, true} {
		t.Run(storedFormName(encrypt), func(t *testing.T) {
			t.Parallel()
			assertFetchesCompressedBytes(t, storeCompressed(t, c, src, encrypt), want.Bytes())
		})
	}
}

// assertFetchesCompressedBytes reads a spread of ranges off s and compares each
// against the compressed stream it was built from.
func assertFetchesCompressedBytes(t *testing.T, s *storedObject, want []byte) {
	t.Helper()
	f := s.fetcher()
	if got := f.compressedSize(); got != int64(len(want)) {
		t.Fatalf("compressedSize = %d, want %d", got, len(want))
	}
	last := int64(len(want)) - 1
	for _, r := range [][2]int64{{0, 0}, {0, 99}, {5, 5}, {17, 4096}, {last - 9, last}} {
		got, err := f.FetchRange(context.Background(), r[0], r[1])
		if err != nil {
			t.Fatalf("FetchRange(%d, %d): %v", r[0], r[1], err)
		}
		if !bytes.Equal(got, want[r[0]:r[1]+1]) {
			t.Errorf("FetchRange(%d, %d) returned the wrong bytes", r[0], r[1])
		}
	}
}

// TestStoredRangeFetcher_CompressedEncryptedRange covers the composition no
// pairwise test reaches: a compressed object inside an encryption envelope,
// read at random ranges. Both layers have to agree that PlaintextSize is the
// compressed size for any of it to work.
func TestStoredRangeFetcher_CompressedEncryptedRange(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)
	src := compressibleFixture(fetchTestChunk*5 + 613)
	s := storeCompressed(t, c, src, true)
	f := s.fetcher()

	r, err := c.DecompressRanged(context.Background(), f, f.compressedSize())
	if err != nil {
		t.Fatalf("DecompressRanged: %v", err)
	}
	defer func() { _ = r.Close() }()

	rnd := rand.New(rand.NewSource(1249)) //nolint:gosec // G404: deterministic test input, not security material
	for range 50 {
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

// TestStoredRangeFetcher_ChargesEveryFetch pins the accounting decision: each
// fetch is metered, so a backend runs out of API calls after as many fetches as
// its limit allows rather than after as many client GETs.
func TestStoredRangeFetcher_ChargesEveryFetch(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)
	s := storeCompressed(t, c, compressibleFixture(fetchTestChunk*4), false)
	f := s.fetcher()

	const fetches = 3
	for i := range fetches {
		if _, err := f.FetchRange(context.Background(), int64(i), int64(i)+15); err != nil {
			t.Fatalf("FetchRange: %v", err)
		}
	}

	// One more API call than the object could afford proves each fetch was
	// charged rather than the read as a whole.
	if s.rt.usage.WithinLimits("be1", getObjectOp, 0, 0) != true {
		t.Fatal("unlimited backend reported over its limit")
	}
	limited := newFakeRangeRuntime("be1", requestCapped(t, fetches, 0, 0))
	s.rt = limited
	f = s.fetcher()
	for i := range fetches {
		if _, err := f.FetchRange(context.Background(), int64(i), int64(i)+15); err != nil {
			t.Fatalf("FetchRange %d: %v", i, err)
		}
	}
	if _, err := f.FetchRange(context.Background(), 0, 15); !errors.Is(err, readpath.ErrUsageLimitSkip) {
		t.Errorf("fetch past the API call limit err = %v, want ErrUsageLimitSkip", err)
	}
}

// TestStoredRangeFetcher_TranslatesEncryptedRange checks that an encrypted copy
// is asked for in ciphertext coordinates; compressed-domain offsets would
// select the wrong bytes and decrypt into plausible garbage.
func TestStoredRangeFetcher_TranslatesEncryptedRange(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)
	s := storeCompressed(t, c, incompressibleFixture(t, fetchTestChunk), true)
	f := s.fetcher()

	if _, err := f.FetchRange(context.Background(), 100, 199); err != nil {
		t.Fatalf("FetchRange: %v", err)
	}
	want, err := encryption.CiphertextRange(100, 199, s.enc.ChunkSize())
	if err != nil {
		t.Fatalf("CiphertextRange: %v", err)
	}
	s.rt.mu.Lock()
	got := s.rt.ranges[0]
	s.rt.mu.Unlock()
	if got != want.BackendRange {
		t.Errorf("backend range = %q, want %q", got, want.BackendRange)
	}
}

// TestStoredRangeFetcher_UnencryptedPassesRangeThrough checks the identity
// mapping: bytes stored verbatim are already in the compressed domain.
func TestStoredRangeFetcher_UnencryptedPassesRangeThrough(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)
	s := storeCompressed(t, c, incompressibleFixture(t, fetchTestChunk), false)

	if _, err := s.fetcher().FetchRange(context.Background(), 100, 199); err != nil {
		t.Fatalf("FetchRange: %v", err)
	}
	s.rt.mu.Lock()
	got := s.rt.ranges[0]
	s.rt.mu.Unlock()
	if got != "bytes=100-199" {
		t.Errorf("backend range = %q, want bytes=100-199", got)
	}
}

// TestStoredRangeFetcher_RejectsInvalidRange covers bounds the seekable library
// should never produce but a corrupt seek table could.
func TestStoredRangeFetcher_RejectsInvalidRange(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)
	f := storeCompressed(t, c, compressibleFixture(fetchTestChunk), false).fetcher()

	for _, r := range [][2]int64{{-1, 10}, {10, 9}} {
		if _, err := f.FetchRange(context.Background(), r[0], r[1]); !errors.Is(err, core.ErrInvalidRange) {
			t.Errorf("FetchRange(%d, %d) err = %v, want ErrInvalidRange", r[0], r[1], err)
		}
	}
}

// TestStoredRangeFetcher_PropagatesBackendError checks that a backend failure
// surfaces and is still charged its API call, since the call was made.
func TestStoredRangeFetcher_PropagatesBackendError(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)
	s := storeCompressed(t, c, compressibleFixture(fetchTestChunk), false)
	sentinel := errors.New("backend unreachable")
	s.be.GetErr = sentinel

	if _, err := s.fetcher().FetchRange(context.Background(), 0, 15); !errors.Is(err, sentinel) {
		t.Errorf("FetchRange err = %v, want %v", err, sentinel)
	}
	if s.rt.fetchCount() != 1 {
		t.Errorf("backend calls = %d, want 1", s.rt.fetchCount())
	}
}

// TestStoredRangeFetcher_RejectsShortBody covers a backend that answers a range
// with fewer bytes than it was asked for.
func TestStoredRangeFetcher_RejectsShortBody(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)
	s := storeCompressed(t, c, compressibleFixture(fetchTestChunk), false)
	s.be.GetReadErr = io.ErrUnexpectedEOF

	if _, err := s.fetcher().FetchRange(context.Background(), 0, 15); err == nil {
		t.Error("FetchRange over a truncated body returned no error")
	}
}

// storedFormName labels the encrypted and unencrypted subtests.
func storedFormName(encrypted bool) string {
	if encrypted {
		return "encrypted"
	}
	return "plaintext"
}

// compressibleFixture returns n bytes zstd can actually shrink, so the stored
// objects these tests build have realistic frame sizes.
func compressibleFixture(n int) []byte {
	out := make([]byte, 0, n)
	line := []byte("the quick brown fox jumps over the lazy dog 0123456789\n")
	for len(out) < n {
		out = append(out, line...)
	}
	return out[:n]
}

// incompressibleFixture returns n bytes zstd cannot shrink, so a test naming a
// specific compressed-stream offset gets an object large enough to hold it.
func incompressibleFixture(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	rnd := rand.New(rand.NewSource(99)) //nolint:gosec // G404: test fixture, not security material
	if _, err := rnd.Read(b); err != nil {
		t.Fatalf("seed random: %v", err)
	}
	return b
}
