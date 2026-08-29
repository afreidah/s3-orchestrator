// -------------------------------------------------------------------------------
// COPY Compression Tests
//
// Author: Alex Freidah
//
// A copy moves the stored bytes verbatim, so the destination is only readable if
// the row written for it repeats what the source row said about those bytes.
// These tests pin that carry across both copy paths, and pin that the bytes on
// the destination are the source's own rather than a re-encoding of them.
// -------------------------------------------------------------------------------

package object

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

const (
	// copySrcKey and copyDstKey are the two ends of every copy here.
	copySrcKey = "compressed-src"
	copyDstKey = "compressed-dst"
	// copySrcBackend is the single backend both keys live on, so the same
	// fixture drives the native fast path and the materialized fallback.
	copySrcBackend = "b1"
)

// copyResult is what one CopyObject through the fleet leaves behind: the
// backend holding both keys, and the row recorded for the destination.
type copyResult struct {
	be  *backendtest.InMemory
	rec objRecordCall
}

// dest returns the bytes that landed on the destination key.
func (r *copyResult) dest(t *testing.T) []byte {
	t.Helper()
	obj, ok := r.be.Get(copyDstKey)
	if !ok {
		t.Fatal("destination object not found on the backend")
	}
	return obj.Data
}

// copyThroughFleet seeds one backend with stored, records loc as the source's
// only location, and copies it. The key, backend and size on loc are filled in
// here so a caller states only what the row says about the bytes. native decides
// whether the backend offers server-side copy, which is what selects the fast
// path over the materialized fallback.
func copyThroughFleet(t *testing.T, loc *core.ObjectLocation, stored []byte, native bool) copyResult {
	t.Helper()
	be := backendtest.NewInMemory()
	be.CopyEnabled = native
	if _, err := be.PutObject(context.Background(), copySrcKey,
		bytes.NewReader(stored), int64(len(stored)), "text/plain", nil); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	loc.ObjectKey, loc.BackendName, loc.SizeBytes = copySrcKey, copySrcBackend, int64(len(stored))
	store, calls := copyObjectStore(t, []core.ObjectLocation{*loc}, nil, copySrcBackend, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{copySrcBackend: be}, nil)

	if _, err := mgr.CopyObject(context.Background(), &CopyObjectRequest{SourceKey: copySrcKey, DestKey: copyDstKey}); err != nil {
		t.Fatalf("CopyObject: %v", err)
	}
	if len(calls.recordObject) != 1 {
		t.Fatalf("recorded %d rows, want 1", len(calls.recordObject))
	}
	return copyResult{be: be, rec: calls.recordObject[0]}
}

// encodeForCopy encodes a compressible body and returns the encoded bytes with
// the plaintext they decode back to, plus the location row that describes them
// the way a compressed PUT would have.
func encodeForCopy(t *testing.T, codec *compression.Codec) ([]byte, []byte, *core.ObjectLocation) {
	t.Helper()
	plain := compressibleBody(putCompressChunk * 3)
	var encoded bytes.Buffer
	if _, err := codec.Compress(&encoded, bytes.NewReader(plain)); err != nil {
		t.Fatalf("Compress: %v", err)
	}
	return encoded.Bytes(), plain, &core.ObjectLocation{
		CompressionAlgorithm:     compression.Algorithm,
		CompressionLevel:         "default",
		CompressionFormatVersion: compression.FormatVersion,
		LogicalSize:              int64(len(plain)),
	}
}

// assertCompressionCarried checks that every column a decoder needs survived
// the copy. A row missing any of them describes bytes nothing can read.
func assertCompressionCarried(t *testing.T, form *core.StoredForm, logicalSize int) {
	t.Helper()
	if form == nil {
		t.Fatal("no representation metadata recorded for a copy of a compressed object")
	}
	if form.CompressionAlgorithm != compression.Algorithm {
		t.Errorf("CompressionAlgorithm = %q, want %q", form.CompressionAlgorithm, compression.Algorithm)
	}
	if form.CompressionLevel != "default" {
		t.Errorf("CompressionLevel = %q, want %q", form.CompressionLevel, "default")
	}
	if form.CompressionFormatVersion != compression.FormatVersion {
		t.Errorf("CompressionFormatVersion = %d, want %d", form.CompressionFormatVersion, compression.FormatVersion)
	}
	if form.LogicalSize != int64(logicalSize) {
		t.Errorf("LogicalSize = %d, want %d", form.LogicalSize, logicalSize)
	}
}

// TestCopyObject_MaterializedCarriesCompression checks the stream-through path:
// the destination holds the source's own encoded bytes, the row describes them,
// and the size recorded is what landed rather than what the client wrote.
func TestCopyObject_MaterializedCarriesCompression(t *testing.T) {
	t.Parallel()
	codec := newPutCodec(t)
	encoded, plain, loc := encodeForCopy(t, codec)

	res := copyThroughFleet(t, loc, encoded, false)

	assertCompressionCarried(t, res.rec.Form, len(plain))
	if !bytes.Equal(res.dest(t), encoded) {
		t.Error("destination bytes differ from the source's stored bytes; the copy was not verbatim")
	}
	if res.rec.Size != int64(len(encoded)) {
		t.Errorf("recorded size %d, want the %d stored bytes", res.rec.Size, len(encoded))
	}
}

// TestCopyObject_NativeCarriesCompression checks the same carry on the
// server-side fast path, which records the destination row without ever holding
// the bytes and so has its own chance to lose the description of them.
func TestCopyObject_NativeCarriesCompression(t *testing.T) {
	t.Parallel()
	codec := newPutCodec(t)
	encoded, plain, loc := encodeForCopy(t, codec)

	res := copyThroughFleet(t, loc, encoded, true)

	if got := res.be.CopyCallCount(); got != 1 {
		t.Fatalf("native copyCalls = %d, want 1", got)
	}
	assertCompressionCarried(t, res.rec.Form, len(plain))
}

// TestCopyObject_DestinationDecodes is the end of the contract the columns exist
// for: the bytes on the destination decode back to the object the client wrote.
func TestCopyObject_DestinationDecodes(t *testing.T) {
	t.Parallel()
	codec := newPutCodec(t)
	encoded, plain, loc := encodeForCopy(t, codec)

	res := copyThroughFleet(t, loc, encoded, false)

	r, err := codec.Decompress(bytes.NewReader(res.dest(t)))
	if err != nil {
		t.Fatalf("Decompress the destination: %v", err)
	}
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read decompressed: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Error("the copied object did not decode back to what was written")
	}
}

// TestCopyObject_CarriesContentHashWithoutEncryption pins the hash on a copy of
// a plaintext object. The hash covers the bytes the client wrote, which a
// verbatim copy does not change, so dropping it would leave the scrubber with a
// copy it cannot verify.
func TestCopyObject_CarriesContentHashWithoutEncryption(t *testing.T) {
	t.Parallel()
	const digest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	body := []byte("copy-me")

	res := copyThroughFleet(t, &core.ObjectLocation{ContentHash: digest}, body, false)

	if res.rec.Form == nil {
		t.Fatal("no representation metadata recorded for a hashed object")
	}
	if res.rec.Form.ContentHash != digest {
		t.Errorf("ContentHash = %q, want %q", res.rec.Form.ContentHash, digest)
	}
}

// TestCopyObject_VerbatimSourceRecordsNoForm checks the other end: a source with
// nothing to say about its bytes records no form at all, rather than an empty
// one a later reader would have to interpret.
func TestCopyObject_VerbatimSourceRecordsNoForm(t *testing.T) {
	t.Parallel()
	res := copyThroughFleet(t, &core.ObjectLocation{}, []byte("copy-me"), false)

	if res.rec.Form != nil {
		t.Errorf("recorded form %+v, want none for a verbatim unhashed object", res.rec.Form)
	}
}
