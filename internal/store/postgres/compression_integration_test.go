// -----------------------------------------------------------------------------
// Compression Admin Integration Tests
//
// Author: Alex Freidah
//
// Drives the bulk compression bindings against a real Postgres. The two
// listings are complements, and getting either predicate wrong makes a pass
// either miss objects or re-encode what it just wrote. The update has to move
// backend_quotas.bytes_used with the size it writes, and has to rewrite the
// envelope columns: re-encrypting a copy mints a new base nonce and wrapped
// key, so a row left holding the old ones describes bytes nothing can decrypt.
// -----------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// encodedForm describes a copy stored both compressed and encrypted, which is
// the case that exercises every column at once.
func encodedForm() *core.StoredForm {
	return &core.StoredForm{
		Encrypted:                true,
		EncryptionKey:            []byte("old-packed-key"),
		KeyID:                    "key-old",
		PlaintextSize:            400,
		CompressionAlgorithm:     "zstd",
		CompressionLevel:         "default",
		CompressionFormatVersion: 1,
		LogicalSize:              1000,
	}
}

// findRewritable returns the row for key from a listing page, failing when the
// listing did not offer it.
func findRewritable(t *testing.T, rows []core.RewritableLocation, key string) core.RewritableLocation {
	t.Helper()
	for i := range rows {
		if rows[i].ObjectKey == key {
			return rows[i]
		}
	}
	t.Fatalf("listing did not offer %q", key)
	return core.RewritableLocation{}
}

// assertAbsent fails when a listing offered a key belonging to the other one.
func assertAbsent(t *testing.T, rows []core.RewritableLocation, key, listing string) {
	t.Helper()
	for i := range rows {
		if rows[i].ObjectKey == key {
			t.Errorf("the %s listing offered %q, which belongs to the other pass", listing, key)
		}
	}
}

// assertEnvelopeCarried checks a listed row describes the stored bytes fully
// enough for a rewrite to unwrap them and put them back.
func assertEnvelopeCarried(t *testing.T, got core.RewritableLocation) {
	t.Helper()
	if !got.Encrypted || got.KeyID != "key-old" || string(got.EncryptionKey) != "old-packed-key" {
		t.Errorf("envelope missing from the listing: %+v", got)
	}
	if got.PlaintextSize != 400 || got.LogicalSize != 1000 {
		t.Errorf("sizes = plaintext %d / logical %d, want 400 / 1000", got.PlaintextSize, got.LogicalSize)
	}
	if got.CompressionAlgorithm != "zstd" || got.CompressionFormatVersion != 1 {
		t.Errorf("encoding not described: %+v", got)
	}
}

// TestStoreInt_CompressionListings_AreComplements asserts a copy appears in
// exactly one of the two listings, so a compress pass never re-encodes what it
// wrote and a decompress pass never decodes what was never encoded.
func TestStoreInt_CompressionListings_AreComplements(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	verbatimKey := uniqueKey(t, "compress-verbatim")
	encodedKey := uniqueKey(t, "compress-encoded")
	if _, err := s.RecordObject(ctx, verbatimKey, "backend-a", 1000, nil); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if _, err := s.RecordObject(ctx, encodedKey, "backend-a", 300, encodedForm()); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	uncompressed, err := s.ListUncompressedLocations(ctx, 1000, 0)
	if err != nil {
		t.Fatalf("ListUncompressedLocations: %v", err)
	}
	compressed, err := s.ListCompressedLocations(ctx, 1000, 0)
	if err != nil {
		t.Fatalf("ListCompressedLocations: %v", err)
	}

	findRewritable(t, uncompressed, verbatimKey)
	assertAbsent(t, uncompressed, encodedKey, "uncompressed")
	assertAbsent(t, compressed, verbatimKey, "compressed")

	// The encryption columns ride along because a rewrite has to unwrap the
	// copy before it can touch the bytes.
	assertEnvelopeCarried(t, findRewritable(t, compressed, encodedKey))
}

// TestStoreInt_MarkObjectCompressed_MovesQuotaAndEnvelope asserts the update
// writes every column a rewritten copy needs and moves bytes_used by the
// difference between what it occupied and what it occupies now.
func TestStoreInt_MarkObjectCompressed_MovesQuotaAndEnvelope(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	resetBytesUsed(t, s, "backend-a")
	key := uniqueKey(t, "compress-quota")
	if _, err := s.RecordObject(ctx, key, "backend-a", 1000, nil); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	before := readBytesUsed(t, s, "backend-a")

	update := &core.CompressedUpdate{
		ObjectKey:     key,
		BackendName:   "backend-a",
		Algorithm:     "zstd",
		Level:         "default",
		FormatVersion: 1,
		SizeBytes:     260,
		PlaintextSize: 250,
		LogicalSize:   1000,
		EncryptionKey: []byte("new-packed-key"),
		KeyID:         "key-new",
	}
	if err := s.MarkObjectCompressed(ctx, update, 1000); err != nil {
		t.Fatalf("MarkObjectCompressed: %v", err)
	}

	if delta := readBytesUsed(t, s, "backend-a") - before; delta != -740 {
		t.Errorf("bytes_used delta = %d, want -740 after a 1000 byte object became 260", delta)
	}

	rows, err := s.GetAllObjectLocations(ctx, key)
	if err != nil {
		t.Fatalf("GetAllObjectLocations: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.SizeBytes != 260 || got.PlaintextSize != 250 || got.LogicalSize != 1000 {
		t.Errorf("sizes = stored %d / plaintext %d / logical %d, want 260 / 250 / 1000",
			got.SizeBytes, got.PlaintextSize, got.LogicalSize)
	}
	if got.CompressionAlgorithm != "zstd" || got.CompressionLevel != "default" || got.CompressionFormatVersion != 1 {
		t.Errorf("encoding not recorded: %+v", got)
	}
	if string(got.EncryptionKey) != "new-packed-key" || got.KeyID != "key-new" {
		t.Errorf("envelope = %q / %q, want the key the rewrite produced",
			got.EncryptionKey, got.KeyID)
	}
}

// TestStoreInt_MarkObjectCompressed_ClearsEncoding asserts the decompress
// direction stops the row claiming an encoding, so nothing downstream tries to
// decode bytes that are no longer encoded.
func TestStoreInt_MarkObjectCompressed_ClearsEncoding(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	resetBytesUsed(t, s, "backend-a")
	key := uniqueKey(t, "decompress-clear")
	if _, err := s.RecordObject(ctx, key, "backend-a", 300, encodedForm()); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	before := readBytesUsed(t, s, "backend-a")

	update := &core.CompressedUpdate{
		ObjectKey:   key,
		BackendName: "backend-a",
		SizeBytes:   1000,
	}
	if err := s.MarkObjectCompressed(ctx, update, 300); err != nil {
		t.Fatalf("MarkObjectCompressed: %v", err)
	}

	if delta := readBytesUsed(t, s, "backend-a") - before; delta != 700 {
		t.Errorf("bytes_used delta = %d, want 700 after a 300 byte object became 1000", delta)
	}

	rows, err := s.GetAllObjectLocations(ctx, key)
	if err != nil {
		t.Fatalf("GetAllObjectLocations: %v", err)
	}
	got := rows[0]
	if got.CompressionAlgorithm != "" || got.CompressionLevel != "" || got.CompressionFormatVersion != 0 {
		t.Errorf("row still claims an encoding: %+v", got)
	}
	if got.LogicalSize != 0 {
		t.Errorf("LogicalSize = %d, want 0 once the row is verbatim again", got.LogicalSize)
	}
	if got.SizeBytes != 1000 {
		t.Errorf("SizeBytes = %d, want the 1000 decoded bytes", got.SizeBytes)
	}
}
