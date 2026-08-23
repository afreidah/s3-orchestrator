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
