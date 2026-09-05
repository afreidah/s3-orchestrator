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
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// anyCandidate is the threshold set that excludes nothing, so a listing test
// sees every verbatim copy regardless of size or any recorded measurement.
func anyCandidate() core.CompressionThresholds {
	return core.CompressionThresholds{MinSize: 0, MinRatio: 1, Level: "default"}
}

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

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestMoveObjectLocation_CarriesRepresentation covers the rebalance and drain
// path. The bytes are moved verbatim, so the row that lands on the destination
// has to say exactly what the source row said about them.
func TestMoveObjectLocation_CarriesRepresentation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/moved", Backend: "backend-a", Size: 4096, Form: compressedForm()}); err != nil {
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
	if _, err := s.InsertPendingIfFits(ctx, &intent); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}
	stale, _ := s.GetStalePending(ctx, time.Now().Add(time.Hour), 10)
	if len(stale) != 1 {
		t.Fatal("seed: pending row missing")
	}

	if _, _, _, err := s.PromotePending(ctx, &stale[0]); err != nil {
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
	if _, err := s.InsertPendingIfFits(ctx, &intent); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}
	stale, _ := s.GetStalePending(ctx, time.Now().Add(time.Hour), 10)
	if len(stale) != 1 {
		t.Fatal("seed: pending row missing")
	}

	_, _, deltas, err := s.PromotePending(ctx, &stale[0])
	if err != nil {
		t.Fatalf("PromotePending: %v", err)
	}
	if got := deltas["backend-a"]; got != 4096 {
		t.Errorf("delta = %d, want the 4096 that landed on the backend", got)
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

	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/replicated", Backend: "backend-a", Size: 4096, Form: compressedForm()}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	if _, _, err := s.RecordReplica(ctx, "bucket/replicated", "backend-b", "backend-a"); err != nil {
		t.Fatalf("RecordReplica: %v", err)
	}

	assertCarried(t, locationOn(t, s, "bucket/replicated", "backend-b"), 4096)
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// probeOn reads back the measurement recorded against one copy.
func probeOn(t *testing.T, s *Store, key, backendName string) (int64, string) {
	t.Helper()
	var (
		size  sql.NullInt64
		level sql.NullString
	)
	err := s.db.QueryRowContext(context.Background(),
		`SELECT compression_probe_size, compression_probe_level
		 FROM object_locations WHERE object_key = ? AND backend_name = ?`,
		key, backendName,
	).Scan(&size, &level)
	if err != nil {
		t.Fatalf("read probe for %s on %s: %v", key, backendName, err)
	}
	return size.Int64, level.String
}

// listKeys returns the keys the uncompressed listing offers under thresholds.
func listKeys(t *testing.T, s *Store, thresholds core.CompressionThresholds) []string {
	t.Helper()
	rows, err := s.ListUncompressedLocations(context.Background(), 10, core.Cursor{}, thresholds)
	if err != nil {
		t.Fatalf("ListUncompressedLocations: %v", err)
	}
	keys := make([]string, len(rows))
	for i := range rows {
		keys[i] = rows[i].ObjectKey
	}
	return keys
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestMoveObjectLocation_CarriesCompressionProbe covers the rebalance path for
// the measurement rather than the stored form. The bytes move verbatim, so what
// the encoder measured about them still holds; a destination row that lands
// without it has the next compression pass download the copy to learn what the
// source row already knew, which is metered egress spent for nothing.
func TestMoveObjectLocation_CarriesCompressionProbe(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/random.bin", Backend: "backend-a", Size: 4096}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if err := s.RecordCompressionProbe(ctx, &core.CompressionProbe{
		ObjectKey: "bucket/random.bin", BackendName: "backend-a", Size: 4090, Level: "default",
	}); err != nil {
		t.Fatalf("RecordCompressionProbe: %v", err)
	}

	if _, err := s.MoveObjectLocation(ctx, "bucket/random.bin", "backend-a", "backend-b"); err != nil {
		t.Fatalf("MoveObjectLocation: %v", err)
	}

	size, level := probeOn(t, s, "bucket/random.bin", "backend-b")
	if size != 4090 || level != "default" {
		t.Errorf("probe on the destination = (%d, %q), want (4090, \"default\")", size, level)
	}
}

// TestListUncompressedLocations_ExcludesRecordedDeclines is the point of
// recording a measurement at all: a copy already known not to shrink enough is
// not offered again, so a second pass does not pay to re-measure it.
//
// The exclusion is judged against the current settings rather than stored as a
// verdict, so loosening min_ratio returns the copy with no read, and a level
// change does too: a measurement taken at another level describes an encoding
// this pass would not produce.
func TestListUncompressedLocations_ExcludesRecordedDeclines(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/random.bin", Backend: "backend-a", Size: 1000}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if err := s.RecordCompressionProbe(ctx, &core.CompressionProbe{
		ObjectKey: "bucket/random.bin", BackendName: "backend-a", Size: 900, Level: "default",
	}); err != nil {
		t.Fatalf("RecordCompressionProbe: %v", err)
	}

	strict := core.CompressionThresholds{MinRatio: 0.5, Level: "default"}
	if keys := listKeys(t, s, strict); len(keys) != 0 {
		t.Errorf("listing offered %v, want nothing: 900/1000 does not reach a 0.5 ratio", keys)
	}

	loosened := core.CompressionThresholds{MinRatio: 0.95, Level: "default"}
	if keys := listKeys(t, s, loosened); len(keys) != 1 {
		t.Errorf("listing offered %v, want the copy back: 900/1000 now reaches the ratio", keys)
	}

	otherLevel := core.CompressionThresholds{MinRatio: 0.5, Level: "better"}
	if keys := listKeys(t, s, otherLevel); len(keys) != 1 {
		t.Errorf("listing offered %v, want the copy back: the measurement is from another level", keys)
	}
}

// TestListUncompressedLocations_ExcludesBelowMinSize checks the size floor is
// applied by the listing. A copy too small to be worth encoding is answered
// from its own row, so selecting it only to decline it would spend a page slot
// on every pass forever.
func TestListUncompressedLocations_ExcludesBelowMinSize(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/small.txt", Backend: "backend-a", Size: 100}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/large.bin", Backend: "backend-a", Size: 9000}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	keys := listKeys(t, s, core.CompressionThresholds{MinSize: 4096, MinRatio: 1, Level: "default"})
	if len(keys) != 1 || keys[0] != "bucket/large.bin" {
		t.Errorf("listing offered %v, want only bucket/large.bin", keys)
	}
}

// TestListUncompressedLocations_SelectsOnlyVerbatim checks the listing that
// drives compress-existing: it must offer copies with no encoding and skip the
// ones that already have one, or a pass would re-encode what it just wrote.
func TestListUncompressedLocations_SelectsOnlyVerbatim(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/plain", Backend: "backend-a", Size: 100}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/encoded", Backend: "backend-a", Size: 40, Form: compressedForm()}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	verbatim, err := s.ListUncompressedLocations(ctx, 10, core.Cursor{}, anyCandidate())
	if err != nil {
		t.Fatalf("ListUncompressedLocations: %v", err)
	}
	if len(verbatim) != 1 || verbatim[0].ObjectKey != "bucket/plain" {
		t.Fatalf("uncompressed listing = %+v, want only bucket/plain", verbatim)
	}

	encoded, err := s.ListCompressedLocations(ctx, 10, core.Cursor{})
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

// TestListUncompressedLocations_PagesByCursor checks the listing resumes after
// the row it is given rather than at an offset. The pass that drives it rewrites
// the rows it reads, so by the time it asks for the next page the earlier ones
// have left the predicate; an offset would land past the rows that moved up and
// the walk would skip them.
func TestListUncompressedLocations_PagesByCursor(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	const total = 5
	for i := range total {
		key := fmt.Sprintf("bucket/obj-%d", i)
		if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Backend: "backend-a", Size: 100}); err != nil {
			t.Fatalf("RecordObject %s: %v", key, err)
		}
	}

	seen := map[string]int{}
	var after core.Cursor
	for range total {
		page, err := s.ListUncompressedLocations(ctx, 2, after, anyCandidate())
		if err != nil {
			t.Fatalf("ListUncompressedLocations: %v", err)
		}
		if len(page) == 0 {
			break
		}
		for _, loc := range page {
			seen[loc.ObjectKey]++
		}
		last := page[len(page)-1]
		after = core.Cursor{ObjectKey: last.ObjectKey, BackendName: last.BackendName}
	}

	if len(seen) != total {
		t.Errorf("walked %d distinct rows, want %d: %v", len(seen), total, seen)
	}
	for key, n := range seen {
		if n != 1 {
			t.Errorf("%s returned %d times, want once", key, n)
		}
	}
}

// TestMarkObjectCompressed_MovesQuota pins the half that is easy to get wrong:
// a rewrite changes how many bytes the copy occupies, and the backend counter
// has to follow it or quota drifts by the compression ratio on every object.
func TestMarkObjectCompressed_MovesQuota(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/shrinking", Backend: "backend-a", Size: 1000}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	// The counter is what this rewrite moves, and a write no longer sets it,
	// so the object's bytes are put there before the rewrite runs.
	seedBytesUsed(t, s, "backend-a", 1000)
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

	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/expanding", Backend: "backend-a", Size: 250, Form: compressedForm()}); err != nil {
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

// TestCompressionStats_ReportsPerBackendTotals covers what the dashboard reads.
// Only encoded copies count: including the verbatim ones would report a ratio
// no encoder produced, and a backend holding none is absent rather than zero so
// "nothing compressed here" stays distinguishable from "compressed to nothing".
func TestCompressionStats_ReportsPerBackendTotals(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/encoded-a", Backend: "backend-a", Size: 250, Form: compressedForm()}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/encoded-b", Backend: "backend-a", Size: 150, Form: compressedForm()}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/verbatim", Backend: "backend-b", Size: 900}); err != nil {
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
	if got.Objects != 2 {
		t.Errorf("Objects = %d, want 2", got.Objects)
	}
	// compressedForm carries a logical size of 8192 per copy.
	if got.LogicalBytes != 16384 {
		t.Errorf("LogicalBytes = %d, want 16384", got.LogicalBytes)
	}
	if got.StoredBytes != 400 {
		t.Errorf("StoredBytes = %d, want 400", got.StoredBytes)
	}
	if _, ok := stats["backend-b"]; ok {
		t.Error("backend-b holds nothing encoded and must be absent, not zero")
	}
}

// TestRecordCompressionProbe_ReportsWriteFailure verifies a failed measurement
// write is reported rather than swallowed. Recording a measurement is what
// takes a declined copy out of the compression listing, so a silent failure
// leaves the pass re-downloading and re-encoding that copy on every future run
// while reporting clean.
func TestRecordCompressionProbe_ReportsWriteFailure(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.db.ExecContext(ctx, `DROP TABLE object_locations`); err != nil {
		t.Fatalf("drop object_locations: %v", err)
	}

	err := s.RecordCompressionProbe(ctx, &core.CompressionProbe{
		ObjectKey: "bucket/random.bin", BackendName: "backend-a", Size: 4090, Level: "default",
	})
	if err == nil {
		t.Fatal("RecordCompressionProbe returned nil against a missing table")
	}
	if !strings.Contains(err.Error(), "record compression probe") {
		t.Errorf("error = %q, want it to name the operation", err)
	}
}

// TestTxAdapterRecordCompressionProbe_ReportsWriteFailure covers the in-transaction
// form, which is the one a move uses to carry a measurement onto the destination
// row. It has to surface the failure so the move rolls back whole: a committed
// move that dropped the measurement is the same wasted re-encode as above, only
// now the copy has changed backends and nothing points at what was lost.
func TestTxAdapterRecordCompressionProbe_ReportsWriteFailure(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	err := s.WithTx(ctx, func(ctx context.Context, tx core.TxAdapter) error {
		adapter, ok := tx.(*sqliteTxAdapter)
		if !ok {
			t.Fatalf("WithTx handed back %T, want *sqliteTxAdapter", tx)
		}
		// Dropped inside the transaction, so the adapter's UPDATE fails and the
		// rollback puts the table back for anything else sharing this store.
		if _, err := adapter.tx.ExecContext(ctx, `DROP TABLE object_locations`); err != nil {
			t.Fatalf("drop object_locations: %v", err)
		}
		return adapter.RecordCompressionProbe(ctx, &core.CompressionProbe{
			ObjectKey: "bucket/random.bin", BackendName: "backend-a", Size: 4090, Level: "default",
		})
	})
	if err == nil {
		t.Fatal("RecordCompressionProbe returned nil against a missing table")
	}
	if !strings.Contains(err.Error(), "record compression probe") {
		t.Errorf("error = %q, want it to name the operation", err)
	}
}
