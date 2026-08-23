// -------------------------------------------------------------------------------
// Ops - Compression Operation Tests
//
// Author: Alex Freidah
//
// The bulk passes rewrite objects in place, so what matters is that each one
// ends with bytes and a row that agree: an encoding described as an encoding,
// at the size it actually occupies, with the logical size the client's object
// still has. These tests drive a real codec over a fake backend and assert on
// the update the store was handed.
//
// The skip cases get equal weight. A pass that declines most of a fleet is the
// expected outcome on media or archives, and reporting those as failures would
// make a healthy run look broken.
// -------------------------------------------------------------------------------

package ops

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"sync/atomic"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/ops/opstest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// compChunk is the chunk size these tests encode at, small enough that a modest
// fixture still crosses frame boundaries.
const compChunk = compression.MinChunkSize

// testCodec builds a codec and closes it with the test.
func testCodec(t *testing.T) *compression.Codec {
	t.Helper()
	c, err := compression.NewCodec(compression.DefaultLevel, compChunk)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// compressibleBytes returns n bytes zstd can shrink.
func compressibleBytes(n int) []byte {
	out := make([]byte, 0, n)
	line := []byte("the quick brown fox jumps over the lazy dog 0123456789\n")
	for len(out) < n {
		out = append(out, line...)
	}
	return out[:n]
}

// oneRowStore serves a single rewritable location once and captures the update
// the pass records for it.
type oneRowStore struct {
	row      core.RewritableLocation
	served   atomic.Bool
	update   core.CompressedUpdate
	previous int64
	marked   atomic.Bool
}

// ListUncompressedLocations serves the row once, then reports the end of the
// listing so the pass terminates.
func (s *oneRowStore) ListUncompressedLocations(_ context.Context, _, _ int) ([]core.RewritableLocation, error) {
	return s.serveOnce(), nil
}

// ListCompressedLocations serves the same row for the reverse direction.
func (s *oneRowStore) ListCompressedLocations(_ context.Context, _, _ int) ([]core.RewritableLocation, error) {
	return s.serveOnce(), nil
}

// serveOnce yields the row on the first page and nothing after it.
func (s *oneRowStore) serveOnce() []core.RewritableLocation {
	if s.served.Swap(true) {
		return nil
	}
	return []core.RewritableLocation{s.row}
}

// MarkObjectCompressed captures what the pass decided the copy now is.
func (s *oneRowStore) MarkObjectCompressed(_ context.Context, u *core.CompressedUpdate, previousSize int64) error {
	s.update, s.previous = *u, previousSize
	s.marked.Store(true)
	return nil
}

// newCompression builds the service over a fake backend holding payload.
func newCompression(t *testing.T, store CompressionStore, cfg config.CompressionConfig, payload []byte) (*Compression, *fakeBackend) {
	t.Helper()
	ctrl := gomock.NewController(t)
	be := &fakeBackend{payload: payload}

	runtime := opstest.NewMockRuntimeOps(ctrl)
	runtime.EXPECT().GetBackend(gomock.Any()).Return(be, nil).AnyTimes()
	backendOps := opstest.NewMockBackendOps(ctrl)
	backendOps.EXPECT().RecordUsage(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()

	return NewCompression(&CompressionDeps{
		Codec:      testCodec(t),
		Config:     cfg,
		Store:      store,
		Runtime:    runtime,
		BackendOps: backendOps,
	}), be
}

// compressionOn returns an enabled config with the production defaults for the
// two thresholds.
func compressionOn() config.CompressionConfig {
	return config.CompressionConfig{
		Enabled:   true,
		Level:     "default",
		ChunkSize: compChunk,
		MinRatio:  config.DefaultCompressionMinRatio,
	}
}

// TestCompressExisting_RewritesAndRecords is the headline: a verbatim object is
// stored as an encoding, and the row it leaves behind says so, at the size that
// now occupies the backend and with the size the client's object still has.
func TestCompressExisting_RewritesAndRecords(t *testing.T) {
	t.Parallel()
	payload := compressibleBytes(compChunk * 2)
	store := &oneRowStore{row: core.RewritableLocation{
		ObjectKey: "bucket/file.txt", BackendName: "backend-a", SizeBytes: int64(len(payload)),
	}}
	svc, be := newCompression(t, store, compressionOn(), payload)

	res, err := svc.CompressExisting(context.Background())
	if err != nil {
		t.Fatalf("CompressExisting: %v", err)
	}
	if res.Total != 1 || res.Succeeded != 1 || res.Skipped != 0 || res.Failed != 0 {
		t.Fatalf("counts = %+v, want one succeeded", res)
	}
	if !store.marked.Load() {
		t.Fatal("the metadata update did not run")
	}
	if be.puts.Load() != 1 {
		t.Errorf("PutObject calls = %d, want 1", be.puts.Load())
	}

	got := store.update
	if got.Algorithm != compression.Algorithm {
		t.Errorf("Algorithm = %q, want %q", got.Algorithm, compression.Algorithm)
	}
	if got.Level != "default" {
		t.Errorf("Level = %q, want %q", got.Level, "default")
	}
	if got.FormatVersion != compression.FormatVersion {
		t.Errorf("FormatVersion = %d, want %d", got.FormatVersion, compression.FormatVersion)
	}
	if got.LogicalSize != int64(len(payload)) {
		t.Errorf("LogicalSize = %d, want %d", got.LogicalSize, len(payload))
	}
	if got.SizeBytes >= int64(len(payload)) {
		t.Errorf("SizeBytes = %d, want fewer than the %d uploaded", got.SizeBytes, len(payload))
	}
	if store.previous != int64(len(payload)) {
		t.Errorf("previous size = %d, want %d so quota moves by the difference",
			store.previous, len(payload))
	}
}

// TestCompressExisting_SkipsIncompressible checks the entropy floor. The object
// is downloaded and encoded, because only the finished encoding answers the
// question, but nothing is written and the row is left alone.
func TestCompressExisting_SkipsIncompressible(t *testing.T) {
	t.Parallel()
	payload := make([]byte, compChunk)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("read random payload: %v", err)
	}
	store := &oneRowStore{row: core.RewritableLocation{
		ObjectKey: "bucket/random.bin", BackendName: "backend-a", SizeBytes: int64(len(payload)),
	}}
	cfg := compressionOn()
	svc, be := newCompression(t, store, cfg, payload)

	res, err := svc.CompressExisting(context.Background())
	if err != nil {
		t.Fatalf("CompressExisting: %v", err)
	}
	if res.Skipped != 1 || res.Succeeded != 0 || res.Failed != 0 {
		t.Errorf("counts = %+v, want one skipped", res)
	}
	if be.puts.Load() != 0 {
		t.Errorf("PutObject calls = %d, want 0; nothing should be written", be.puts.Load())
	}
	if store.marked.Load() {
		t.Error("the row was rewritten for an object that was not compressed")
	}
}

// TestCompressExisting_SkipsBelowMinSize checks the size floor is answered from
// the row: an object under it costs no backend read at all.
func TestCompressExisting_SkipsBelowMinSize(t *testing.T) {
	t.Parallel()
	payload := compressibleBytes(512)
	store := &oneRowStore{row: core.RewritableLocation{
		ObjectKey: "bucket/small.txt", BackendName: "backend-a", SizeBytes: int64(len(payload)),
	}}
	cfg := compressionOn()
	cfg.MinSize = 4096
	svc, be := newCompression(t, store, cfg, payload)

	res, err := svc.CompressExisting(context.Background())
	if err != nil {
		t.Fatalf("CompressExisting: %v", err)
	}
	if res.Skipped != 1 || res.Succeeded != 0 {
		t.Errorf("counts = %+v, want one skipped", res)
	}
	if be.gets.Load() != 0 {
		t.Errorf("GetObject calls = %d, want 0; the row alone answers the size floor", be.gets.Load())
	}
}

// TestDecompressExisting_RestoresStoredBytes checks the reverse direction: the
// encoding is decoded back to what the client wrote, and the row stops claiming
// an algorithm so nothing tries to decode it again.
func TestDecompressExisting_RestoresStoredBytes(t *testing.T) {
	t.Parallel()
	plain := compressibleBytes(compChunk * 2)
	codec := testCodec(t)
	var encoded bytes.Buffer
	if _, err := codec.Compress(&encoded, bytes.NewReader(plain)); err != nil {
		t.Fatalf("Compress: %v", err)
	}

	store := &oneRowStore{row: core.RewritableLocation{
		ObjectKey:                "bucket/file.txt",
		BackendName:              "backend-a",
		SizeBytes:                int64(encoded.Len()),
		CompressionAlgorithm:     compression.Algorithm,
		CompressionFormatVersion: compression.FormatVersion,
		LogicalSize:              int64(len(plain)),
	}}
	svc, be := newCompression(t, store, compressionOn(), encoded.Bytes())

	res, err := svc.DecompressExisting(context.Background())
	if err != nil {
		t.Fatalf("DecompressExisting: %v", err)
	}
	if res.Succeeded != 1 || res.Failed != 0 {
		t.Fatalf("counts = %+v, want one succeeded", res)
	}
	if got := store.update.Algorithm; got != "" {
		t.Errorf("Algorithm = %q, want empty so the row no longer claims an encoding", got)
	}
	if store.update.SizeBytes != int64(len(plain)) {
		t.Errorf("SizeBytes = %d, want the %d decoded bytes", store.update.SizeBytes, len(plain))
	}
	if !bytes.Equal(be.lastPut, plain) {
		t.Error("the object written back is not what was originally encoded")
	}
}

// TestCompressExisting_WithoutCodec checks a deployment with no codec reports
// that rather than rewriting anything.
func TestCompressExisting_WithoutCodec(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	svc := NewCompression(&CompressionDeps{
		Store:      &oneRowStore{},
		Runtime:    opstest.NewMockRuntimeOps(ctrl),
		BackendOps: opstest.NewMockBackendOps(ctrl),
	})

	if _, err := svc.CompressExisting(context.Background()); err != ErrCompressionUnavailable {
		t.Errorf("err = %v, want ErrCompressionUnavailable", err)
	}
	if _, err := svc.DecompressExisting(context.Background()); err != ErrCompressionUnavailable {
		t.Errorf("err = %v, want ErrCompressionUnavailable", err)
	}
}

// TestRewriteRow_LogicalSizeOfSource pins which column names the bytes a
// transform will read, which differs by how the copy is stored.
func TestRewriteRow_LogicalSizeOfSource(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		row  core.RewritableLocation
		want int64
	}{
		{"verbatim", core.RewritableLocation{SizeBytes: 100}, 100},
		{"encrypted", core.RewritableLocation{SizeBytes: 140, Encrypted: true, PlaintextSize: 100}, 100},
		{
			"encoded",
			core.RewritableLocation{SizeBytes: 40, CompressionAlgorithm: "zstd", LogicalSize: 100},
			100,
		},
		{
			"encoded and encrypted",
			core.RewritableLocation{
				SizeBytes: 60, Encrypted: true, PlaintextSize: 40,
				CompressionAlgorithm: "zstd", LogicalSize: 100,
			},
			100,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := &rewriteRow{tt.row}
			if got := r.LogicalSizeOfSource(); got != tt.want {
				t.Errorf("LogicalSizeOfSource() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestCompressExisting_EncryptedObject covers the hardest path in either pass:
// compression sits inside encryption, so an encrypted copy has to be decrypted,
// encoded, and re-encrypted. The row it leaves behind carries three sizes that
// all mean different things, and getting any of them wrong makes the object
// unreadable.
func TestCompressExisting_EncryptedObject(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t)
	plain := compressibleBytes(compChunk * 2)

	// Seed the backend with what an encrypted PUT of that object would hold.
	res, err := enc.Encrypt(context.Background(), bytes.NewReader(plain), int64(len(plain)))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	ciphertext, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}

	store := &oneRowStore{row: core.RewritableLocation{
		ObjectKey:     "bucket/secret.txt",
		BackendName:   "backend-a",
		SizeBytes:     int64(len(ciphertext)),
		Encrypted:     true,
		EncryptionKey: encryption.PackKeyData(res.BaseNonce, res.WrappedDEK),
		KeyID:         res.KeyID,
		PlaintextSize: int64(len(plain)),
	}}

	ctrl := gomock.NewController(t)
	be := &fakeBackend{payload: ciphertext}
	runtime := opstest.NewMockRuntimeOps(ctrl)
	runtime.EXPECT().GetBackend(gomock.Any()).Return(be, nil).AnyTimes()
	backendOps := opstest.NewMockBackendOps(ctrl)
	backendOps.EXPECT().RecordUsage(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
	codec := testCodec(t)

	svc := NewCompression(&CompressionDeps{
		Codec:      codec,
		Config:     compressionOn(),
		Encryptor:  enc,
		Store:      store,
		Runtime:    runtime,
		BackendOps: backendOps,
	})

	got, err := svc.CompressExisting(context.Background())
	if err != nil {
		t.Fatalf("CompressExisting: %v", err)
	}
	if got.Succeeded != 1 || got.Failed != 0 {
		t.Fatalf("counts = %+v, want one succeeded", got)
	}

	u := store.update
	if u.LogicalSize != int64(len(plain)) {
		t.Errorf("LogicalSize = %d, want the %d bytes the client wrote", u.LogicalSize, len(plain))
	}
	if u.PlaintextSize >= u.LogicalSize {
		t.Errorf("PlaintextSize = %d, want the encoded stream, smaller than the %d byte object",
			u.PlaintextSize, u.LogicalSize)
	}
	if u.SizeBytes <= u.PlaintextSize {
		t.Errorf("SizeBytes = %d, want more than the %d byte encoding it encrypts",
			u.SizeBytes, u.PlaintextSize)
	}
	// The row has to carry the new envelope. Re-encrypting minted a new base
	// nonce and wrapped key, so the old ones would describe bytes nothing can
	// decrypt - which is what the round trip below actually proves.
	if len(u.EncryptionKey) == 0 || u.KeyID == "" {
		t.Fatalf("update carries no envelope: key=%d bytes, keyID=%q", len(u.EncryptionKey), u.KeyID)
	}
	if bytes.Equal(u.EncryptionKey, store.row.EncryptionKey) {
		t.Error("the row kept the old packed key after a re-encryption")
	}

	// The written object must still be readable: decrypt it, then decode it,
	// and the client's bytes come back.
	decrypted, _, err := enc.DecryptStored(context.Background(), bytes.NewReader(be.lastPut),
		u.EncryptionKey, u.KeyID, u.PlaintextSize, nil)
	if err != nil {
		t.Fatalf("DecryptStored: %v", err)
	}
	decoded, err := codec.DecompressStream(decrypted)
	if err != nil {
		t.Fatalf("DecompressStream: %v", err)
	}
	defer func() { _ = decoded.Close() }()
	roundTripped, err := io.ReadAll(decoded)
	if err != nil {
		t.Fatalf("read decoded: %v", err)
	}
	if !bytes.Equal(roundTripped, plain) {
		t.Error("the rewritten object did not decrypt and decode back to the original")
	}
}

// TestCompressExisting_EncryptedWithoutEncryptor checks a copy the pass cannot
// unwrap is failed rather than rewritten. Encoding ciphertext as though it were
// the object would store bytes that decode to nothing.
func TestCompressExisting_EncryptedWithoutEncryptor(t *testing.T) {
	t.Parallel()
	store := &oneRowStore{row: core.RewritableLocation{
		ObjectKey: "bucket/secret", BackendName: "backend-a", SizeBytes: 4096,
		Encrypted: true, PlaintextSize: 4000,
	}}
	svc, be := newCompression(t, store, compressionOn(), compressibleBytes(4096))

	res, err := svc.CompressExisting(context.Background())
	if err != nil {
		t.Fatalf("CompressExisting: %v", err)
	}
	if res.Failed != 1 || res.Succeeded != 0 {
		t.Errorf("counts = %+v, want one failed", res)
	}
	if be.puts.Load() != 0 {
		t.Errorf("PutObject calls = %d, want 0", be.puts.Load())
	}
}

// TestDecompressExisting_UndecodableBytes checks a row that claims an encoding
// over bytes that are not one is failed, and the object is left alone.
func TestDecompressExisting_UndecodableBytes(t *testing.T) {
	t.Parallel()
	store := &oneRowStore{row: core.RewritableLocation{
		ObjectKey: "bucket/claims-encoding", BackendName: "backend-a", SizeBytes: 64,
		CompressionAlgorithm: compression.Algorithm, LogicalSize: 4096,
	}}
	svc, _ := newCompression(t, store, compressionOn(), []byte("not a zstd stream at all"))

	res, err := svc.DecompressExisting(context.Background())
	if err != nil {
		t.Fatalf("DecompressExisting: %v", err)
	}
	if res.Failed != 1 || res.Succeeded != 0 {
		t.Errorf("counts = %+v, want one failed", res)
	}
	if store.marked.Load() {
		t.Error("the row was rewritten for an object that could not be decoded")
	}
}

// failingListStore fails the listing, which is the one error that ends a pass
// rather than being counted against a single object.
type failingListStore struct{ err error }

// ListUncompressedLocations fails.
func (f failingListStore) ListUncompressedLocations(context.Context, int, int) ([]core.RewritableLocation, error) {
	return nil, f.err
}

// ListCompressedLocations fails.
func (f failingListStore) ListCompressedLocations(context.Context, int, int) ([]core.RewritableLocation, error) {
	return nil, f.err
}

// MarkObjectCompressed is never reached when the listing fails.
func (f failingListStore) MarkObjectCompressed(context.Context, *core.CompressedUpdate, int64) error {
	return nil
}

// TestCompressExisting_ListingFailureStopsPass checks a listing error ends the
// run and surfaces, rather than being counted against an object. The counts
// gathered so far come back with it so a caller can report partial progress.
func TestCompressExisting_ListingFailureStopsPass(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("database is down")
	svc, _ := newCompression(t, failingListStore{err: wantErr}, compressionOn(), nil)

	res, err := svc.CompressExisting(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want the listing error", err)
	}
	if res.Total != 0 {
		t.Errorf("Total = %d, want 0; nothing was listed to process", res.Total)
	}

	if _, err := svc.DecompressExisting(context.Background()); !errors.Is(err, wantErr) {
		t.Errorf("decompress err = %v, want the listing error", err)
	}
}

// failingCodec fails whichever half a test drives, standing in for a codec
// error the concrete codec cannot be made to produce.
type failingCodec struct{ err error }

// Compress fails.
func (f failingCodec) Compress(io.Writer, io.Reader) (int64, error) { return 0, f.err }

// DecompressStream fails.
func (f failingCodec) DecompressStream(io.Reader) (io.ReadCloser, error) { return nil, f.err }

// TestCompressExisting_EncodeFailureCountsAgainstObject checks a codec error is
// charged to the one object and the pass carries on, rather than ending the run.
// One unreadable object must not stop a fleet-wide rewrite.
func TestCompressExisting_EncodeFailureCountsAgainstObject(t *testing.T) {
	t.Parallel()
	store := &oneRowStore{row: core.RewritableLocation{
		ObjectKey: "bucket/file.txt", BackendName: "backend-a", SizeBytes: 4096,
	}}
	ctrl := gomock.NewController(t)
	be := &fakeBackend{payload: compressibleBytes(4096)}
	runtime := opstest.NewMockRuntimeOps(ctrl)
	runtime.EXPECT().GetBackend(gomock.Any()).Return(be, nil).AnyTimes()
	backendOps := opstest.NewMockBackendOps(ctrl)
	backendOps.EXPECT().RecordUsage(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()

	svc := NewCompression(&CompressionDeps{
		Codec:      failingCodec{err: errors.New("encoder exploded")},
		Config:     compressionOn(),
		Store:      store,
		Runtime:    runtime,
		BackendOps: backendOps,
	})

	res, err := svc.CompressExisting(context.Background())
	if err != nil {
		t.Fatalf("CompressExisting: %v", err)
	}
	if res.Failed != 1 || res.Succeeded != 0 || res.Skipped != 0 {
		t.Errorf("counts = %+v, want one failed", res)
	}
	if be.puts.Load() != 0 {
		t.Errorf("PutObject calls = %d, want 0", be.puts.Load())
	}
	if store.marked.Load() {
		t.Error("the row was rewritten for an object that never encoded")
	}
}
