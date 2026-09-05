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
// Postgres is where that has actually happened. The conditional insert built
// its params from four fields while the query wrote nine, so reconcile recorded
// encrypted objects as plaintext, and the unit tests stayed green because
// SQLite reached the same rows by a different path.
// -------------------------------------------------------------------------------

//go:build integration

package postgres

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

// TestPgInsertPaths_PreserveRepresentation drives every write path that creates
// an object_locations row and asserts the representation survives.
func TestPgInsertPaths_PreserveRepresentation(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	form := fullyPopulatedForm()

	tests := []struct {
		name  string
		write func(t *testing.T, key string)
	}{
		{
			name: "RecordObject",
			write: func(t *testing.T, key string) {
				if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 1024, Form: form}); err != nil {
					t.Fatalf("RecordObject: %v", err)
				}
			},
		},
		{
			name: "RecordObjectAndClearPending",
			write: func(t *testing.T, key string) {
				if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
					Key: key, Size: 1024, Form: form,
					Copies: []core.ObjectCopy{{Backend: "backend-a", IntentID: "intent-absent"}},
				}); err != nil {
					t.Fatalf("RecordObject with intent: %v", err)
				}
			},
		},
		{
			name: "ImportObject",
			write: func(t *testing.T, key string) {
				inserted, err := s.ImportObject(ctx, &core.ImportObjectRequest{
					Key: key, Backend: "backend-a", Size: 1024, Form: form,
				})
				if err != nil {
					t.Fatalf("ImportObject: %v", err)
				}
				if inserted != core.ImportInserted {
					t.Fatalf("ImportObject outcome = %s, want inserted", inserted)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := uniqueKey(t, "representation")
			defer func() { _, _, _ = s.DeleteObject(ctx, key) }()

			tt.write(t, key)
			assertFormPreserved(t, readBackOne(t, s, key), form)
		})
	}
}

// TestPgPendingPromote_PreservesRepresentation covers the reaper's path into
// object_locations. The intent is the only record of what was written once the
// PUT's commit has failed, so anything it drops is unrecoverable.
func TestPgPendingPromote_PreservesRepresentation(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	form := fullyPopulatedForm()
	key := uniqueKey(t, "promoted")
	defer func() { _, _, _ = s.DeleteObject(ctx, key) }()

	intent := &core.PendingObject{
		IntentID:                 key,
		ObjectKey:                key,
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
	defer func() { _ = s.DeletePending(ctx, key) }()

	// Read the intent back before promoting: the reaper resolves an intent it
	// re-read from the table, so a column lost on the pending write is already
	// gone by the time the promote runs.
	stale, err := s.GetStalePending(ctx, time.Now().Add(time.Minute), 100)
	if err != nil {
		t.Fatalf("GetStalePending: %v", err)
	}
	var found *core.PendingObject
	for i := range stale {
		if stale[i].IntentID == key {
			found = &stale[i]
		}
	}
	if found == nil {
		t.Fatalf("intent %s not returned by GetStalePending", key)
	}
	if found.CompressionAlgorithm != form.CompressionAlgorithm ||
		found.CompressionLevel != form.CompressionLevel ||
		found.CompressionFormatVersion != form.CompressionFormatVersion ||
		found.LogicalSize != form.LogicalSize {
		t.Errorf("pending row lost compression metadata: %+v", found)
	}

	if _, _, _, err := s.PromotePending(ctx, found); err != nil {
		t.Fatalf("PromotePending: %v", err)
	}
	assertFormPreserved(t, readBackOne(t, s, key), form)
}

// TestPgRecordReplica_PreservesRepresentation covers the replica insert, which
// copies from the source row inside the INSERT ... SELECT rather than from a
// caller-supplied form. A replica that loses the metadata describes bytes that
// are not what it claims: the copier moves the stored bytes verbatim.
func TestPgRecordReplica_PreservesRepresentation(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	form := fullyPopulatedForm()
	key := uniqueKey(t, "replicated")
	defer func() { _, _, _ = s.DeleteObject(ctx, key) }()

	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 1024, Form: form}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if _, inserted, err := s.RecordReplica(ctx, key, "backend-b", "backend-a"); err != nil || !inserted {
		t.Fatalf("RecordReplica: inserted=%v err=%v", inserted, err)
	}

	locs, err := s.GetAllObjectLocations(ctx, key)
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

// TestPgLegacyRow_ReadsAsUncompressed pins the upgrade case: a row written
// before these columns existed carries SQL NULL in all four and must read back
// as an object stored verbatim, not as one claiming a zero-length logical size
// or an unknown algorithm.
func TestPgLegacyRow_ReadsAsUncompressed(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "legacy")
	defer func() { _, _, _ = s.DeleteObject(ctx, key) }()

	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 512}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE object_locations
		    SET compression_algorithm = NULL, compression_level = NULL,
		        compression_format_version = NULL, logical_size = NULL
		  WHERE object_key = $1`, key); err != nil {
		t.Fatalf("null out compression columns: %v", err)
	}

	got := readBackOne(t, s, key)
	if got.CompressionAlgorithm != "" || got.CompressionLevel != "" ||
		got.CompressionFormatVersion != 0 || got.LogicalSize != 0 {
		t.Errorf("legacy row did not read as uncompressed: %+v", got)
	}
	if got.SizeBytes != 512 {
		t.Errorf("SizeBytes = %d, want 512", got.SizeBytes)
	}
}
