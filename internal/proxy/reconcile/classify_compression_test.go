// -------------------------------------------------------------------------------
// Compressed Import Classification Tests
//
// Author: Alex Freidah
//
// A rediscovered object is recognised as one this orchestrator encoded by its
// seek table, not by the zstd frame magic: the stored form is a standard
// Zstandard stream on purpose, so the magic alone cannot separate an object
// written here from a .zst file a client uploaded. These tests pin both answers,
// and pin that a recognised object's logical size comes back with it - without
// that number nothing can size a response for it.
// -------------------------------------------------------------------------------

package reconcile

import (
	"bytes"
	"context"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// classifyChunk is small enough that a modest fixture crosses frame boundaries.
const classifyChunk = compression.MinChunkSize

// newClassifyCodec builds a codec and closes it with the test.
func newClassifyCodec(t *testing.T) *compression.Codec {
	t.Helper()
	c, err := compression.NewCodec(compression.DefaultLevel, classifyChunk)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// compressibleText returns n bytes zstd can shrink.
func compressibleText(n int) []byte {
	out := make([]byte, 0, n)
	line := []byte("the quick brown fox jumps over the lazy dog 0123456789\n")
	for len(out) < n {
		out = append(out, line...)
	}
	return out[:n]
}

// classifyWithCodec runs ClassifyImport over a seeded object with a codec wired,
// which is what production does.
func classifyWithCodec(t *testing.T, be *backendtest.InMemory, codec StoredInspector, key string) *core.StoredForm {
	t.Helper()
	obj, ok := be.Get(key)
	if !ok {
		t.Fatalf("no object %q seeded", key)
	}
	form, err := ClassifyImport(context.Background(), ClassifyDeps{
		Backend: be,
		Stores:  siblingStub{},
		Codec:   codec,
		Source:  "test",
	}, "b1", key, int64(len(obj.Data)))
	if err != nil {
		t.Fatalf("ClassifyImport: %v", err)
	}
	return form
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestClassifyImport_RecognisesCompressedObject is the headline: an object this
// codec wrote, rediscovered with no ledger row, is imported as compressed and
// carries the logical size its seek table declares. Recorded as verbatim it
// would be served back as chunked zstd at the wrong size.
func TestClassifyImport_RecognisesCompressedObject(t *testing.T) {
	t.Parallel()
	codec := newClassifyCodec(t)
	be := backendtest.NewInMemory()
	plain := compressibleText(classifyChunk * 3)

	var stored bytes.Buffer
	if _, err := codec.Compress(&stored, bytes.NewReader(plain)); err != nil {
		t.Fatalf("Compress: %v", err)
	}
	be.Objects["vb/encoded"] = backendtest.Object{Data: stored.Bytes()}

	form := classifyWithCodec(t, be, codec, "vb/encoded")

	if form == nil {
		t.Fatal("no representation metadata recorded for a compressed object")
	}
	if form.CompressionAlgorithm != compression.Algorithm {
		t.Errorf("CompressionAlgorithm = %q, want %q", form.CompressionAlgorithm, compression.Algorithm)
	}
	if form.CompressionFormatVersion != compression.FormatVersion {
		t.Errorf("CompressionFormatVersion = %d, want %d", form.CompressionFormatVersion, compression.FormatVersion)
	}
	if form.LogicalSize != int64(len(plain)) {
		t.Errorf("LogicalSize = %d, want %d", form.LogicalSize, len(plain))
	}
	if form.Encrypted {
		t.Error("a compressed object with no envelope must not be recorded as encrypted")
	}
}

// TestClassifyImport_PlainZstdIsNotOurs is the other half of the recognition,
// and the reason it is the seek table that decides. A client's own zstd upload
// has the same frame magic as an object written here, and decoding it on read
// would hand that client back something they never uploaded.
func TestClassifyImport_PlainZstdIsNotOurs(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	// A plain zstd stream: frame magic, no trailing seek table.
	be.Objects["vb/clients.zst"] = backendtest.Object{
		Data: append([]byte{0x28, 0xB5, 0x2F, 0xFD}, compressibleText(4096)...),
	}

	form := classifyWithCodec(t, be, newClassifyCodec(t), "vb/clients.zst")

	if form != nil && form.CompressionAlgorithm != "" {
		t.Errorf("CompressionAlgorithm = %q, want empty: a .zst upload is not ours to decode",
			form.CompressionAlgorithm)
	}
}

// TestClassifyImport_PlaintextStaysVerbatim checks ordinary bytes are untouched
// by the added inspection.
func TestClassifyImport_PlaintextStaysVerbatim(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.Objects["vb/plain.txt"] = backendtest.Object{Data: []byte("just an object")}

	if form := classifyWithCodec(t, be, newClassifyCodec(t), "vb/plain.txt"); form != nil {
		t.Errorf("plaintext recorded %+v, want no metadata", form)
	}
}

// TestClassifyImport_NoCodecImportsVerbatim checks the deployment with no codec
// wired does not guess. Nothing can decode the object there, so recording it as
// compressed would describe bytes that instance cannot serve.
func TestClassifyImport_NoCodecImportsVerbatim(t *testing.T) {
	t.Parallel()
	codec := newClassifyCodec(t)
	be := backendtest.NewInMemory()

	var stored bytes.Buffer
	if _, err := codec.Compress(&stored, bytes.NewReader(compressibleText(classifyChunk))); err != nil {
		t.Fatalf("Compress: %v", err)
	}
	be.Objects["vb/encoded"] = backendtest.Object{Data: stored.Bytes()}

	if form := classifyWithCodec(t, be, nil, "vb/encoded"); form != nil {
		t.Errorf("recorded %+v with no codec wired, want no metadata", form)
	}
}

// countingBackend counts the ranged reads a classification spends, which is the
// cost this inspection adds to a reconcile walk.
type countingBackend struct {
	*backendtest.InMemory
	gets int
}

// GetObject forwards and tallies.
func (c *countingBackend) GetObject(ctx context.Context, key, rangeHeader string) (*backend.GetObjectResult, error) {
	c.gets++
	return c.InMemory.GetObject(ctx, key, rangeHeader)
}

// TestClassifyImport_ReadCost pins what the walk pays per object. The header
// read was already there; the tail read is what recognition added, and the frame
// magic keeps it off everything that could not be an encoding. A backend of
// ordinary objects therefore costs exactly what it did before.
func TestClassifyImport_ReadCost(t *testing.T) {
	t.Parallel()
	codec := newClassifyCodec(t)
	inner := backendtest.NewInMemory()

	var stored bytes.Buffer
	if _, err := codec.Compress(&stored, bytes.NewReader(compressibleText(classifyChunk))); err != nil {
		t.Fatalf("Compress: %v", err)
	}
	inner.Objects["vb/encoded"] = backendtest.Object{Data: stored.Bytes()}
	inner.Objects["vb/plain.txt"] = backendtest.Object{Data: compressibleText(4096)}

	tests := []struct {
		name string
		key  string
		want int
	}{
		{"plaintext costs the header read alone", "vb/plain.txt", 1},
		{"an encoding costs one more to read its seek table", "vb/encoded", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			be := &countingBackend{InMemory: inner}
			obj, _ := inner.Get(tt.key)
			if _, err := ClassifyImport(context.Background(), ClassifyDeps{
				Backend: be,
				Stores:  siblingStub{},
				Codec:   codec,
				Source:  "test",
			}, "b1", tt.key, int64(len(obj.Data))); err != nil {
				t.Fatalf("ClassifyImport: %v", err)
			}
			if be.gets != tt.want {
				t.Errorf("ranged reads = %d, want %d", be.gets, tt.want)
			}
		})
	}
}

// TestClassifyImport_EncryptedCompressedAdoptsSibling covers the case the bytes
// cannot answer: compression runs before encryption, so an encrypted object's
// encoding is inside the ciphertext and invisible from outside. The surviving
// sibling row is what carries it, and it has to carry the compression columns
// along with the key.
func TestClassifyImport_EncryptedCompressedAdoptsSibling(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)
	be := backendtest.NewInMemory()

	sib := encryptInto(t, enc, be, "vb/both", "the object body")
	sib.CompressionAlgorithm = compression.Algorithm
	sib.CompressionLevel = "default"
	sib.CompressionFormatVersion = compression.FormatVersion
	sib.LogicalSize = 4096

	form, err := classify(t, be, "vb/both", []core.ObjectLocation{sib})
	if err != nil {
		t.Fatalf("ClassifyImport: %v", err)
	}
	if form == nil {
		t.Fatal("no metadata adopted from the surviving sibling")
	}
	if form.CompressionAlgorithm != compression.Algorithm {
		t.Errorf("CompressionAlgorithm = %q, want %q", form.CompressionAlgorithm, compression.Algorithm)
	}
	if form.LogicalSize != 4096 {
		t.Errorf("LogicalSize = %d, want 4096", form.LogicalSize)
	}
	if len(form.EncryptionKey) == 0 {
		t.Error("the key was not adopted alongside the compression columns")
	}
}
