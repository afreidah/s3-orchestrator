// -------------------------------------------------------------------------------
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
// -------------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"fmt"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

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

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestStoreInt_CompressionListings_AreComplements asserts a copy appears in
// exactly one of the two listings, so a compress pass never re-encodes what it
// wrote and a decompress pass never decodes what was never encoded.
func TestStoreInt_CompressionListings_AreComplements(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	verbatimKey := uniqueKey(t, "compress-verbatim")
	encodedKey := uniqueKey(t, "compress-encoded")
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: verbatimKey, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 1000}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: encodedKey, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 300, Form: encodedForm()}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	uncompressed, err := s.ListUncompressedLocations(ctx, 1000, core.Cursor{}, core.CompressionThresholds{MinSize: 0, MinRatio: 1, Level: "default"})
	if err != nil {
		t.Fatalf("ListUncompressedLocations: %v", err)
	}
	compressed, err := s.ListCompressedLocations(ctx, 1000, core.Cursor{})
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

// TestStoreInt_CompressionListings_PageByCursor asserts the listing resumes
// after the row it is handed rather than at an offset. decompress-existing
// rewrites the rows it reads, so each page it finishes leaves the predicate
// before the next page is asked for; an offset would land past the rows that
// moved up, and the pass would report a clean run having skipped them.
func TestStoreInt_CompressionListings_PageByCursor(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	const total = 6
	want := seedEncodedCopies(t, s, ctx, total)
	seen := walkCompressedByCursor(t, s, ctx, want, total)
	assertWalkedEachOnce(t, seen, total)
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// seedEncodedCopies records total encoded copies under this test's key prefix
// and reports the keys it wrote.
func seedEncodedCopies(t *testing.T, s *Store, ctx context.Context, total int) map[string]bool {
	t.Helper()
	want := map[string]bool{}
	for i := range total {
		key := uniqueKey(t, fmt.Sprintf("obj-%d", i))
		if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 300, Form: encodedForm()}); err != nil {
			t.Fatalf("RecordObject %s: %v", key, err)
		}
		want[key] = true
	}
	return want
}

// walkCompressedByCursor pages the compressed listing two rows at a time,
// carrying the cursor forward exactly as the bulk passes do, and counts how
// often each wanted key came back. Other tests share this table, so the walk
// starts at this test's own key prefix and ignores rows it did not seed.
func walkCompressedByCursor(t *testing.T, s *Store, ctx context.Context, want map[string]bool, total int) map[string]int {
	t.Helper()
	seen := map[string]int{}
	after := core.Cursor{ObjectKey: t.Name() + "/"}
	for range 4 * total {
		page, err := s.ListCompressedLocations(ctx, 2, after)
		if err != nil {
			t.Fatalf("ListCompressedLocations: %v", err)
		}
		if len(page) == 0 || len(seen) == total {
			return seen
		}
		countWanted(page, want, seen)
		last := page[len(page)-1]
		after = core.Cursor{ObjectKey: last.ObjectKey, BackendName: last.BackendName}
	}
	return seen
}

// countWanted folds one page into the tally, ignoring rows other tests seeded.
func countWanted(page []core.RewritableLocation, want map[string]bool, seen map[string]int) {
	for i := range page {
		if want[page[i].ObjectKey] {
			seen[page[i].ObjectKey]++
		}
	}
}

// assertWalkedEachOnce fails unless the walk returned every seeded row exactly
// once, which is the whole point of a cursor: no row skipped, none repeated.
func assertWalkedEachOnce(t *testing.T, seen map[string]int, total int) {
	t.Helper()
	if len(seen) != total {
		t.Errorf("walked %d of this test's %d rows: %v", len(seen), total, seen)
	}
	for key, n := range seen {
		if n != 1 {
			t.Errorf("%s returned %d times, want once", key, n)
		}
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestStoreInt_MarkObjectCompressed_MovesQuotaAndEnvelope asserts the update
// writes every column a rewritten copy needs and moves bytes_used by the
// difference between what it occupied and what it occupies now.
func TestStoreInt_MarkObjectCompressed_MovesQuotaAndEnvelope(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	resetBytesUsed(t, s, "backend-a")
	key := uniqueKey(t, "compress-quota")
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 1000}); err != nil {
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
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 300, Form: encodedForm()}); err != nil {
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

// TestStoreInt_CompressionStats_PerBackendTotals covers the dashboard read
// against a real Postgres. Only encoded copies count, and a backend holding
// none is absent rather than present as zero.
func TestStoreInt_CompressionStats_PerBackendTotals(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	encodedKey := uniqueKey(t, "stats-encoded")
	verbatimKey := uniqueKey(t, "stats-verbatim")
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: encodedKey, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 300, Form: encodedForm()}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: verbatimKey, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 900}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	stats, err := s.CompressionStats(ctx)
	if err != nil {
		t.Fatalf("CompressionStats: %v", err)
	}
	got, ok := stats["backend-a"]
	if !ok {
		t.Fatalf("backend-a missing from %+v", stats)
	}
	// Other tests in this package seed rows too, so assert the shape rather
	// than exact totals: the verbatim copy must not have been counted.
	if got.Objects < 1 {
		t.Errorf("Objects = %d, want at least the one encoded copy", got.Objects)
	}
	if got.LogicalBytes < got.StoredBytes {
		t.Errorf("logical %d < stored %d; the totals cannot describe a saving",
			got.LogicalBytes, got.StoredBytes)
	}
	if got.StoredBytes >= got.LogicalBytes {
		t.Errorf("stored %d >= logical %d; a verbatim copy was counted in",
			got.StoredBytes, got.LogicalBytes)
	}
}
