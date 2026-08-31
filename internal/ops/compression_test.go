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
	"fmt"
	"io"
	"slices"
	"sync/atomic"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/ops/opstest"
	"github.com/afreidah/s3-orchestrator/internal/progress"
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

// incompressibleBytes returns n bytes zstd cannot shrink, which is what makes a
// pass reach the ratio decision rather than the size one.
func incompressibleBytes(t *testing.T, n int) []byte {
	t.Helper()
	out := make([]byte, n)
	if _, err := rand.Read(out); err != nil {
		t.Fatalf("read random payload: %v", err)
	}
	return out
}

// oneRowStore serves a single rewritable location once and captures the update
// the pass records for it.
type oneRowStore struct {
	row      core.RewritableLocation
	served   atomic.Bool
	update   core.CompressedUpdate
	previous int64
	marked   atomic.Bool
	probes   []core.CompressionProbe
	probeErr error
}

// ListUncompressedLocations serves the row once, then reports the end of the
// listing so the pass terminates.
//
// The size floor is applied here because that is where the real store applies
// it: a copy under it is not a candidate rather than a candidate the pass
// declines, so it never reaches the pass at all.
func (s *oneRowStore) ListUncompressedLocations(_ context.Context, _ int, _ core.Cursor, t core.CompressionThresholds) ([]core.RewritableLocation, error) {
	rows := s.serveOnce()
	if len(rows) == 1 && rows[0].LogicalSize == 0 && rows[0].SizeBytes < t.MinSize {
		return nil, nil
	}
	return rows, nil
}

// RecordCompressionProbe records what a declined copy was measured at, so a
// test can assert the pass wrote down what its encode cost bought.
func (s *oneRowStore) RecordCompressionProbe(_ context.Context, probe *core.CompressionProbe) error {
	s.probes = append(s.probes, *probe)
	return s.probeErr
}

// ListCompressedLocations serves the same row for the reverse direction.
func (s *oneRowStore) ListCompressedLocations(_ context.Context, _ int, _ core.Cursor) ([]core.RewritableLocation, error) {
	return s.serveOnce(), nil
}

// serveOnce yields the row on the first page and nothing after it.
func (s *oneRowStore) serveOnce() []core.RewritableLocation {
	if s.served.Swap(true) {
		return nil
	}
	return []core.RewritableLocation{s.row}
}

// pagedStore serves a fixed set of copies through the cursor the driver pages
// with, so a test can watch a capped run stop partway rather than seeing every
// row arrive on one page.
type pagedStore struct {
	rows      []core.RewritableLocation
	pageSizes []int
	marked    int
}

// ListUncompressedLocations returns the rows after the cursor, capped at limit,
// and records what the driver asked for.
func (s *pagedStore) ListUncompressedLocations(_ context.Context, limit int, after core.Cursor, _ core.CompressionThresholds) ([]core.RewritableLocation, error) {
	s.pageSizes = append(s.pageSizes, limit)
	out := make([]core.RewritableLocation, 0, limit)
	for i := range s.rows {
		if s.rows[i].ObjectKey <= after.ObjectKey {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, s.rows[i])
	}
	return out, nil
}

// ListCompressedLocations serves nothing: these tests drive the forward pass.
func (s *pagedStore) ListCompressedLocations(context.Context, int, core.Cursor) ([]core.RewritableLocation, error) {
	return nil, nil
}

// MarkObjectCompressed counts the copies the pass rewrote. A rewritten copy is
// dropped from the listing, matching a real store, so the pass cannot see it
// again on a later page.
func (s *pagedStore) MarkObjectCompressed(_ context.Context, u *core.CompressedUpdate, _ int64) error {
	s.marked++
	s.rows = slices.DeleteFunc(s.rows, func(r core.RewritableLocation) bool {
		return r.ObjectKey == u.ObjectKey
	})
	return nil
}

// RecordCompressionProbe is unused: these tests use compressible payloads.
func (s *pagedStore) RecordCompressionProbe(context.Context, *core.CompressionProbe) error {
	return nil
}

// pagedRows builds n distinct copies, keyed so cursor order is obvious.
func pagedRows(n int, size int64) []core.RewritableLocation {
	rows := make([]core.RewritableLocation, n)
	for i := range rows {
		rows[i] = core.RewritableLocation{
			ObjectKey:   fmt.Sprintf("bucket/obj-%03d", i),
			BackendName: "backend-a",
			SizeBytes:   size,
		}
	}
	return rows
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
	usageGate := opstest.NewMockUsageGate(ctrl)
	usageGate.EXPECT().WithinLimits(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(true).AnyTimes()
	usageGate.EXPECT().Record(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()

	return NewCompression(&CompressionDeps{
		Codec:   testCodec(t),
		Config:  cfg,
		Store:   store,
		Runtime: runtime,
		Usage:   usageGate,
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

	res, err := svc.CompressExisting(context.Background(), nil, 0)
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

	res, err := svc.CompressExisting(context.Background(), nil, 0)
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

// TestCompressExisting_ExcludesBelowMinSize checks the size floor reaches the
// listing: an object under it is never selected, so it costs no backend read
// and does not occupy a page slot on this pass or any later one.
func TestCompressExisting_ExcludesBelowMinSize(t *testing.T) {
	t.Parallel()
	payload := compressibleBytes(512)
	store := &oneRowStore{row: core.RewritableLocation{
		ObjectKey: "bucket/small.txt", BackendName: "backend-a", SizeBytes: int64(len(payload)),
	}}
	cfg := compressionOn()
	cfg.MinSize = 4096
	svc, be := newCompression(t, store, cfg, payload)

	res, err := svc.CompressExisting(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("CompressExisting: %v", err)
	}
	if res.Total != 0 || res.Succeeded != 0 {
		t.Errorf("counts = %+v, want nothing considered", res)
	}
	if be.gets.Load() != 0 {
		t.Errorf("GetObject calls = %d, want 0; the listing answers the size floor", be.gets.Load())
	}
}

// TestCompressExisting_StopsAtMax checks a capped run converts the number asked
// for and stops, which is what lets a fleet-sized conversion be spread across
// maintenance windows instead of run all at once.
func TestCompressExisting_StopsAtMax(t *testing.T) {
	t.Parallel()
	payload := compressibleBytes(8192)
	store := &pagedStore{rows: pagedRows(10, int64(len(payload)))}
	svc, be := newCompression(t, store, compressionOn(), payload)

	res, err := svc.CompressExisting(context.Background(), nil, 3)
	if err != nil {
		t.Fatalf("CompressExisting: %v", err)
	}
	if res.Succeeded != 3 {
		t.Errorf("rewrote %d, want the 3 asked for", res.Succeeded)
	}
	if be.gets.Load() != 3 {
		t.Errorf("GetObject calls = %d, want 3; a capped run reads only what it converts", be.gets.Load())
	}
	if store.marked != 3 {
		t.Errorf("marked %d rows, want 3", store.marked)
	}
}

// TestCompressExisting_MaxNarrowsThePage checks the cap reaches the listing. A
// run asking for 3 must not pull a full page of a hundred rows it will never
// look at.
func TestCompressExisting_MaxNarrowsThePage(t *testing.T) {
	t.Parallel()
	payload := compressibleBytes(8192)
	store := &pagedStore{rows: pagedRows(10, int64(len(payload)))}
	svc, _ := newCompression(t, store, compressionOn(), payload)

	if _, err := svc.CompressExisting(context.Background(), nil, 3); err != nil {
		t.Fatalf("CompressExisting: %v", err)
	}
	if len(store.pageSizes) == 0 || store.pageSizes[0] != 3 {
		t.Errorf("first page asked for %v, want 3", store.pageSizes)
	}
}

// TestCompressExisting_ResumesWhereTheLastRunStopped is the property that makes
// a capped run usable without an operator tracking anything: a converted copy
// leaves the listing, so the next run continues rather than repeating.
func TestCompressExisting_ResumesWhereTheLastRunStopped(t *testing.T) {
	t.Parallel()
	payload := compressibleBytes(8192)
	store := &pagedStore{rows: pagedRows(5, int64(len(payload)))}
	svc, _ := newCompression(t, store, compressionOn(), payload)

	for range 2 {
		if _, err := svc.CompressExisting(context.Background(), nil, 2); err != nil {
			t.Fatalf("CompressExisting: %v", err)
		}
	}

	if store.marked != 4 {
		t.Errorf("rewrote %d across two runs of 2, want 4 distinct copies", store.marked)
	}
	if len(store.rows) != 1 {
		t.Errorf("%d copies left uncompressed, want the 1 the two runs did not reach", len(store.rows))
	}
}

// TestCompressExisting_MaxZeroConvertsEverything pins the default: the knob is
// opt-in, and its absence is the whole-fleet run that existed before it.
func TestCompressExisting_MaxZeroConvertsEverything(t *testing.T) {
	t.Parallel()
	payload := compressibleBytes(8192)
	store := &pagedStore{rows: pagedRows(7, int64(len(payload)))}
	svc, _ := newCompression(t, store, compressionOn(), payload)

	res, err := svc.CompressExisting(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("CompressExisting: %v", err)
	}
	if res.Succeeded != 7 || len(store.rows) != 0 {
		t.Errorf("rewrote %d with %d left, want all 7 converted", res.Succeeded, len(store.rows))
	}
}

// TestCompressExisting_CancelStopsWithoutFailingThePage checks a cancelled pass
// exits rather than running out the page it is on. Without the check every
// remaining copy fails its download, and a run stopped on purpose reports a
// hundred failures that never happened.
func TestCompressExisting_CancelStopsWithoutFailingThePage(t *testing.T) {
	t.Parallel()
	payload := compressibleBytes(8192)
	store := &pagedStore{rows: pagedRows(50, int64(len(payload)))}
	svc, _ := newCompression(t, store, compressionOn(), payload)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := svc.CompressExisting(ctx, nil, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CompressExisting error = %v, want context.Canceled", err)
	}
	if res.Failed != 0 {
		t.Errorf("failed = %d, want 0; cancelling is not a rewrite failure", res.Failed)
	}
}

// TestCompressExisting_RecordsRatioDecline checks the encode that proved a copy
// incompressible is written down. Discarding it would have every later pass
// spend the same download and encode to reach the same verdict, which on a
// metered backend is quota spent for nothing.
func TestCompressExisting_RecordsRatioDecline(t *testing.T) {
	t.Parallel()
	payload := incompressibleBytes(t, 8192)
	store := &oneRowStore{row: core.RewritableLocation{
		ObjectKey: "bucket/random.bin", BackendName: "backend-a", SizeBytes: int64(len(payload)),
	}}
	cfg := compressionOn()
	svc, _ := newCompression(t, store, cfg, payload)

	res, err := svc.CompressExisting(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("CompressExisting: %v", err)
	}
	if res.Skipped != 1 || res.Succeeded != 0 {
		t.Errorf("counts = %+v, want one skipped", res)
	}
	if len(store.probes) != 1 {
		t.Fatalf("recorded %d probes, want 1", len(store.probes))
	}
	got := store.probes[0]
	if got.ObjectKey != "bucket/random.bin" || got.BackendName != "backend-a" {
		t.Errorf("probe names %s/%s, want bucket/random.bin/backend-a", got.ObjectKey, got.BackendName)
	}
	if got.Size <= 0 {
		t.Errorf("probe size = %d, want what the encoder produced", got.Size)
	}
	if got.Level != cfg.Level {
		t.Errorf("probe level = %q, want the level it was measured at (%q)", got.Level, cfg.Level)
	}
}

// TestCompressExisting_RatioDeclineSurvivesProbeFailure checks a copy is still
// declined when the measurement cannot be stored. Losing the record costs a
// later pass the work of measuring again; treating it as a rewrite failure
// would misreport a healthy run.
func TestCompressExisting_RatioDeclineSurvivesProbeFailure(t *testing.T) {
	t.Parallel()
	payload := incompressibleBytes(t, 8192)
	store := &oneRowStore{
		row: core.RewritableLocation{
			ObjectKey: "bucket/random.bin", BackendName: "backend-a", SizeBytes: int64(len(payload)),
		},
		probeErr: errors.New("probe write failed"),
	}
	svc, be := newCompression(t, store, compressionOn(), payload)

	res, err := svc.CompressExisting(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("CompressExisting: %v", err)
	}
	if res.Skipped != 1 || res.Failed != 0 {
		t.Errorf("counts = %+v, want one skipped and no failures", res)
	}
	if be.puts.Load() != 0 {
		t.Errorf("PutObject calls = %d, want 0; the copy stays as it is", be.puts.Load())
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

	res, err := svc.DecompressExisting(context.Background(), nil, 0)
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
		Store:   &oneRowStore{},
		Runtime: opstest.NewMockRuntimeOps(ctrl),
		Usage:   opstest.NewMockUsageGate(ctrl),
	})

	if _, err := svc.CompressExisting(context.Background(), nil, 0); !errors.Is(err, ErrCompressionUnavailable) {
		t.Errorf("err = %v, want ErrCompressionUnavailable", err)
	}
	if _, err := svc.DecompressExisting(context.Background(), nil, 0); !errors.Is(err, ErrCompressionUnavailable) {
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
	usageGate := opstest.NewMockUsageGate(ctrl)
	usageGate.EXPECT().WithinLimits(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(true).AnyTimes()
	usageGate.EXPECT().Record(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
	codec := testCodec(t)

	svc := NewCompression(&CompressionDeps{
		Codec:     codec,
		Config:    compressionOn(),
		Encryptor: enc,
		Store:     store,
		Runtime:   runtime,
		Usage:     usageGate,
	})

	got, err := svc.CompressExisting(context.Background(), nil, 0)
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

	res, err := svc.CompressExisting(context.Background(), nil, 0)
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

	res, err := svc.DecompressExisting(context.Background(), nil, 0)
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
func (f failingListStore) ListUncompressedLocations(context.Context, int, core.Cursor, core.CompressionThresholds) ([]core.RewritableLocation, error) {
	return nil, f.err
}

// ListCompressedLocations fails.
func (f failingListStore) ListCompressedLocations(context.Context, int, core.Cursor) ([]core.RewritableLocation, error) {
	return nil, f.err
}

// MarkObjectCompressed is never reached when the listing fails.
func (f failingListStore) MarkObjectCompressed(context.Context, *core.CompressedUpdate, int64) error {
	return nil
}

// RecordCompressionProbe is never reached when the listing fails.
func (f failingListStore) RecordCompressionProbe(context.Context, *core.CompressionProbe) error {
	return nil
}

// TestCompressExisting_ListingFailureStopsPass checks a listing error ends the
// run and surfaces, rather than being counted against an object. The counts
// gathered so far come back with it so a caller can report partial progress.
func TestCompressExisting_ListingFailureStopsPass(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("database is down")
	svc, _ := newCompression(t, failingListStore{err: wantErr}, compressionOn(), nil)

	res, err := svc.CompressExisting(context.Background(), nil, 0)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want the listing error", err)
	}
	if res.Total != 0 {
		t.Errorf("Total = %d, want 0; nothing was listed to process", res.Total)
	}

	if _, err := svc.DecompressExisting(context.Background(), nil, 0); !errors.Is(err, wantErr) {
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
	usageGate := opstest.NewMockUsageGate(ctrl)
	usageGate.EXPECT().WithinLimits(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(true).AnyTimes()
	usageGate.EXPECT().Record(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()

	svc := NewCompression(&CompressionDeps{
		Codec:   failingCodec{err: errors.New("encoder exploded")},
		Config:  compressionOn(),
		Store:   store,
		Runtime: runtime,
		Usage:   usageGate,
	})

	res, err := svc.CompressExisting(context.Background(), nil, 0)
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

// TestCompressExisting_ReportsProgress checks the pass reports each object as
// it goes. These read and rewrite an entire fleet, so a caller watching one
// needs to see it move; a summary that only arrives at the end is
// indistinguishable from a hung request.
//
// The status matters as much as the label: a skipped object reports skipped
// rather than failed, so a run over media does not read as one long failure.
func TestCompressExisting_ReportsProgress(t *testing.T) {
	t.Parallel()
	payload := make([]byte, compChunk)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("read random payload: %v", err)
	}
	store := &oneRowStore{row: core.RewritableLocation{
		ObjectKey: "bucket/random.bin", BackendName: "backend-a", SizeBytes: int64(len(payload)),
	}}
	svc, _ := newCompression(t, store, compressionOn(), payload)

	var steps []progress.Step
	obs := func(s progress.Step) { steps = append(steps, s) }

	if _, err := svc.CompressExisting(context.Background(), obs, 0); err != nil {
		t.Fatalf("CompressExisting: %v", err)
	}

	if len(steps) != 2 {
		t.Fatalf("steps = %d, want a start and an end for the one object: %+v", len(steps), steps)
	}
	if steps[0].Phase != progress.PhaseStart || steps[0].Label != "bucket/random.bin" {
		t.Errorf("first step = %+v, want a start labelled with the object key", steps[0])
	}
	if steps[1].Phase != progress.PhaseEnd {
		t.Errorf("second step = %+v, want an end", steps[1])
	}
	if steps[1].Status != progress.StatusSkipped {
		t.Errorf("status = %q, want %q: the object was declined, not failed",
			steps[1].Status, progress.StatusSkipped)
	}
}
