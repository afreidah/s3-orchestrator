// -------------------------------------------------------------------------------
// Reconcile Cursor Collation - Integration Tests
//
// Author: Alex Freidah
//
// Guards the byte-order reconcile cursor fix (issue #1030). The sorted-merge in
// internal/proxy/reconcile compares keys in Go byte order against S3
// ListObjectsV2 (UTF-8 byte ordered), so the DB cursor query must also emit keys
// in byte order. ListObjectsByBackendKeyAsc enforces this with COLLATE "C" on
// both the cursor predicate and ORDER BY. These tests seed an adversarial key
// set that sorts differently under byte order vs an ICU locale collation, then
// assert (1) the cursor pages keys in byte order, with an ICU control proving
// the keys are collation-sensitive, and (2) reconcile of an identical DB/backend
// key set converges to a pure no-op across two consecutive passes (the bug
// manifested as nonzero import/remove counts that oscillate and never converge).
// -------------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"log/slog"
	"slices"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy/reconcile"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// adversarialKeys sorts differently under byte order vs a locale/ICU collation:
// mixed case, punctuation, and digits exercise the collation boundary an ICU
// ordering would reshuffle (e.g. case-insensitive grouping, punctuation
// weighting) but byte order keeps strictly by code point.
var adversarialKeys = []string{
	"A", "a", "B", "b", "Zoo", "zoo",
	"a/b", "a-b", "a.b", "a_b",
	"A1", "a1", "ab", "Ab",
}

// seedAdversarialKeys registers backendName then records every adversarial key
// against it so the object_locations table holds the full collation-sensitive
// set for one backend. The registration satisfies the backend_name FK; nil
// encryption meta keeps the rows plaintext; reconcile only reads key + size.
func seedAdversarialKeys(t *testing.T, s *Store, backendName string) {
	t.Helper()
	ctx := context.Background()
	if err := s.SyncQuotaLimits(ctx, []config.BackendConfig{
		{Name: backendName, QuotaBytes: 1 << 30},
	}); err != nil {
		t.Fatalf("SyncQuotaLimits(%q): %v", backendName, err)
	}
	for _, k := range adversarialKeys {
		if _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: k, Backend: backendName, Size: 1}); err != nil {
			t.Fatalf("RecordObject(%q, %q): %v", k, backendName, err)
		}
	}
}

// -------------------------------------------------------------------------
// CURSOR BYTE ORDER
// -------------------------------------------------------------------------

// TestStoreInt_ListObjectsByBackendKeyAsc_ByteOrder verifies the reconcile
// cursor pages keys in Go byte order across pagination, and proves via an ICU
// control that the seeded keys are collation-sensitive (so the assertion is
// meaningful even when the container's default collation already byte-orders).
func TestStoreInt_ListObjectsByBackendKeyAsc_ByteOrder(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	const backendName = "recon-collate-cursor-be"

	seedAdversarialKeys(t, s, backendName)

	// --- log the container's database collation for diagnosis ---
	var datcollate string
	if err := s.pool.QueryRow(ctx,
		`SELECT datcollate FROM pg_database WHERE datname = current_database()`,
	).Scan(&datcollate); err != nil {
		t.Fatalf("read datcollate: %v", err)
	}
	t.Logf("database datcollate = %q", datcollate)

	// --- page through the cursor with a small limit to exercise pagination ---
	var got []string
	cursor := ""
	for {
		rows, err := s.ListObjectsByBackendKeyAsc(ctx, backendName, cursor, 3)
		if err != nil {
			t.Fatalf("ListObjectsByBackendKeyAsc(cursor=%q): %v", cursor, err)
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			got = append(got, r.ObjectKey)
		}
		cursor = rows[len(rows)-1].ObjectKey
	}

	// --- the cursor must emit keys in Go byte order ---
	want := slices.Clone(adversarialKeys)
	slices.Sort(want) // Go string sort is byte (code point) order.
	if !slices.Equal(got, want) {
		t.Fatalf("cursor order mismatch:\n got  %q\n want %q", got, want)
	}

	// --- ICU control: prove the keys reorder under a locale collation ---
	icuRows, err := s.pool.Query(ctx,
		`SELECT object_key FROM object_locations
		 WHERE backend_name = $1
		 ORDER BY object_key COLLATE "en-US-x-icu"`, backendName)
	if err != nil {
		t.Fatalf("ICU control query (is the en-US-x-icu collation available?): %v", err)
	}
	var icuOrder []string
	for icuRows.Next() {
		var k string
		if err := icuRows.Scan(&k); err != nil {
			icuRows.Close()
			t.Fatalf("scan ICU row: %v", err)
		}
		icuOrder = append(icuOrder, k)
	}
	icuRows.Close()
	if err := icuRows.Err(); err != nil {
		t.Fatalf("ICU control rows: %v", err)
	}

	// --- if ICU order equals byte order the keys are not adversarial enough ---
	if slices.Equal(icuOrder, want) {
		t.Fatalf("ICU order equals byte order; key set is not collation-sensitive "+
			"(or ICU collation unavailable). icu=%q byte=%q", icuOrder, want)
	}
}

// -------------------------------------------------------------------------
// RECONCILE CONVERGENCE
// -------------------------------------------------------------------------

// fakeObjectLister is a reconcile.ObjectLister that replays a fixed, byte-sorted
// key set as backend.ListedObject pages. With the DB holding the identical set,
// a correct reconcile is a pure no-op.
type fakeObjectLister struct {
	keys []string
}

// ListObjects emits the configured keys (byte-sorted) in a single page so the
// S3-side stream matches the byte order the merge expects.
func (f *fakeObjectLister) ListObjects(_ context.Context, _ string, fn func([]backend.ListedObject) error) error {
	page := make([]backend.ListedObject, 0, len(f.keys))
	for _, k := range f.keys {
		page = append(page, backend.ListedObject{Key: k, SizeBytes: 1})
	}
	return fn(page)
}

// runReconcile wires fresh streams + Result and runs one full sorted-merge of
// the fake backend against the DB cursor for backendName, returning the counts.
func runReconcile(t *testing.T, s *Store, backendName string, keys []string) reconcile.Result {
	t.Helper()
	ctx := context.Background()

	fake := &fakeObjectLister{keys: keys}
	db := reconcile.NewDBCursorStream(reconcile.DBCursorStreamDeps{Store: s, BackendName: backendName})
	s3 := reconcile.NewS3KeyStream(ctx, fake, nil, nil, "")
	defer s3.Stop()
	defer db.Stop()

	var res reconcile.Result
	importer := func(_ context.Context, _, _ string, _ int64, _ bool) (core.ImportOutcome, error) {
		return core.ImportInserted, nil
	}
	deleter := func(_ context.Context, _, _ string) error { return nil }
	onImp := reconcile.ImportHandler(slog.Default(), backendName, importer, &res)
	onDel := reconcile.DeleteHandler(slog.Default(), backendName, deleter, &res)

	if err := reconcile.Sorted(ctx, s3, db, onImp, onDel); err != nil {
		t.Fatalf("reconcile.Sorted: %v", err)
	}
	return res
}

// TestStoreInt_ReconcileConverges_AdversarialKeys verifies that reconciling an
// adversarial key set present identically on both the backend and the DB
// produces zero imports and zero removes, and stays a no-op on a second pass.
// Before the byte-order cursor fix the locale-collated cursor mis-paired keys,
// reporting false imports/removes that oscillate and never converge.
func TestStoreInt_ReconcileConverges_AdversarialKeys(t *testing.T) {
	s := adapterPgStore(t)
	const backendName = "recon-collate-converge-be"

	seedAdversarialKeys(t, s, backendName)

	// --- backend lists the same keys, byte-sorted, as the DB holds ---
	keys := slices.Clone(adversarialKeys)
	slices.Sort(keys)

	// --- first pass: identical sets must reconcile to a pure no-op ---
	first := runReconcile(t, s, backendName, keys)
	if first.Imported != 0 || first.Removed != 0 {
		t.Fatalf("first pass not a no-op: imported=%d removed=%d", first.Imported, first.Removed)
	}

	// --- second pass: still 0/0; nonzero here is the oscillation signature ---
	second := runReconcile(t, s, backendName, keys)
	if second.Imported != 0 || second.Removed != 0 {
		t.Fatalf("second pass not a no-op (oscillation): imported=%d removed=%d", second.Imported, second.Removed)
	}
}
