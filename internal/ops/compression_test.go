// -------------------------------------------------------------------------------
// Ops - Compression Operation Tests
//
// Author: Alex Freidah
//
// The bulk passes rewrite each copy onto its own backend, so what matters is
// that each placement carries bytes and a form that agree: an encoding
// described as an encoding, at the size it actually occupies, with the logical
// size the client's object still has. These tests drive a real codec over a
// fake backend and assert on what the mover placed.
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
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/ops/opstest"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/storedform"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// compChunk is the chunk size these tests encode at, small enough that a modest
// fixture still crosses frame boundaries.
const compChunk = compression.MinChunkSize

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

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

// oneRowStore serves a single rewritable location once and captures the probe
// a declined copy is patched with, along with every filter the pass listed by.
type oneRowStore struct {
	row        core.ObjectLocation
	filtersMu  sync.Mutex
	filters    []core.FormFilter
	served     atomic.Bool
	probes     []core.CompressionProbe
	probedLocs []core.ObjectLocation
	probeErr   error
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// ListCopiesByForm serves the row once, then reports the end of the listing so
// the pass terminates. It records the filter it was asked for and applies the
// candidate size floor, as the real store does, so a copy under it never
// reaches the pass.
func (s *oneRowStore) ListCopiesByForm(_ context.Context, f *core.FormFilter, _ int, _ core.Cursor) ([]core.ObjectLocation, error) {
	s.filtersMu.Lock()
	s.filters = append(s.filters, *f)
	s.filtersMu.Unlock()
	rows := s.serveOnce()
	if f.Candidates != nil && len(rows) == 1 && rows[0].ObjectSize() < f.Candidates.MinSize {
		return nil, nil
	}
	return rows, nil
}

// CountCopiesByForm is unused by the passes.
func (s *oneRowStore) CountCopiesByForm(context.Context, core.StoredFlags, core.StoredFlags) (int64, error) {
	return 0, nil
}

// UpdateCopyMetadata records the probe a declined copy was measured at and the
// copy it names, so a test can assert the pass wrote down what its encode cost
// bought.
func (s *oneRowStore) UpdateCopyMetadata(_ context.Context, loc *core.ObjectLocation, p *core.CopyPatch) error {
	if p.Probe != nil {
		s.probes = append(s.probes, *p.Probe)
		s.probedLocs = append(s.probedLocs, *loc)
	}
	return s.probeErr
}

// serveOnce yields the row on the first page and nothing after it.
func (s *oneRowStore) serveOnce() []core.ObjectLocation {
	if s.served.Swap(true) {
		return nil
	}
	return []core.ObjectLocation{s.row}
}

// pagedStore serves a fixed set of copies through the cursor the driver pages
// with, so a test can watch a capped run stop partway rather than seeing every
// row arrive on one page.
type pagedStore struct {
	rows      []core.ObjectLocation
	pageSizes []int
	marked    int
}

// ListCopiesByForm returns the rows after the cursor, capped at limit, and
// records what the driver asked for. It serves nothing to a listing of
// compressed copies: these tests drive the forward pass.
func (s *pagedStore) ListCopiesByForm(_ context.Context, f *core.FormFilter, limit int, after core.Cursor) ([]core.ObjectLocation, error) {
	if f.With.Has(core.StoredCompressed) {
		return nil, nil
	}
	s.pageSizes = append(s.pageSizes, limit)
	out := make([]core.ObjectLocation, 0, limit)
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

// CountCopiesByForm is unused by the passes.
func (s *pagedStore) CountCopiesByForm(context.Context, core.StoredFlags, core.StoredFlags) (int64, error) {
	return 0, nil
}

// rewrote counts a copy the pass placed. A rewritten copy is dropped from the
// listing, matching a real store, so the pass cannot see it again on a later
// page.
func (s *pagedStore) rewrote(loc *core.ObjectLocation) {
	s.marked++
	s.rows = slices.DeleteFunc(s.rows, func(r core.ObjectLocation) bool {
		return r.ObjectKey == loc.ObjectKey
	})
}

// UpdateCopyMetadata is unused: these tests use compressible payloads.
func (s *pagedStore) UpdateCopyMetadata(context.Context, *core.ObjectLocation, *core.CopyPatch) error {
	return nil
}

// pagedRows builds n distinct copies, keyed so cursor order is obvious.
func pagedRows(n int, size int64) []core.ObjectLocation {
	rows := make([]core.ObjectLocation, n)
	for i := range rows {
		rows[i] = core.ObjectLocation{
			ObjectKey:   fmt.Sprintf("bucket/obj-%03d", i),
			BackendName: "backend-a",
			SizeBytes:   size,
		}
	}
	return rows
}

// newCompression builds the service over a fake backend holding payload and
// a mover that places what the pass reforms.
func newCompression(t *testing.T, store RewriteStore, cfg config.CompressionConfig, payload []byte) (*Compression, *fakeBackend, *fakeMover) {
	t.Helper()
	return newCompressionWith(t, store, cfg, payload, testCodec(t), nil)
}

// newCompressionWith builds the service over the given codec and encryptor.
// The codec is an interface so a test can hand in a failing one.
func newCompressionWith(t *testing.T, store RewriteStore, cfg config.CompressionConfig, payload []byte, codec storedform.Codec, enc *encryption.Encryptor) (*Compression, *fakeBackend, *fakeMover) {
	t.Helper()
	ctrl := gomock.NewController(t)
	be := &fakeBackend{payload: payload}
	mover := &fakeMover{}

	runtime := opstest.NewMockRuntimeOps(ctrl)
	runtime.EXPECT().GetBackend(gomock.Any()).Return(be, nil).AnyTimes()

	return NewCompression(&CompressionDeps{
		Codec:     codec,
		Config:    cfg,
		Encryptor: enc,
		Store:     store,
		Runtime:   runtime,
		Mover:     mover,
	}), be, mover
}

// onlyPlacement returns the one copy the mover placed, failing the test when
// there is not exactly one.
func onlyPlacement(t *testing.T, mover *fakeMover) placedCopy {
	t.Helper()
	placed := mover.placements()
	if len(placed) != 1 {
		t.Fatalf("placements = %d, want 1", len(placed))
	}
	return placed[0]
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
	store := &oneRowStore{row: core.ObjectLocation{
		ObjectKey: "bucket/file.txt", BackendName: "backend-a", SizeBytes: int64(len(payload)),
	}}
	svc, be, mover := newCompression(t, store, compressionOn(), payload)

	res, err := svc.CompressExisting(context.Background(), nil, 0, "")
	if err != nil {
		t.Fatalf("CompressExisting: %v", err)
	}
	if res.Total != 1 || res.Succeeded != 1 || res.Skipped != 0 || res.Failed != 0 {
		t.Fatalf("counts = %+v, want one succeeded", res)
	}
	if be.puts.Load() != 1 {
		t.Errorf("PutObject calls = %d, want 1", be.puts.Load())
	}

	placed := onlyPlacement(t, mover)
	got := placed.form
	if got.CompressionAlgorithm != compression.Algorithm {
		t.Errorf("Algorithm = %q, want %q", got.CompressionAlgorithm, compression.Algorithm)
	}
	if got.CompressionLevel != "default" {
		t.Errorf("Level = %q, want %q", got.CompressionLevel, "default")
	}
	if got.CompressionFormatVersion != compression.FormatVersion {
		t.Errorf("FormatVersion = %d, want %d", got.CompressionFormatVersion, compression.FormatVersion)
	}
	if got.LogicalSize != int64(len(payload)) {
		t.Errorf("LogicalSize = %d, want %d", got.LogicalSize, len(payload))
	}
	if placed.size >= int64(len(payload)) || placed.size != int64(len(placed.body)) {
		t.Errorf("size = %d over %d bytes, want the encoding, fewer than the %d uploaded",
			placed.size, len(placed.body), len(payload))
	}
	decoded := decodeAll(t, placed.body)
	if !bytes.Equal(decoded, payload) {
		t.Error("the placed encoding does not decode back to the original")
	}
}

// decodeAll decodes one zstd stream in full.
func decodeAll(t *testing.T, encoded []byte) []byte {
	t.Helper()
	r, err := testCodec(t).DecompressStream(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("DecompressStream: %v", err)
	}
	defer func() { _ = r.Close() }()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read decoded: %v", err)
	}
	return out
}

// TestCompressionPasses_WantedForm checks the form each pass asks the copy to
// end up in: compression is added or removed, and an envelope is kept either
// way.
func TestCompressionPasses_WantedForm(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t)
	plain := compressibleBytes(compChunk)
	keyData, keyID, ciphertext := sealed(t, enc, plain)
	row := core.ObjectLocation{
		ObjectKey: "bucket/secret.txt", BackendName: "backend-a", SizeBytes: int64(len(ciphertext)),
		Encrypted: true, EncryptionKey: keyData, KeyID: keyID, PlaintextSize: int64(len(plain)),
	}
	svc, _, mover := newCompressionWith(t, &oneRowStore{row: row}, compressionOn(), ciphertext, testCodec(t), enc)

	if _, err := svc.CompressExisting(context.Background(), nil, 0, ""); err != nil {
		t.Fatalf("CompressExisting: %v", err)
	}
	placed := onlyPlacement(t, mover)
	if got := placed.form.Form() & core.StoredLayers; got != core.StoredLayers {
		t.Errorf("compress-existing placed %v, want compressed and encrypted", got)
	}
}

// TestCompressionPasses_ListByForm verifies each pass selects its copies by
// stored form: the forward pass lists uncompressed candidates judged against
// the configured thresholds, and the reverse pass lists compressed copies, both
// on the backend the operator named.
func TestCompressionPasses_ListByForm(t *testing.T) {
	t.Parallel()
	cfg := compressionOn()
	forward := &oneRowStore{}
	svc, _, _ := newCompression(t, forward, cfg, nil)
	if _, err := svc.CompressExisting(context.Background(), nil, 0, "backend-a"); err != nil {
		t.Fatalf("CompressExisting: %v", err)
	}
	reverse := &oneRowStore{}
	svc, _, _ = newCompression(t, reverse, cfg, nil)
	if _, err := svc.DecompressExisting(context.Background(), nil, 0, "backend-a"); err != nil {
		t.Fatalf("DecompressExisting: %v", err)
	}

	wantForward := core.FormFilter{
		Without: core.StoredCompressed, Backend: "backend-a",
		Candidates: &core.CompressionThresholds{MinSize: cfg.MinSize, MinRatio: cfg.MinRatio, Level: cfg.Level},
	}
	if len(forward.filters) == 0 {
		t.Fatal("the forward pass never listed")
	}
	if got := forward.filters[0]; !reflect.DeepEqual(got, wantForward) {
		t.Errorf("forward filter = %+v (candidates %+v), want %+v", got, got.Candidates, wantForward)
	}
	wantReverse := core.FormFilter{With: core.StoredCompressed, Backend: "backend-a"}
	if len(reverse.filters) == 0 || !reflect.DeepEqual(reverse.filters[0], wantReverse) {
		t.Errorf("reverse filters = %+v, want %+v", reverse.filters, wantReverse)
	}
}

// TestCompressExisting_SkipsIncompressible checks the entropy floor. The object
// is downloaded and encoded, because only the finished encoding answers the
// question, but nothing is written and nothing is placed.
func TestCompressExisting_SkipsIncompressible(t *testing.T) {
	t.Parallel()
	payload := make([]byte, compChunk)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("read random payload: %v", err)
	}
	store := &oneRowStore{row: core.ObjectLocation{
		ObjectKey: "bucket/random.bin", BackendName: "backend-a", SizeBytes: int64(len(payload)),
	}}
	cfg := compressionOn()
	svc, be, mover := newCompression(t, store, cfg, payload)

	res, err := svc.CompressExisting(context.Background(), nil, 0, "")
	if err != nil {
		t.Fatalf("CompressExisting: %v", err)
	}
	if res.Skipped != 1 || res.Succeeded != 0 || res.Failed != 0 {
		t.Errorf("counts = %+v, want one skipped", res)
	}
	if be.puts.Load() != 0 {
		t.Errorf("PutObject calls = %d, want 0; nothing should be written", be.puts.Load())
	}
	if n := len(mover.placements()); n != 0 {
		t.Errorf("placements = %d, want none for an object that was not compressed", n)
	}
}

// TestCompressExisting_ExcludesBelowMinSize checks the size floor reaches the
// listing: an object under it is never selected, so it costs no backend read
// and does not occupy a page slot on this pass or any later one.
func TestCompressExisting_ExcludesBelowMinSize(t *testing.T) {
	t.Parallel()
	payload := compressibleBytes(512)
	store := &oneRowStore{row: core.ObjectLocation{
		ObjectKey: "bucket/small.txt", BackendName: "backend-a", SizeBytes: int64(len(payload)),
	}}
	cfg := compressionOn()
	cfg.MinSize = 4096
	svc, be, _ := newCompression(t, store, cfg, payload)

	res, err := svc.CompressExisting(context.Background(), nil, 0, "")
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
	svc, be, mover := newCompression(t, store, compressionOn(), payload)
	mover.onPlaced = store.rewrote

	res, err := svc.CompressExisting(context.Background(), nil, 3, "")
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
	svc, _, mover := newCompression(t, store, compressionOn(), payload)
	mover.onPlaced = store.rewrote

	if _, err := svc.CompressExisting(context.Background(), nil, 3, ""); err != nil {
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
	svc, _, mover := newCompression(t, store, compressionOn(), payload)
	mover.onPlaced = store.rewrote

	for range 2 {
		if _, err := svc.CompressExisting(context.Background(), nil, 2, ""); err != nil {
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
	svc, _, mover := newCompression(t, store, compressionOn(), payload)
	mover.onPlaced = store.rewrote

	res, err := svc.CompressExisting(context.Background(), nil, 0, "")
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
	svc, _, _ := newCompression(t, store, compressionOn(), payload)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := svc.CompressExisting(ctx, nil, 0, "")
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
	store := &oneRowStore{row: core.ObjectLocation{
		ObjectKey: "bucket/random.bin", BackendName: "backend-a", SizeBytes: int64(len(payload)),
	}}
	cfg := compressionOn()
	svc, _, _ := newCompression(t, store, cfg, payload)

	res, err := svc.CompressExisting(context.Background(), nil, 0, "")
	if err != nil {
		t.Fatalf("CompressExisting: %v", err)
	}
	if res.Skipped != 1 || res.Succeeded != 0 {
		t.Errorf("counts = %+v, want one skipped", res)
	}
	if len(store.probes) != 1 {
		t.Fatalf("recorded %d probes, want 1", len(store.probes))
	}
	if loc := store.probedLocs[0]; loc.ObjectKey != "bucket/random.bin" || loc.BackendName != "backend-a" {
		t.Errorf("probe names %s/%s, want bucket/random.bin/backend-a", loc.ObjectKey, loc.BackendName)
	}
	got := store.probes[0]
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
		row: core.ObjectLocation{
			ObjectKey: "bucket/random.bin", BackendName: "backend-a", SizeBytes: int64(len(payload)),
		},
		probeErr: errors.New("probe write failed"),
	}
	svc, be, _ := newCompression(t, store, compressionOn(), payload)

	res, err := svc.CompressExisting(context.Background(), nil, 0, "")
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

// TestCompressExisting_RatioDeclineOnReplacedCopy checks a decline whose copy a
// newer write replaced is still a plain skip: there is no row left to record
// the probe against, and that is not a failure of the pass.
func TestCompressExisting_RatioDeclineOnReplacedCopy(t *testing.T) {
	t.Parallel()
	payload := incompressibleBytes(t, 8192)
	store := &oneRowStore{
		row: core.ObjectLocation{
			ObjectKey: "bucket/random.bin", BackendName: "backend-a", SizeBytes: int64(len(payload)),
		},
		probeErr: core.ErrCopyChanged,
	}
	svc, be, _ := newCompression(t, store, compressionOn(), payload)

	res, err := svc.CompressExisting(context.Background(), nil, 0, "")
	if err != nil {
		t.Fatalf("CompressExisting: %v", err)
	}
	if res.Skipped != 1 || res.Failed != 0 || res.Succeeded != 0 {
		t.Errorf("counts = %+v, want one skipped and no failures", res)
	}
	if len(store.probes) != 1 {
		t.Errorf("probe writes = %d, want 1", len(store.probes))
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

	store := &oneRowStore{row: core.ObjectLocation{
		ObjectKey:                "bucket/file.txt",
		BackendName:              "backend-a",
		SizeBytes:                int64(encoded.Len()),
		CompressionAlgorithm:     compression.Algorithm,
		CompressionFormatVersion: compression.FormatVersion,
		LogicalSize:              int64(len(plain)),
	}}
	svc, be, mover := newCompression(t, store, compressionOn(), encoded.Bytes())

	res, err := svc.DecompressExisting(context.Background(), nil, 0, "")
	if err != nil {
		t.Fatalf("DecompressExisting: %v", err)
	}
	if res.Succeeded != 1 || res.Failed != 0 {
		t.Fatalf("counts = %+v, want one succeeded", res)
	}
	placed := onlyPlacement(t, mover)
	if got := placed.form.Form(); got != 0 {
		t.Errorf("form = %v, want verbatim so the row no longer claims an encoding", got)
	}
	if placed.size != int64(len(plain)) {
		t.Errorf("size = %d, want the %d decoded bytes", placed.size, len(plain))
	}
	if !bytes.Equal(be.lastPut, plain) {
		t.Error("the object written back is not what was originally encoded")
	}
}

// TestDecompressExisting_KeepsTheEnvelope checks the reverse pass over an
// encrypted encoding decodes underneath the envelope and seals the decoded
// bytes again, so the copy stays encrypted.
func TestDecompressExisting_KeepsTheEnvelope(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t)
	codec := testCodec(t)
	plain := compressibleBytes(compChunk * 2)
	var encoded bytes.Buffer
	if _, err := codec.Compress(&encoded, bytes.NewReader(plain)); err != nil {
		t.Fatalf("Compress: %v", err)
	}
	keyData, keyID, ciphertext := sealed(t, enc, encoded.Bytes())
	store := &oneRowStore{row: core.ObjectLocation{
		ObjectKey: "bucket/secret.txt", BackendName: "backend-a", SizeBytes: int64(len(ciphertext)),
		Encrypted: true, EncryptionKey: keyData, KeyID: keyID, PlaintextSize: int64(encoded.Len()),
		CompressionAlgorithm: compression.Algorithm, CompressionFormatVersion: compression.FormatVersion,
		LogicalSize: int64(len(plain)),
	}}
	svc, _, mover := newCompressionWith(t, store, compressionOn(), ciphertext, codec, enc)

	res, err := svc.DecompressExisting(context.Background(), nil, 0, "")
	if err != nil || res.Succeeded != 1 {
		t.Fatalf("DecompressExisting = %+v, %v; want one succeeded", res, err)
	}
	placed := onlyPlacement(t, mover)
	if got := placed.form.Form() & core.StoredLayers; got != core.StoredEncrypted {
		t.Fatalf("form = %v, want encrypted only", got)
	}
	decrypted, _, err := enc.DecryptStored(context.Background(), bytes.NewReader(placed.body),
		placed.form.EncryptionKey, placed.form.KeyID, placed.form.PlaintextSize, nil)
	if err != nil {
		t.Fatalf("DecryptStored: %v", err)
	}
	got, err := io.ReadAll(decrypted)
	if err != nil {
		t.Fatalf("read decrypted: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Error("the rewritten copy does not decrypt to the original")
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
		Mover:   &fakeMover{},
	})

	if _, err := svc.CompressExisting(context.Background(), nil, 0, ""); !errors.Is(err, ErrCompressionUnavailable) {
		t.Errorf("err = %v, want ErrCompressionUnavailable", err)
	}
	if _, err := svc.DecompressExisting(context.Background(), nil, 0, ""); !errors.Is(err, ErrCompressionUnavailable) {
		t.Errorf("err = %v, want ErrCompressionUnavailable", err)
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

	store := &oneRowStore{row: core.ObjectLocation{
		ObjectKey:     "bucket/secret.txt",
		BackendName:   "backend-a",
		SizeBytes:     int64(len(ciphertext)),
		Encrypted:     true,
		EncryptionKey: encryption.PackKeyData(res.BaseNonce, res.WrappedDEK),
		KeyID:         res.KeyID,
		PlaintextSize: int64(len(plain)),
	}}

	codec := testCodec(t)
	svc, be, mover := newCompressionWith(t, store, compressionOn(), ciphertext, codec, enc)

	got, err := svc.CompressExisting(context.Background(), nil, 0, "")
	if err != nil {
		t.Fatalf("CompressExisting: %v", err)
	}
	if got.Succeeded != 1 || got.Failed != 0 {
		t.Fatalf("counts = %+v, want one succeeded", got)
	}

	placed := onlyPlacement(t, mover)
	u := placed.form
	if u.LogicalSize != int64(len(plain)) {
		t.Errorf("LogicalSize = %d, want the %d bytes the client wrote", u.LogicalSize, len(plain))
	}
	if u.PlaintextSize >= u.LogicalSize {
		t.Errorf("PlaintextSize = %d, want the encoded stream, smaller than the %d byte object",
			u.PlaintextSize, u.LogicalSize)
	}
	if placed.size <= u.PlaintextSize {
		t.Errorf("size = %d, want more than the %d byte encoding it encrypts",
			placed.size, u.PlaintextSize)
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
	store := &oneRowStore{row: core.ObjectLocation{
		ObjectKey: "bucket/secret", BackendName: "backend-a", SizeBytes: 4096,
		Encrypted: true, PlaintextSize: 4000,
	}}
	svc, be, _ := newCompression(t, store, compressionOn(), compressibleBytes(4096))

	res, err := svc.CompressExisting(context.Background(), nil, 0, "")
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
	store := &oneRowStore{row: core.ObjectLocation{
		ObjectKey: "bucket/claims-encoding", BackendName: "backend-a", SizeBytes: 64,
		CompressionAlgorithm: compression.Algorithm, LogicalSize: 4096,
	}}
	svc, _, mover := newCompression(t, store, compressionOn(), []byte("not a zstd stream at all"))

	res, err := svc.DecompressExisting(context.Background(), nil, 0, "")
	if err != nil {
		t.Fatalf("DecompressExisting: %v", err)
	}
	if res.Failed != 1 || res.Succeeded != 0 {
		t.Errorf("counts = %+v, want one failed", res)
	}
	if n := len(mover.placements()); n != 0 {
		t.Errorf("placements = %d, want none for an object that could not be decoded", n)
	}
}

// failingListStore fails the listing, which is the one error that ends a pass
// rather than being counted against a single object.
type failingListStore struct{ err error }

// ListCopiesByForm fails.
func (f failingListStore) ListCopiesByForm(context.Context, *core.FormFilter, int, core.Cursor) ([]core.ObjectLocation, error) {
	return nil, f.err
}

// CountCopiesByForm is never reached by the passes.
func (f failingListStore) CountCopiesByForm(context.Context, core.StoredFlags, core.StoredFlags) (int64, error) {
	return 0, f.err
}

// UpdateCopyMetadata is never reached when the listing fails.
func (f failingListStore) UpdateCopyMetadata(context.Context, *core.ObjectLocation, *core.CopyPatch) error {
	return nil
}

// TestCompressExisting_ListingFailureStopsPass checks a listing error ends the
// run and surfaces, rather than being counted against an object. The counts
// gathered so far come back with it so a caller can report partial progress.
func TestCompressExisting_ListingFailureStopsPass(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("database is down")
	svc, _, _ := newCompression(t, failingListStore{err: wantErr}, compressionOn(), nil)

	res, err := svc.CompressExisting(context.Background(), nil, 0, "")
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want the listing error", err)
	}
	if res.Total != 0 {
		t.Errorf("Total = %d, want 0; nothing was listed to process", res.Total)
	}

	if _, err := svc.DecompressExisting(context.Background(), nil, 0, ""); !errors.Is(err, wantErr) {
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
	store := &oneRowStore{row: core.ObjectLocation{
		ObjectKey: "bucket/file.txt", BackendName: "backend-a", SizeBytes: 4096,
	}}
	svc, be, mover := newCompressionWith(t, store, compressionOn(), compressibleBytes(4096),
		failingCodec{err: errors.New("encoder exploded")}, nil)

	res, err := svc.CompressExisting(context.Background(), nil, 0, "")
	if err != nil {
		t.Fatalf("CompressExisting: %v", err)
	}
	if res.Failed != 1 || res.Succeeded != 0 || res.Skipped != 0 {
		t.Errorf("counts = %+v, want one failed", res)
	}
	if be.puts.Load() != 0 {
		t.Errorf("PutObject calls = %d, want 0", be.puts.Load())
	}
	if n := len(mover.placements()); n != 0 {
		t.Errorf("placements = %d, want none for an object that never encoded", n)
	}
}

// TestCompressExisting_ReportsProgress checks the pass reports each object as
// it goes, with a skipped object reported as skipped rather than failed.
func TestCompressExisting_ReportsProgress(t *testing.T) {
	t.Parallel()
	payload := make([]byte, compChunk)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("read random payload: %v", err)
	}
	store := &oneRowStore{row: core.ObjectLocation{
		ObjectKey: "bucket/random.bin", BackendName: "backend-a", SizeBytes: int64(len(payload)),
	}}
	svc, _, _ := newCompression(t, store, compressionOn(), payload)

	var steps []progress.Step
	obs := func(s progress.Step) { steps = append(steps, s) }

	if _, err := svc.CompressExisting(context.Background(), obs, 0, ""); err != nil {
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

// TestCompressExisting_ChangedCopyIsNotAFailure covers a client writing the key
// while the pass was converting it. The placement refuses, and the copy is
// counted as changed rather than failed: nothing is broken, the pass simply
// lost a race it is expected to lose sometimes.
func TestCompressExisting_ChangedCopyIsNotAFailure(t *testing.T) {
	t.Parallel()
	payload := compressibleBytes(8192)
	store := &oneRowStore{row: core.ObjectLocation{
		ObjectKey: "bucket/raced.txt", BackendName: "backend-a", SizeBytes: int64(len(payload)),
	}}
	svc, _, mover := newCompression(t, store, compressionOn(), payload)
	mover.placeErr = fmt.Errorf("place copy: %w", core.ErrCopyChanged)

	res, err := svc.CompressExisting(context.Background(), nil, 0, "")
	if err != nil {
		t.Fatalf("CompressExisting: %v", err)
	}

	if res.Changed != 1 {
		t.Errorf("Changed = %d, want 1", res.Changed)
	}
	if res.Failed != 0 {
		t.Errorf("Failed = %d, want 0 - a lost race is not a failure", res.Failed)
	}
	if res.Succeeded != 0 {
		t.Errorf("Succeeded = %d, want 0 - nothing was recorded", res.Succeeded)
	}
}

// TestCompressExisting_MovesTheCopyTheListingReported pins that the move names
// the copy the listing served, storage key included, since that key is what
// lets the placement refuse a copy a client replaced.
func TestCompressExisting_MovesTheCopyTheListingReported(t *testing.T) {
	t.Parallel()
	payload := compressibleBytes(8192)
	row := core.ObjectLocation{
		ObjectKey: "bucket/quiet.txt", BackendName: "backend-a", StorageKey: "bucket/quiet.txt!w1",
		SizeBytes: int64(len(payload)), Identity: &core.ObjectIdentity{ETag: "etag-v1"},
	}
	svc, _, mover := newCompression(t, &oneRowStore{row: row}, compressionOn(), payload)

	if _, err := svc.CompressExisting(context.Background(), nil, 0, ""); err != nil {
		t.Fatalf("CompressExisting: %v", err)
	}

	reqs := mover.moveRequests()
	if len(reqs) != 1 {
		t.Fatalf("MoveObject calls = %d, want 1", len(reqs))
	}
	if got := reqs[0].Source; got.StorageKey != row.StorageKey || !reflect.DeepEqual(got.Identity, row.Identity) {
		t.Errorf("moved %+v, want the listed copy at %s", got, row.StorageKey)
	}
	if reqs[0].DestName != row.BackendName || reqs[0].Reasons != writepath.RewriteMoveReasons {
		t.Errorf("move to %q with %+v, want a rewrite onto %s", reqs[0].DestName, reqs[0].Reasons, row.BackendName)
	}
}
