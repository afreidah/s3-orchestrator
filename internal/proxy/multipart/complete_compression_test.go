// -------------------------------------------------------------------------------
// Multipart Compression Tests
//
// Author: Alex Freidah
//
// A completed upload is one object however many parts built it, so assembly has
// to produce the same thing a single PUT would: an encoding whose chunk
// boundaries owe nothing to the part sizes the client happened to choose, a row
// that says how big the object really is, and the client's own bytes back out
// of it. These tests pin that, and pin the case where the encoding is thrown
// away because it did not earn its place.
// -------------------------------------------------------------------------------

package multipart

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// mpChunk is small enough that a modest fixture still crosses frame
// boundaries; mpUploadID and mpObjectKey name the upload every test assembles.
const (
	mpChunk     = compression.MinChunkSize
	mpUploadID  = "upload-z"
	mpObjectKey = "multi/zipped"
)

// newMPCodec builds a codec for assembly and closes it with the test.
func newMPCodec(t *testing.T) *compression.Codec {
	t.Helper()
	c, err := compression.NewCodec(compression.DefaultLevel, mpChunk)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// mpCompressionOn returns a config with compression enabled. MinRatio is set
// explicitly because the fleet builds this struct directly and config
// validation, which applies the default in production, does not run here.
func mpCompressionOn() config.CompressionConfig {
	return config.CompressionConfig{
		Enabled:   true,
		Level:     "default",
		ChunkSize: mpChunk,
		MinRatio:  config.DefaultCompressionMinRatio,
	}
}

// compressiblePart returns n bytes zstd can shrink.
func compressiblePart(n int) []byte {
	out := make([]byte, 0, n)
	line := []byte("the quick brown fox jumps over the lazy dog 0123456789\n")
	for len(out) < n {
		out = append(out, line...)
	}
	return out[:n]
}

// incompressiblePart returns n bytes zstd cannot shrink.
func incompressiblePart(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("read random part: %v", err)
	}
	return b
}

// assembled is what one completion leaves behind: the bytes that landed on the
// backend and the row recorded for them.
type assembled struct {
	stored []byte
	rec    multipartObjectCall
}

// completeWithParts seeds each part on a backend, completes the upload, and
// returns the assembled object with its row. Part sizes come from the fixture
// rather than the chunk size, which is the point: a client picks them.
func completeWithParts(t *testing.T, opts *fleetOpts, bodies [][]byte) assembled {
	t.Helper()
	ctx := context.Background()
	be := backendtest.NewInMemory()

	parts := make([]core.MultipartPart, len(bodies))
	numbers := make([]int, len(bodies))
	for i, body := range bodies {
		key := multipartPartKey(mpUploadID, i+1)
		if _, err := be.PutObject(ctx, key, bytes.NewReader(body), int64(len(body)),
			"application/octet-stream", nil); err != nil {
			t.Fatalf("seed part %d: %v", i+1, err)
		}
		parts[i] = core.MultipartPart{PartNumber: i + 1, SizeBytes: int64(len(body))}
		numbers[i] = i + 1
	}

	store, calls := completeStoreSetup(t,
		&core.MultipartUpload{
			UploadID: mpUploadID, ObjectKey: mpObjectKey,
			BackendName: "b1", ContentType: "application/octet-stream",
		}, parts, nil)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, opts)
	if _, err := mgr.CompleteMultipartUpload(ctx, "multi", "zipped", mpUploadID, partsOf(numbers...)); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}
	if len(calls.recordObject) != 1 {
		t.Fatalf("recorded %d rows, want 1", len(calls.recordObject))
	}
	obj, ok := be.Get(mpObjectKey)
	if !ok {
		t.Fatal("assembled object not found on the backend")
	}
	return assembled{stored: obj.Data, rec: calls.recordObject[0]}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestComplete_CompressesAssembledObject is the headline: parts sized by the
// client assemble into one encoded object, the row describes the encoding and
// carries the size the client uploaded, and the stored bytes decode back to the
// concatenation of the parts.
func TestComplete_CompressesAssembledObject(t *testing.T) {
	t.Parallel()
	codec := newMPCodec(t)
	// Deliberately not a multiple of the chunk size: a part boundary landing
	// mid-chunk is the case the encoding has to be indifferent to.
	bodies := [][]byte{
		compressiblePart(mpChunk + 700),
		compressiblePart(mpChunk*2 - 300),
		compressiblePart(1500),
	}
	var want []byte
	for _, b := range bodies {
		want = append(want, b...)
	}

	res := completeWithParts(t, &fleetOpts{Codec: codec, Compression: mpCompressionOn()}, bodies)

	if len(res.stored) >= len(want) {
		t.Fatalf("stored %d bytes for a %d byte object; compression did not apply", len(res.stored), len(want))
	}
	if res.rec.Form == nil {
		t.Fatal("no representation metadata recorded for a compressed assembly")
	}
	if res.rec.Form.CompressionAlgorithm != compression.Algorithm {
		t.Errorf("CompressionAlgorithm = %q, want %q", res.rec.Form.CompressionAlgorithm, compression.Algorithm)
	}
	if res.rec.Form.CompressionFormatVersion != compression.FormatVersion {
		t.Errorf("CompressionFormatVersion = %d, want %d", res.rec.Form.CompressionFormatVersion, compression.FormatVersion)
	}
	if res.rec.Form.CompressionLevel != "default" {
		t.Errorf("CompressionLevel = %q, want %q", res.rec.Form.CompressionLevel, "default")
	}
	if res.rec.Form.LogicalSize != int64(len(want)) {
		t.Errorf("LogicalSize = %d, want %d", res.rec.Form.LogicalSize, len(want))
	}
	if res.rec.Size != int64(len(res.stored)) {
		t.Errorf("recorded size %d, want the %d bytes that landed", res.rec.Size, len(res.stored))
	}

	r, err := codec.Decompress(bytes.NewReader(res.stored))
	if err != nil {
		t.Fatalf("Decompress the assembled object: %v", err)
	}
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read decompressed: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Error("the assembled object did not decode back to the parts that built it")
	}
}

// countingCodec delegates to a real codec and counts both halves, so a test can
// tell an encoding that was made and thrown away from one that was never made.
type countingCodec struct {
	inner            Codec
	encodes, decodes int
}

// Compress implements Codec.
func (c *countingCodec) Compress(dst io.Writer, src io.Reader) (int64, error) {
	c.encodes++
	return c.inner.Compress(dst, src)
}

// Decompress implements Codec.
func (c *countingCodec) Decompress(rs io.ReadSeeker) (io.ReadCloser, error) {
	c.decodes++
	return c.inner.Decompress(rs)
}

// TestComplete_DiscardsUselessEncoding pins the path that only exists here: the
// part pipe hands over the plaintext once, so an encoding that misses min_ratio
// is decoded back out of its own buffer rather than re-read from the backend.
// The object must land verbatim and the row must not claim an algorithm.
func TestComplete_DiscardsUselessEncoding(t *testing.T) {
	t.Parallel()
	bodies := [][]byte{
		incompressiblePart(t, mpChunk+700),
		incompressiblePart(t, mpChunk),
	}
	var want []byte
	for _, b := range bodies {
		want = append(want, b...)
	}

	codec := &countingCodec{inner: newMPCodec(t)}
	res := completeWithParts(t, &fleetOpts{Codec: codec, Compression: mpCompressionOn()}, bodies)

	// Without both counts this test cannot tell "encoded, then discarded" from
	// "never encoded at all", and either would leave the bytes verbatim.
	if codec.encodes != 1 {
		t.Errorf("encoded %d times, want 1", codec.encodes)
	}
	if codec.decodes != 1 {
		t.Errorf("decoded %d times, want 1; the plaintext came from somewhere else", codec.decodes)
	}
	if !bytes.Equal(res.stored, want) {
		t.Error("an incompressible assembly was not stored verbatim")
	}
	if res.rec.Form != nil && res.rec.Form.CompressionAlgorithm != "" {
		t.Errorf("CompressionAlgorithm = %q, want empty for an assembly stored raw", res.rec.Form.CompressionAlgorithm)
	}
	if res.rec.Size != int64(len(want)) {
		t.Errorf("recorded size %d, want the %d bytes the client uploaded", res.rec.Size, len(want))
	}
}

// TestComplete_BelowMinSizeStaysVerbatim checks the size floor applies to the
// assembled object rather than to any single part.
func TestComplete_BelowMinSizeStaysVerbatim(t *testing.T) {
	t.Parallel()
	cfg := mpCompressionOn()
	cfg.MinSize = 1 << 20
	bodies := [][]byte{compressiblePart(512), compressiblePart(512)}

	res := completeWithParts(t, &fleetOpts{Codec: newMPCodec(t), Compression: cfg}, bodies)

	if len(res.stored) != 1024 {
		t.Errorf("stored %d bytes, want the 1024 uploaded", len(res.stored))
	}
	if res.rec.Form != nil && res.rec.Form.CompressionAlgorithm != "" {
		t.Errorf("CompressionAlgorithm = %q, want empty below min_size", res.rec.Form.CompressionAlgorithm)
	}
}

// TestComplete_DisabledLeavesBytesVerbatim checks a wired codec does nothing
// while the config says compression is off, so the feature stays opt-in on this
// path too.
func TestComplete_DisabledLeavesBytesVerbatim(t *testing.T) {
	t.Parallel()
	bodies := [][]byte{compressiblePart(mpChunk), compressiblePart(mpChunk)}

	res := completeWithParts(t, &fleetOpts{
		Codec:       newMPCodec(t),
		Compression: config.CompressionConfig{},
	}, bodies)

	if len(res.stored) != mpChunk*2 {
		t.Errorf("stored %d bytes, want %d; compression ran while disabled", len(res.stored), mpChunk*2)
	}
	if res.rec.Form != nil && res.rec.Form.CompressionAlgorithm != "" {
		t.Errorf("CompressionAlgorithm = %q, want empty", res.rec.Form.CompressionAlgorithm)
	}
}

// failingCodec fails every encode, standing in for a mid-assembly codec error
// the real codec cannot be made to produce.
type failingCodec struct{ err error }

// Compress implements Codec.
func (f failingCodec) Compress(_ io.Writer, _ io.Reader) (int64, error) { return 0, f.err }

// Decompress implements Codec.
func (f failingCodec) Decompress(_ io.ReadSeeker) (io.ReadCloser, error) { return nil, f.err }

// TestComplete_EncodeFailureLeavesUploadRetryable checks the ordering contract
// holds for a compression failure the same as for any other: the completion
// fails before anything is committed, and the parts survive so the client can
// retry.
func TestComplete_EncodeFailureLeavesUploadRetryable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	be := backendtest.NewInMemory()
	body := compressiblePart(mpChunk)
	partKey := multipartPartKey(mpUploadID, 1)
	if _, err := be.PutObject(ctx, partKey, bytes.NewReader(body), int64(len(body)),
		"application/octet-stream", nil); err != nil {
		t.Fatalf("seed part: %v", err)
	}

	store, calls := completeStoreSetup(t,
		&core.MultipartUpload{
			UploadID: mpUploadID, ObjectKey: mpObjectKey,
			BackendName: "b1", ContentType: "application/octet-stream",
		}, []core.MultipartPart{{PartNumber: 1, SizeBytes: int64(len(body))}}, nil)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, &fleetOpts{
		Codec:       failingCodec{err: errors.New("encoder exploded")},
		Compression: mpCompressionOn(),
	})

	_, err := mgr.CompleteMultipartUpload(ctx, "multi", "zipped", mpUploadID, partsOf(1))
	if err == nil {
		t.Fatal("expected the completion to fail when the encoder does")
	}
	if be.Has(mpObjectKey) {
		t.Error("assembled object was written despite the encode failing")
	}
	if !be.Has(partKey) {
		t.Error("part was deleted after a failed completion; the retry has nothing to read")
	}
	if len(calls.recordObject) != 0 {
		t.Errorf("recorded %d rows for a failed completion, want 0", len(calls.recordObject))
	}
}
