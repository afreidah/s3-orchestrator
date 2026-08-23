// -------------------------------------------------------------------------------
// Representation Carry Tests
//
// Author: Alex Freidah
//
// Every path that moves an object's stored bytes to another backend without
// re-encoding them has to write the same description of those bytes on the row
// it creates. A copy that lands without it is an object nothing can decode, and
// the replicator will spread that row to further backends. These tests drive
// the real store rather than a mock, because the failure they guard against is
// a column quietly missing from an INSERT.
// -------------------------------------------------------------------------------

package sqlite

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// compressedForm describes an object stored both compressed and encrypted,
// which is the case that carries every column at once.
func compressedForm() *core.StoredForm {
	return &core.StoredForm{
		Encrypted:                true,
		EncryptionKey:            []byte("wrapped-dek-bytes"),
		KeyID:                    "key-1",
		PlaintextSize:            2048,
		ContentHash:              "abc123",
		CompressionAlgorithm:     "zstd",
		CompressionLevel:         "default",
		CompressionFormatVersion: 1,
		LogicalSize:              8192,
	}
}

// assertCarried checks a row describes the stored bytes the way the source did.
// Every field is stated: the point of the test is that none of them is dropped.
func assertCarried(t *testing.T, got *core.ObjectLocation, wantSize int64) {
	t.Helper()
	want := compressedForm()
	if got.SizeBytes != wantSize {
		t.Errorf("SizeBytes = %d, want %d", got.SizeBytes, wantSize)
	}
	if !got.Encrypted {
		t.Error("Encrypted = false, want true")
	}
	if !bytes.Equal(got.EncryptionKey, want.EncryptionKey) {
		t.Errorf("EncryptionKey = %q, want %q", got.EncryptionKey, want.EncryptionKey)
	}
	if got.KeyID != want.KeyID {
		t.Errorf("KeyID = %q, want %q", got.KeyID, want.KeyID)
	}
	if got.PlaintextSize != want.PlaintextSize {
		t.Errorf("PlaintextSize = %d, want %d", got.PlaintextSize, want.PlaintextSize)
	}
	if got.ContentHash != want.ContentHash {
		t.Errorf("ContentHash = %q, want %q", got.ContentHash, want.ContentHash)
	}
	if got.CompressionAlgorithm != want.CompressionAlgorithm {
		t.Errorf("CompressionAlgorithm = %q, want %q", got.CompressionAlgorithm, want.CompressionAlgorithm)
	}
	if got.CompressionLevel != want.CompressionLevel {
		t.Errorf("CompressionLevel = %q, want %q", got.CompressionLevel, want.CompressionLevel)
	}
	if got.CompressionFormatVersion != want.CompressionFormatVersion {
		t.Errorf("CompressionFormatVersion = %d, want %d", got.CompressionFormatVersion, want.CompressionFormatVersion)
	}
	if got.LogicalSize != want.LogicalSize {
		t.Errorf("LogicalSize = %d, want %d", got.LogicalSize, want.LogicalSize)
	}
}

// locationOn returns the copy of key held on backendName.
func locationOn(t *testing.T, s *Store, key, backendName string) *core.ObjectLocation {
	t.Helper()
	locs, err := s.GetAllObjectLocations(context.Background(), key)
	if err != nil {
		t.Fatalf("GetAllObjectLocations: %v", err)
	}
	for i := range locs {
		if locs[i].BackendName == backendName {
			return &locs[i]
		}
	}
	t.Fatalf("no copy of %q on %q; have %+v", key, backendName, locs)
	return nil
}

// TestMoveObjectLocation_CarriesRepresentation covers the rebalance and drain
// path. The bytes are moved verbatim, so the row that lands on the destination
// has to say exactly what the source row said about them.
func TestMoveObjectLocation_CarriesRepresentation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.RecordObject(ctx, "bucket/moved", "backend-a", 4096, compressedForm()); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	if _, err := s.MoveObjectLocation(ctx, "bucket/moved", "backend-a", "backend-b"); err != nil {
		t.Fatalf("MoveObjectLocation: %v", err)
	}

	assertCarried(t, locationOn(t, s, "bucket/moved", "backend-b"), 4096)
}

// TestPromotePending_CarriesRepresentation covers crash recovery. A PUT that
// died between the upload and the commit leaves an intent the reaper promotes
// on a later tick, and the promoted row has to describe the bytes the way the
// PUT would have; anything less and the recovered object is one nothing can
// decode.
func TestPromotePending_CarriesRepresentation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	intent := core.PendingObject{
		IntentID:    "intent-z",
		ObjectKey:   "bucket/recovered",
		BackendName: "backend-a",
		SizeBytes:   4096,
	}
	intent.ApplyStoredForm(compressedForm())
	if err := s.InsertPending(ctx, &intent); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}
	stale, _ := s.GetStalePending(ctx, time.Now().Add(time.Hour), 10)
	if len(stale) != 1 {
		t.Fatal("seed: pending row missing")
	}

	if _, _, err := s.PromotePending(ctx, &stale[0]); err != nil {
		t.Fatalf("PromotePending: %v", err)
	}

	assertCarried(t, locationOn(t, s, "bucket/recovered", "backend-a"), 4096)
}

// TestPromotePending_ChargesStoredSize pins the sizing half: quota counts what
// occupies the backend. Charging the logical size instead would drift every
// recovered object by its compression ratio, and the drift is silent.
func TestPromotePending_ChargesStoredSize(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	intent := core.PendingObject{
		IntentID:    "intent-q",
		ObjectKey:   "bucket/sized",
		BackendName: "backend-a",
		SizeBytes:   4096,
	}
	// LogicalSize is twice what landed, so a charge against the wrong one shows.
	intent.ApplyStoredForm(compressedForm())
	if err := s.InsertPending(ctx, &intent); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}
	stale, _ := s.GetStalePending(ctx, time.Now().Add(time.Hour), 10)
	if len(stale) != 1 {
		t.Fatal("seed: pending row missing")
	}

	if _, _, err := s.PromotePending(ctx, &stale[0]); err != nil {
		t.Fatalf("PromotePending: %v", err)
	}

	stats, err := s.GetQuotaStats(ctx)
	if err != nil {
		t.Fatalf("GetQuotaStats: %v", err)
	}
	if got := stats["backend-a"].BytesUsed; got != 4096 {
		t.Errorf("bytes_used = %d, want the 4096 that landed on the backend", got)
	}
}

// TestRecordReplica_CarriesRepresentation covers the replicator. The replica is
// a second copy of the same stored bytes, so it needs the same description; a
// replica recorded as verbatim is one the read path serves as compressed bytes
// at the wrong size.
func TestRecordReplica_CarriesRepresentation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.RecordObject(ctx, "bucket/replicated", "backend-a", 4096, compressedForm()); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	if _, _, err := s.RecordReplica(ctx, "bucket/replicated", "backend-b", "backend-a"); err != nil {
		t.Fatalf("RecordReplica: %v", err)
	}

	assertCarried(t, locationOn(t, s, "bucket/replicated", "backend-b"), 4096)
}

// TestListUncompressedLocations_SelectsOnlyVerbatim checks the listing that
// drives compress-existing: it must offer copies with no encoding and skip the
// ones that already have one, or a pass would re-encode what it just wrote.
func TestListUncompressedLocations_SelectsOnlyVerbatim(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.RecordObject(ctx, "bucket/plain", "backend-a", 100, nil); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if _, err := s.RecordObject(ctx, "bucket/encoded", "backend-a", 40, compressedForm()); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	verbatim, err := s.ListUncompressedLocations(ctx, 10, 0)
	if err != nil {
		t.Fatalf("ListUncompressedLocations: %v", err)
	}
	if len(verbatim) != 1 || verbatim[0].ObjectKey != "bucket/plain" {
		t.Fatalf("uncompressed listing = %+v, want only bucket/plain", verbatim)
	}

	encoded, err := s.ListCompressedLocations(ctx, 10, 0)
	if err != nil {
		t.Fatalf("ListCompressedLocations: %v", err)
	}
	if len(encoded) != 1 || encoded[0].ObjectKey != "bucket/encoded" {
		t.Fatalf("compressed listing = %+v, want only bucket/encoded", encoded)
	}
	// The encryption columns ride along because a rewrite has to unwrap them
	// before it can touch the bytes.
	if !encoded[0].Encrypted || encoded[0].KeyID != "key-1" {
		t.Errorf("encryption metadata missing from the listing: %+v", encoded[0])
	}
	if encoded[0].LogicalSize != 8192 {
		t.Errorf("LogicalSize = %d, want 8192", encoded[0].LogicalSize)
	}
}

// TestMarkObjectCompressed_MovesQuota pins the half that is easy to get wrong:
// a rewrite changes how many bytes the copy occupies, and the backend counter
// has to follow it or quota drifts by the compression ratio on every object.
func TestMarkObjectCompressed_MovesQuota(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.RecordObject(ctx, "bucket/shrinking", "backend-a", 1000, nil); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	before, err := s.GetQuotaStats(ctx)
	if err != nil {
		t.Fatalf("GetQuotaStats: %v", err)
	}

	update := &core.CompressedUpdate{
		ObjectKey:     "bucket/shrinking",
		BackendName:   "backend-a",
		Algorithm:     "zstd",
		Level:         "default",
		FormatVersion: 1,
		SizeBytes:     250,
		LogicalSize:   1000,
	}
	if err := s.MarkObjectCompressed(ctx, update, 1000); err != nil {
		t.Fatalf("MarkObjectCompressed: %v", err)
	}

	loc := locationOn(t, s, "bucket/shrinking", "backend-a")
	if loc.SizeBytes != 250 {
		t.Errorf("SizeBytes = %d, want the 250 now stored", loc.SizeBytes)
	}
	if loc.CompressionAlgorithm != "zstd" || loc.LogicalSize != 1000 {
		t.Errorf("row does not describe the encoding: %+v", loc)
	}

	after, err := s.GetQuotaStats(ctx)
	if err != nil {
		t.Fatalf("GetQuotaStats: %v", err)
	}
	if got, want := after["backend-a"].BytesUsed, before["backend-a"].BytesUsed-750; got != want {
		t.Errorf("bytes_used = %d, want %d after a 1000 byte object became 250", got, want)
	}
}

// TestMarkObjectCompressed_ClearsOnDecompress checks the reverse update: the
// row stops claiming an encoding, so nothing downstream tries to decode bytes
// that are no longer encoded.
func TestMarkObjectCompressed_ClearsOnDecompress(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.RecordObject(ctx, "bucket/expanding", "backend-a", 250, compressedForm()); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	update := &core.CompressedUpdate{
		ObjectKey:   "bucket/expanding",
		BackendName: "backend-a",
		SizeBytes:   1000,
	}
	if err := s.MarkObjectCompressed(ctx, update, 250); err != nil {
		t.Fatalf("MarkObjectCompressed: %v", err)
	}

	loc := locationOn(t, s, "bucket/expanding", "backend-a")
	if loc.CompressionAlgorithm != "" {
		t.Errorf("CompressionAlgorithm = %q, want empty", loc.CompressionAlgorithm)
	}
	if loc.LogicalSize != 0 {
		t.Errorf("LogicalSize = %d, want 0 once the row is verbatim again", loc.LogicalSize)
	}
	if loc.SizeBytes != 1000 {
		t.Errorf("SizeBytes = %d, want the 1000 decoded bytes", loc.SizeBytes)
	}
}
