// -------------------------------------------------------------------------------
// Compression Column Round-Trip Tests
//
// Author: Alex Freidah
//
// Every path that writes an object_locations row is driven with a fully
// populated StoredForm and read back, because a write path that silently drops
// representation metadata records an object it cannot serve: the bytes on the
// backend are compressed and encrypted while the row claims otherwise.
//
// That is not hypothetical. The conditional insert did exactly this with the
// encryption columns, building its params from four fields while the query
// wrote nine, and reconcile recorded encrypted objects as plaintext until it
// was found in production.
// -------------------------------------------------------------------------------

package sqlite

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// fullyPopulatedForm is a StoredForm with every representation field set to a
// distinguishable value, so a column dropped on the way in or out shows up as
// a zero rather than blending into a neighbour.
func fullyPopulatedForm() *core.StoredForm {
	return &core.StoredForm{
		Encrypted:                true,
		EncryptionKey:            []byte("packed-nonce-and-wrapped-dek"),
		KeyID:                    "kid-1",
		PlaintextSize:            2048,
		ContentHash:              "abc123",
		CompressionAlgorithm:     "zstd-seekable",
		CompressionLevel:         "better",
		CompressionFormatVersion: 1,
		LogicalSize:              4096,
	}
}

// assertFormPreserved checks a read-back location against the form that wrote
// it.
func assertFormPreserved(t *testing.T, got *core.ObjectLocation, want *core.StoredForm) {
	t.Helper()
	if got.Encrypted != want.Encrypted {
		t.Errorf("Encrypted = %v, want %v", got.Encrypted, want.Encrypted)
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

// readBackOne reads the single copy of key and fails if there is not exactly
// one.
func readBackOne(t *testing.T, s *Store, key string) *core.ObjectLocation {
	t.Helper()
	locs, err := s.GetAllObjectLocations(context.Background(), key)
	if err != nil {
		t.Fatalf("GetAllObjectLocations(%s): %v", key, err)
	}
	if len(locs) != 1 {
		t.Fatalf("got %d copies of %s, want 1", len(locs), key)
	}
	return &locs[0]
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestInsertPaths_PreserveRepresentation drives every write path that creates
// an object_locations row and asserts the representation survives.
func TestInsertPaths_PreserveRepresentation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	form := fullyPopulatedForm()

	tests := []struct {
		name  string
		key   string
		write func(t *testing.T, s *Store, key string)
	}{
		{
			name: "RecordObject",
			key:  "bucket/record",
			write: func(t *testing.T, s *Store, key string) {
				if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Backend: "backend-a", Size: 1024, Form: form}); err != nil {
					t.Fatalf("RecordObject: %v", err)
				}
			},
		},
		{
			name: "RecordObjectAndClearPending",
			key:  "bucket/record-clear",
			write: func(t *testing.T, s *Store, key string) {
				if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
					Key: key, Backend: "backend-a", Size: 1024, Form: form, IntentID: "intent-x",
				}); err != nil {
					t.Fatalf("RecordObject with intent: %v", err)
				}
			},
		},
		{
			name: "ImportObject",
			key:  "bucket/import",
			write: func(t *testing.T, s *Store, key string) {
				outcome, err := s.ImportObject(ctx, &core.ImportObjectRequest{
					Key: key, Backend: "backend-a", Size: 1024, Form: form,
				})
				if err != nil {
					t.Fatalf("ImportObject: %v", err)
				}
				if outcome != core.ImportInserted {
					t.Fatalf("ImportObject outcome = %s, want inserted", outcome)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			tt.write(t, s, tt.key)
			assertFormPreserved(t, readBackOne(t, s, tt.key), form)
		})
	}
}

// TestPendingPromote_PreservesRepresentation covers the reaper's path into
// object_locations. The intent is the only record of what was written once the
// PUT's commit has failed, so anything it drops is unrecoverable.
func TestPendingPromote_PreservesRepresentation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	form := fullyPopulatedForm()

	intent := &core.PendingObject{
		IntentID:                 "intent-promote",
		ObjectKey:                "bucket/promoted",
		BackendName:              "backend-a",
		SizeBytes:                1024,
		Encrypted:                form.Encrypted,
		EncryptionKey:            form.EncryptionKey,
		KeyID:                    form.KeyID,
		PlaintextSize:            form.PlaintextSize,
		ContentHash:              form.ContentHash,
		CompressionAlgorithm:     form.CompressionAlgorithm,
		CompressionLevel:         form.CompressionLevel,
		CompressionFormatVersion: form.CompressionFormatVersion,
		LogicalSize:              form.LogicalSize,
	}
	if _, err := s.InsertPendingIfFits(ctx, intent); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}

	// Read the intent back before promoting: the reaper resolves an intent it
	// re-read from the table, so a column lost on the pending write is already
	// gone by the time the promote runs.
	stale, err := s.GetStalePending(ctx, time.Now().Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("GetStalePending: %v", err)
	}
	if len(stale) != 1 {
		t.Fatalf("got %d stale intents, want 1", len(stale))
	}
	if stale[0].CompressionAlgorithm != form.CompressionAlgorithm ||
		stale[0].CompressionLevel != form.CompressionLevel ||
		stale[0].CompressionFormatVersion != form.CompressionFormatVersion ||
		stale[0].LogicalSize != form.LogicalSize {
		t.Errorf("pending row lost compression metadata: %+v", stale[0])
	}

	if _, _, _, err := s.PromotePending(ctx, &stale[0]); err != nil {
		t.Fatalf("PromotePending: %v", err)
	}
	assertFormPreserved(t, readBackOne(t, s, "bucket/promoted"), form)
}

// TestRecordReplica_PreservesRepresentation covers the replica insert, which
// copies from the source row rather than from a caller-supplied form. A replica
// that loses the metadata describes bytes that are not what it claims: the
// copier moves the stored bytes verbatim.
func TestRecordReplica_PreservesRepresentation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	form := fullyPopulatedForm()

	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/replicated", Backend: "backend-a", Size: 1024, Form: form}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if _, inserted, err := s.RecordReplica(ctx, "bucket/replicated", "backend-b", "backend-a"); err != nil || !inserted {
		t.Fatalf("RecordReplica: inserted=%v err=%v", inserted, err)
	}

	locs, err := s.GetAllObjectLocations(ctx, "bucket/replicated")
	if err != nil {
		t.Fatalf("GetAllObjectLocations: %v", err)
	}
	if len(locs) != 2 {
		t.Fatalf("got %d copies, want 2", len(locs))
	}
	for i := range locs {
		t.Run(locs[i].BackendName, func(t *testing.T) {
			assertFormPreserved(t, &locs[i], form)
		})
	}
}

// TestLegacyRow_ReadsAsUncompressed pins the upgrade case: a row written before
// these columns existed carries SQL NULL in all four and must read back as an
// object stored verbatim, not as one claiming a zero-length logical size or an
// unknown algorithm.
func TestLegacyRow_ReadsAsUncompressed(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/legacy", "backend-a", 512)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE object_locations
		    SET compression_algorithm = NULL, compression_level = NULL,
		        compression_format_version = NULL, logical_size = NULL
		  WHERE object_key = ?`, "bucket/legacy"); err != nil {
		t.Fatalf("null out compression columns: %v", err)
	}

	got := readBackOne(t, s, "bucket/legacy")
	if got.CompressionAlgorithm != "" || got.CompressionLevel != "" ||
		got.CompressionFormatVersion != 0 || got.LogicalSize != 0 {
		t.Errorf("legacy row did not read as uncompressed: %+v", got)
	}
	if got.SizeBytes != 512 {
		t.Errorf("SizeBytes = %d, want 512", got.SizeBytes)
	}
}
