// -------------------------------------------------------------------------------
// PUT Compression Tests
//
// Author: Alex Freidah
//
// A compressed write is only correct if two numbers stay apart: the size the
// client wrote, which the object is known by, and the size that lands on a
// backend, which placement, quota and accounting all decide on. These tests
// pin both, and pin that the stored bytes decode back to what was written.
// -------------------------------------------------------------------------------

package object

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// putCompressChunk is the codec chunk size these tests write at, small enough
// that a modest fixture still crosses frame boundaries.
const putCompressChunk = compression.MinChunkSize

// newPutCodec builds a codec for the write path and closes it with the test.
func newPutCodec(t *testing.T) *compression.Codec {
	t.Helper()
	c, err := compression.NewCodec(compression.DefaultLevel, putCompressChunk)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// compressibleBody returns n bytes zstd can shrink, so a test asserting the
// stored size dropped is not fighting incompressible input.
func compressibleBody(n int) []byte {
	out := make([]byte, 0, n)
	line := []byte("the quick brown fox jumps over the lazy dog 0123456789\n")
	for len(out) < n {
		out = append(out, line...)
	}
	return out[:n]
}

// compressionOn returns a config with compression enabled at the given
// minimum size. MinRatio is set explicitly because the fleet builds this
// struct directly: config validation, which is what applies the default in
// production, does not run here, and a zero ratio would store every object
// verbatim.
func compressionOn(minSize int64) config.CompressionConfig {
	return config.CompressionConfig{
		Enabled:   true,
		Level:     "default",
		ChunkSize: putCompressChunk,
		MinSize:   minSize,
		MinRatio:  config.DefaultCompressionMinRatio,
	}
}

// incompressibleBody returns n bytes zstd cannot shrink. The encoder detects
// the unshrinkable blocks and stores them raw, so the encoded form comes back
// the same size as the original plus frame and seek-table overhead.
func incompressibleBody(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("read random body: %v", err)
	}
	return b
}

// putResult is what one PUT through the fleet leaves behind: the bytes on the
// backend and the representation metadata recorded alongside them.
type putResult struct {
	be     *backendtest.InMemory
	stored []byte
	size   int64
	form   *core.StoredForm
}

// putThroughFleet runs one PUT against a single in-memory backend and returns
// the stored bytes with the row that describes them.
func putThroughFleet(t *testing.T, opts *fleetOpts, key string, body []byte) putResult {
	t.Helper()
	be := backendtest.NewInMemory()
	calls := &objectsCalls{}
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjGetBackend(calls, "b1", nil)).AnyTimes()
	store.EXPECT().GetLeastUtilizedBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjGetLeastUtilized(calls, "b1", nil)).AnyTimes()
	store.EXPECT().RecordObjectAndClearPending(
		gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjRecordAndClear(calls, nil)).AnyTimes()
	store.EXPECT().RecordObject(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjRecord(calls, nil)).AnyTimes()
	storetest.Permissive(store)

	opts.Order = []string{"b1"}
	f := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, opts)

	if _, err := f.PutObject(context.Background(), key, bytes.NewReader(body), int64(len(body)), "text/plain", nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if len(calls.recordObject) != 1 {
		t.Fatalf("recorded %d rows, want 1", len(calls.recordObject))
	}
	rec := calls.recordObject[0]
	return putResult{be: be, stored: be.Objects[key].Data, size: rec.Size, form: rec.Form}
}

// TestPut_CompressesAndRecordsRepresentation checks the whole write-side
// contract at once: fewer bytes land than the client sent, the row says how
// they were encoded and how big the object really is, and the stored bytes
// decode back to exactly what was written.
func TestPut_CompressesAndRecordsRepresentation(t *testing.T) {
	t.Parallel()
	codec := newPutCodec(t)
	src := compressibleBody(putCompressChunk * 3)

	res := putThroughFleet(t, &fleetOpts{Codec: codec, Compression: compressionOn(0)}, "key", src)

	if len(res.stored) >= len(src) {
		t.Fatalf("stored %d bytes for a %d byte object; compression did not apply", len(res.stored), len(src))
	}
	if res.form == nil {
		t.Fatal("no representation metadata recorded for a compressed object")
	}
	if res.form.CompressionAlgorithm != compression.Algorithm {
		t.Errorf("CompressionAlgorithm = %q, want %q", res.form.CompressionAlgorithm, compression.Algorithm)
	}
	if res.form.CompressionFormatVersion != compression.FormatVersion {
		t.Errorf("CompressionFormatVersion = %d, want %d", res.form.CompressionFormatVersion, compression.FormatVersion)
	}
	if res.form.CompressionLevel != "default" {
		t.Errorf("CompressionLevel = %q, want %q", res.form.CompressionLevel, "default")
	}
	if res.form.LogicalSize != int64(len(src)) {
		t.Errorf("LogicalSize = %d, want %d", res.form.LogicalSize, len(src))
	}
	if res.size != int64(len(res.stored)) {
		t.Errorf("recorded size %d, want the %d bytes that landed", res.size, len(res.stored))
	}

	r, err := codec.Decompress(bytes.NewReader(res.stored))
	if err != nil {
		t.Fatalf("Decompress the stored object: %v", err)
	}
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read decompressed: %v", err)
	}
	if !bytes.Equal(got, src) {
		t.Error("the stored object did not decode back to what was written")
	}
}

// TestPut_SkipsObjectsBelowMinSize checks that a small object is stored
// verbatim, and that the row says so by carrying no algorithm at all rather
// than an algorithm with nothing behind it.
func TestPut_SkipsObjectsBelowMinSize(t *testing.T) {
	t.Parallel()
	src := compressibleBody(512)

	res := putThroughFleet(t, &fleetOpts{
		Codec:       newPutCodec(t),
		Compression: compressionOn(4096),
	}, "small", src)

	if !bytes.Equal(res.stored, src) {
		t.Error("an object below min_size was not stored verbatim")
	}
	if res.form != nil && res.form.CompressionAlgorithm != "" {
		t.Errorf("CompressionAlgorithm = %q, want empty for an uncompressed object", res.form.CompressionAlgorithm)
	}
}

// TestPut_SkipsIncompressibleObject checks the entropy floor: an object the
// encoder cannot shrink is stored as the client sent it, and the row says so by
// carrying no algorithm. Without this the orchestrator would pay encode CPU on
// every such write and decode CPU on every read of it, forever, for no bytes
// saved.
func TestPut_SkipsIncompressibleObject(t *testing.T) {
	t.Parallel()
	src := incompressibleBody(t, putCompressChunk*3)

	res := putThroughFleet(t, &fleetOpts{
		Codec:       newPutCodec(t),
		Compression: compressionOn(0),
	}, "random", src)

	if !bytes.Equal(res.stored, src) {
		t.Error("an incompressible object was not stored verbatim")
	}
	if res.form != nil && res.form.CompressionAlgorithm != "" {
		t.Errorf("CompressionAlgorithm = %q, want empty for an object stored raw", res.form.CompressionAlgorithm)
	}
	if res.size != int64(len(src)) {
		t.Errorf("recorded size %d, want the %d bytes the client sent", res.size, len(src))
	}
}

// TestPut_HonoursMinRatio checks that the threshold is what decides, not merely
// whether the object shrank at all. An unreachable ratio stores a highly
// compressible object verbatim.
func TestPut_HonoursMinRatio(t *testing.T) {
	t.Parallel()
	src := compressibleBody(putCompressChunk * 3)
	cfg := compressionOn(0)
	cfg.MinRatio = 0.000001

	res := putThroughFleet(t, &fleetOpts{Codec: newPutCodec(t), Compression: cfg}, "key", src)

	if !bytes.Equal(res.stored, src) {
		t.Error("an object that missed min_ratio was still stored compressed")
	}
	if res.form != nil && res.form.CompressionAlgorithm != "" {
		t.Errorf("CompressionAlgorithm = %q, want empty", res.form.CompressionAlgorithm)
	}
}

// TestPut_DisabledLeavesBytesVerbatim checks that a wired codec does nothing
// while the config says compression is off, so the feature is genuinely opt-in.
func TestPut_DisabledLeavesBytesVerbatim(t *testing.T) {
	t.Parallel()
	src := compressibleBody(putCompressChunk * 2)

	res := putThroughFleet(t, &fleetOpts{
		Codec:       newPutCodec(t),
		Compression: config.CompressionConfig{},
	}, "plain", src)

	if !bytes.Equal(res.stored, src) {
		t.Error("compression ran while disabled")
	}
	if res.form != nil && res.form.CompressionAlgorithm != "" {
		t.Errorf("CompressionAlgorithm = %q, want empty", res.form.CompressionAlgorithm)
	}
}

// TestPut_CompressedAndEncryptedSizes pins the composition the read path
// depends on: PlaintextSize is the encryptor's input, which is the compressed
// stream, while LogicalSize stays the object the client wrote. Confusing the
// two makes every ranged read of a compressed object address the wrong bytes.
func TestPut_CompressedAndEncryptedSizes(t *testing.T) {
	t.Parallel()
	src := compressibleBody(putCompressChunk * 2)

	res := putThroughFleet(t, &fleetOpts{
		Codec:       newPutCodec(t),
		Compression: compressionOn(0),
		Encryptor:   newTestEncryptor(t),
	}, "both", src)

	if res.form == nil || !res.form.Encrypted {
		t.Fatal("row is not marked encrypted")
	}
	if res.form.LogicalSize != int64(len(src)) {
		t.Errorf("LogicalSize = %d, want the %d bytes the client wrote", res.form.LogicalSize, len(src))
	}
	if res.form.PlaintextSize >= int64(len(src)) {
		t.Errorf("PlaintextSize = %d, want the compressed size, below %d", res.form.PlaintextSize, len(src))
	}
	if res.size != int64(len(res.stored)) {
		t.Errorf("recorded size %d, want the %d ciphertext bytes stored", res.size, len(res.stored))
	}
}

// TestPut_EligibilityUsesStoredSize covers the ordering change: quota is
// checked against the bytes that will actually land. A write too large for the
// backend raw but small enough compressed has to succeed, which it cannot if
// eligibility still runs before the body is encoded.
func TestPut_EligibilityUsesStoredSize(t *testing.T) {
	t.Parallel()
	src := compressibleBody(putCompressChunk * 4)
	limit := int64(len(src)) / 2

	res := putThroughFleet(t, &fleetOpts{
		Codec:       newPutCodec(t),
		Compression: compressionOn(0),
		UsageLimits: map[string]core.UsageLimits{"b1": {IngressByteLimit: limit}},
	}, "tight", src)

	if res.form == nil || res.form.CompressionAlgorithm != compression.Algorithm {
		t.Fatal("the write did not compress")
	}
	if res.size > limit {
		t.Errorf("stored %d bytes against an ingress limit of %d", res.size, limit)
	}
}

// noDecode supplies the decode half of ObjectCodec for write-path fakes, which
// never reach it. Embedded rather than repeated so a fake states only the
// behaviour its test is about.
type noDecode struct{}

// DecompressRanged implements ObjectCodec.
func (noDecode) DecompressRanged(context.Context, compression.RangeFetcher, int64) (compression.RangedReader, error) {
	return nil, errors.New("decode is not reached on the write path")
}

// failingCodec fails every encode. It is why ObjectCodec is an interface: the
// concrete codec only fails on inputs the write path cannot produce, so the
// mid-upload failure branch is unreachable without a fake.
type failingCodec struct {
	noDecode
	err error
}

// Compress implements ObjectCodec by consuming nothing and failing.
func (f failingCodec) Compress(io.Writer, io.Reader) (int64, error) { return 0, f.err }

// shortCodec reports having written more bytes than it did, standing in for a
// codec whose byte count and output disagree.
type shortCodec struct {
	noDecode
	claim int64
}

// Compress implements ObjectCodec.
func (s shortCodec) Compress(dst io.Writer, _ io.Reader) (int64, error) {
	_, err := dst.Write([]byte("short"))
	return s.claim, err
}

// TestPut_CompressFailureAbortsWrite checks that an encode failure fails the
// upload rather than falling back to storing the object uncompressed. A silent
// fallback would leave the row describing bytes that are not there.
func TestPut_CompressFailureAbortsWrite(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	calls := &objectsCalls{}
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjGetBackend(calls, "b1", nil)).AnyTimes()
	store.EXPECT().GetLeastUtilizedBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjGetLeastUtilized(calls, "b1", nil)).AnyTimes()
	storetest.Permissive(store)

	boom := errors.New("encoder exploded")
	f := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, &fleetOpts{
		Order:       []string{"b1"},
		Codec:       failingCodec{err: boom},
		Compression: compressionOn(0),
	})

	src := compressibleBody(4096)
	_, err := f.PutObject(context.Background(), "boom", bytes.NewReader(src), int64(len(src)), "text/plain", nil)
	if !errors.Is(err, boom) {
		t.Fatalf("PutObject err = %v, want %v", err, boom)
	}
	if be.Has("boom") {
		t.Error("bytes were stored despite the encode failing")
	}
}

// TestPut_CompressedSizeComesFromTheCodec pins which number the write path
// trusts. The codec reports what it wrote, and that is what has to be announced
// to the backend and recorded: taking the sink's own length instead would be
// wrong for a body that spilled to disk, where it is not tracked.
func TestPut_CompressedSizeComesFromTheCodec(t *testing.T) {
	t.Parallel()
	src := compressibleBody(4096)

	res := putThroughFleet(t, &fleetOpts{
		Codec:       shortCodec{claim: 5},
		Compression: compressionOn(0),
	}, "claim", src)

	if res.size != 5 {
		t.Errorf("recorded size %d, want the 5 bytes the codec reported", res.size)
	}
	if !bytes.Equal(res.stored, []byte("short")) {
		t.Errorf("stored %q, want the codec's output", res.stored)
	}
}
