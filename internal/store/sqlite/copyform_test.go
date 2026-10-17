// -------------------------------------------------------------------------------
// SQLite Store - Stored Form Tests
//
// Author: Alex Freidah
//
// The form and object_size columns are generated in SQL from the same fields
// Go reads through StoredForm.Form and ObjectLocation.ObjectSize, so the parity
// tests here are what pin the two definitions together. The rest covers the
// copy listing and count every bulk pass selects its work through.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// The backend, layer values and seeded keys the form fixtures share.
const (
	formBackend   = "backend-a"
	formKeyID     = "kid"
	formDEK       = "wrapped-dek"
	formAlgorithm = "zstd"
	formLevel     = "default"
	formHash      = "sha256:abc"
	keyPlain      = "bucket/plain"
	keyHashed     = "bucket/hashed"
	keySealed     = "bucket/sealed"
	keyLost       = "bucket/lost"
	keyPacked     = "bucket/packed"
	keyOwned      = "bucket/owned"
)

// paritySize is the stored size of every parity row.
const paritySize = 1000

// backfillCopies is the filter the checksum backfill lists by: managed copies
// with no content hash.
var backfillCopies = core.FormFilter{Without: core.StoredHashed, ManagedOnly: true}

// unreadableCopies is the filter the unreadable purge lists by: copies
// encrypted with no key.
var unreadableCopies = core.FormFilter{With: core.StoredEncrypted, Without: core.StoredKeyed}

// listByForm returns the first page of copies f selects, failing the test on
// error.
func listByForm(t *testing.T, s *Store, f *core.FormFilter) []core.ObjectLocation {
	t.Helper()
	locs, err := s.ListCopiesByForm(context.Background(), f, 100, core.Cursor{})
	if err != nil {
		t.Fatalf("ListCopiesByForm(%+v): %v", f, err)
	}
	return locs
}

// recordForm records one copy of key on the fixture backend through
// RecordObject, stored as form.
func recordForm(t *testing.T, s *Store, key string, size int64, form *core.StoredForm) {
	t.Helper()
	if _, _, err := s.RecordObject(context.Background(), &core.RecordObjectRequest{
		Key: key, Copies: []core.ObjectCopy{{Backend: formBackend}}, Size: size, Form: form,
	}); err != nil {
		t.Fatalf("RecordObject(%s): %v", key, err)
	}
}

// formFromFlags builds a stored form carrying the fields each flag in set
// stands for, with sizes distinct enough that ObjectSize shows which it read.
func formFromFlags(set core.StoredFlags) *core.StoredForm {
	form := &core.StoredForm{}
	if set.Has(core.StoredCompressed) {
		form.CompressionAlgorithm = formAlgorithm
		form.CompressionLevel = formLevel
		form.CompressionFormatVersion = 1
		form.LogicalSize = 5000
	}
	if set.Has(core.StoredEncrypted) {
		form.Encrypted = true
		form.KeyID = formKeyID
		form.PlaintextSize = 3000
	}
	if set.Has(core.StoredHashed) {
		form.ContentHash = formHash
	}
	if set.Has(core.StoredKeyed) {
		form.EncryptionKey = []byte(formDEK)
	}
	return form
}

// parityForms is every combination of the four flags plus the edge cases a
// zero or empty field makes: an encrypted empty object, an empty key slice, and
// an encoding with no recorded logical size.
func parityForms() map[string]*core.StoredForm {
	forms := make(map[string]*core.StoredForm)
	for set := range core.StoredFlags(16) {
		forms[fmt.Sprintf("bucket/flags-%02d", set)] = formFromFlags(set)
	}
	forms["bucket/empty-encrypted"] = &core.StoredForm{Encrypted: true, EncryptionKey: []byte(formDEK), KeyID: formKeyID}
	forms["bucket/empty-key"] = &core.StoredForm{Encrypted: true, EncryptionKey: []byte{}, KeyID: formKeyID, PlaintextSize: 10}
	forms["bucket/no-logical-size"] = &core.StoredForm{CompressionAlgorithm: formAlgorithm, CompressionLevel: formLevel}
	return forms
}

// assertFormParity reads the generated columns of every parity row back and
// compares them with what Go derives from the form the insert records, so a
// key with no envelope reads as plaintext.
func assertFormParity(t *testing.T, s *Store, forms map[string]*core.StoredForm) {
	t.Helper()
	ctx := context.Background()
	for key, form := range forms {
		var gotForm core.StoredFlags
		var gotSize int64
		if err := s.db.QueryRowContext(ctx,
			`SELECT form, object_size FROM object_locations WHERE object_key = ?`, key,
		).Scan(&gotForm, &gotSize); err != nil {
			t.Fatalf("read generated columns for %s: %v", key, err)
		}
		loc := core.ObjectLocation{ObjectKey: key, SizeBytes: paritySize, StoredForm: form.Recorded()}
		if gotForm != loc.Form() {
			t.Errorf("%s: form column = %s, Go Form() = %s", key, gotForm, loc.Form())
		}
		if gotSize != loc.ObjectSize() {
			t.Errorf("%s: object_size column = %d, Go ObjectSize() = %d", key, gotSize, loc.ObjectSize())
		}
	}
}

// seedForms records one copy per interesting form: plaintext, hashed, sealed
// with its key, encrypted with no key, and compressed.
func seedForms(t *testing.T, s *Store) {
	t.Helper()
	recordForm(t, s, keyPlain, 20, nil)
	recordForm(t, s, keyHashed, 20, &core.StoredForm{ContentHash: formHash})
	recordForm(t, s, keySealed, 20, &core.StoredForm{
		Encrypted: true, EncryptionKey: []byte(formDEK), KeyID: formKeyID, PlaintextSize: 10, ContentHash: formHash,
	})
	recordForm(t, s, keyLost, 20, &core.StoredForm{Encrypted: true, KeyID: formKeyID, PlaintextSize: 10})
	recordForm(t, s, keyPacked, 20, &core.StoredForm{
		CompressionAlgorithm: formAlgorithm, CompressionLevel: formLevel, LogicalSize: 50, ContentHash: formHash,
	})
}

// formMaskCases pairs each With/Without mask with the seeded keys it selects,
// in listing order.
var formMaskCases = []struct {
	with, without core.StoredFlags
	want          []string
}{
	{0, 0, []string{keyHashed, keyLost, keyPacked, keyPlain, keySealed}},
	{core.StoredEncrypted, 0, []string{keyLost, keySealed}},
	{0, core.StoredEncrypted, []string{keyHashed, keyPacked, keyPlain}},
	{core.StoredHashed, core.StoredCompressed, []string{keyHashed, keySealed}},
	{core.StoredEncrypted, core.StoredKeyed, []string{keyLost}},
	{core.StoredCompressed | core.StoredHashed, 0, []string{keyPacked}},
	{0, core.StoredHashed, []string{keyLost, keyPlain}},
}

// -------------------------------------------------------------------------
// GENERATED COLUMNS
// -------------------------------------------------------------------------

// TestStoredFormParity_SchemaMatchesGo pins the form and object_size columns
// schema.sql generates to the Go definitions, for every flag combination.
func TestStoredFormParity_SchemaMatchesGo(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	forms := parityForms()
	for key, form := range forms {
		recordForm(t, s, key, paritySize, form)
	}
	assertFormParity(t, s, forms)
}

// TestStoredFormParity_MigrationMatchesGo pins the columns the migration adds to
// an existing table to the same Go definitions, computed over rows the tx
// adapter inserted before the columns existed.
func TestStoredFormParity_MigrationMatchesGo(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	rewindToSchemaVersion(t, s, 25)

	forms := parityForms()
	tx, err := s.rawDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	a := &sqliteTxAdapter{q: tx}
	for key, form := range forms {
		if err := a.InsertObjectLocation(ctx, core.ObjectFromStoredForm(key, formBackend, key, paritySize, form, nil)); err != nil {
			_ = tx.Rollback()
			t.Fatalf("InsertObjectLocation(%s): %v", key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if err := s.RunMigrations(ctx); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	assertFormParity(t, s, forms)
}

// -------------------------------------------------------------------------
// LISTING AND COUNTING BY FORM
// -------------------------------------------------------------------------

// TestListCopiesByForm_Masks verifies a copy is listed only when it carries
// every flag of With and none of Without.
func TestListCopiesByForm_Masks(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seedForms(t, s)
	for _, tc := range formMaskCases {
		got := keysOf(listByForm(t, s, &core.FormFilter{With: tc.with, Without: tc.without}))
		if !slices.Equal(got, tc.want) {
			t.Errorf("with %s without %s = %v, want %v", tc.with, tc.without, got, tc.want)
		}
	}
}

// TestCountCopiesByForm_MatchesTheListing verifies the count reports exactly
// what the listing would return for the same masks, so a dashboard figure and
// the pass it describes cannot disagree.
func TestCountCopiesByForm_MatchesTheListing(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	if n, err := s.CountCopiesByForm(ctx, 0, 0); err != nil || n != 0 {
		t.Fatalf("empty store count = %d, %v; want 0", n, err)
	}
	seedForms(t, s)
	for _, tc := range formMaskCases {
		n, err := s.CountCopiesByForm(ctx, tc.with, tc.without)
		if err != nil {
			t.Fatalf("CountCopiesByForm: %v", err)
		}
		if n != int64(len(tc.want)) {
			t.Errorf("with %s without %s counted %d, want %d", tc.with, tc.without, n, len(tc.want))
		}
	}
}

// TestListCopiesByForm_ManagedOnly verifies ManagedOnly leaves out a copy the
// orchestrator does not manage, and that without it the copy is listed.
func TestListCopiesByForm_ManagedOnly(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	recordForm(t, s, keyOwned, 100, nil)
	if _, err := s.ImportObject(context.Background(), &core.ImportObjectRequest{
		Key: "stray.txt", Backend: formBackend, Size: 400, Unmanaged: true,
	}); err != nil {
		t.Fatalf("ImportObject(unmanaged): %v", err)
	}

	if got := keysOf(listByForm(t, s, &core.FormFilter{Without: core.StoredHashed})); len(got) != 2 {
		t.Errorf("every unhashed copy = %v, want the owned and the stray", got)
	}
	if got := keysOf(listByForm(t, s, &backfillCopies)); !slices.Equal(got, []string{keyOwned}) {
		t.Errorf("managed unhashed copies = %v, want only %s", got, keyOwned)
	}
}

// TestListCopiesByForm_CandidateFloorReadsObjectSize verifies the size floor
// judges the object the client wrote rather than the stored bytes: an
// encrypted copy whose envelope clears the floor but whose plaintext does not
// is not a candidate.
func TestListCopiesByForm_CandidateFloorReadsObjectSize(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	recordForm(t, s, keySealed, 9000, &core.StoredForm{
		Encrypted: true, EncryptionKey: []byte(formDEK), KeyID: formKeyID, PlaintextSize: 100,
	})

	floor := core.CompressionThresholds{MinSize: 4096, MinRatio: 1, Level: formLevel}
	if keys := listKeys(t, s, floor); len(keys) != 0 {
		t.Errorf("listing offered %v, want nothing: the 100-byte plaintext is under the floor", keys)
	}
	floor.MinSize = 64
	if keys := listKeys(t, s, floor); len(keys) != 1 {
		t.Errorf("listing offered %v, want the copy once the floor is under its plaintext", keys)
	}
}
