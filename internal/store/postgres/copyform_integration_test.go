// -------------------------------------------------------------------------------
// Stored Form Integration Tests
//
// Author: Alex Freidah
//
// The form and object_size columns are generated in SQL from the same fields
// Go reads through StoredForm.Form and ObjectLocation.ObjectSize, so the parity
// test here is what pins the two definitions together on Postgres. The rest
// covers the copy listing and count every bulk pass selects its work through.
// Other tests share the table, so listings are read from this test's own key
// prefix and counts are compared as deltas.
// -------------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// The backends and layer values the form fixtures share.
const (
	formBackend      = "backend-a"
	formOtherBackend = "backend-b"
	formKeyID        = "kid"
	formDEK          = "wrapped-dek"
	formAlgorithm    = "zstd"
	formLevel        = "default"
	formHash         = "sha256:abc"
)

// The key suffixes the mask fixtures seed under the test's prefix.
const (
	suffixPlain  = "plain"
	suffixHashed = "hashed"
	suffixSealed = "sealed"
	suffixLost   = "lost"
	suffixPacked = "packed"
)

// paritySize is the stored size of every parity row.
const paritySize = 1000

// backfillCopies is the filter the checksum backfill lists by: managed copies
// with no content hash.
var backfillCopies = core.FormFilter{Without: core.StoredHashed, ManagedOnly: true}

// unreadableCopies is the filter the unreadable purge lists by: copies
// encrypted with no key.
var unreadableCopies = core.FormFilter{With: core.StoredEncrypted, Without: core.StoredKeyed}

// pgCandidatesUnder is the filter compress-existing lists by: uncompressed
// copies still worth encoding under thresholds.
func pgCandidatesUnder(thresholds core.CompressionThresholds) *core.FormFilter {
	return &core.FormFilter{Without: core.StoredCompressed, Candidates: &thresholds}
}

// pgListByForm returns the copies f selects among the keys under this test's
// prefix, in listing order.
func pgListByForm(t *testing.T, s *Store, f *core.FormFilter) []core.ObjectLocation {
	t.Helper()
	prefix := t.Name() + "/"
	rows, err := s.ListCopiesByForm(context.Background(), f, 10000, core.Cursor{ObjectKey: prefix})
	if err != nil {
		t.Fatalf("ListCopiesByForm(%+v): %v", f, err)
	}
	var out []core.ObjectLocation
	for i := range rows {
		if strings.HasPrefix(rows[i].ObjectKey, prefix) {
			out = append(out, rows[i])
		}
	}
	return out
}

// pgKeysByForm returns the keys of the copies f selects under this test's
// prefix, in listing order.
func pgKeysByForm(t *testing.T, s *Store, f *core.FormFilter) []string {
	t.Helper()
	rows := pgListByForm(t, s, f)
	keys := make([]string, len(rows))
	for i := range rows {
		keys[i] = rows[i].ObjectKey
	}
	return keys
}

// pgRecordForm records one copy of key on backend stored as form, and removes
// it when the test ends so later tests' ledger figures do not see it.
func pgRecordForm(t *testing.T, s *Store, key, backend string, size int64, form *core.StoredForm) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key: key, Copies: []core.ObjectCopy{{Backend: backend}}, Size: size, Form: form,
	}); err != nil {
		t.Fatalf("RecordObject(%s): %v", key, err)
	}
	t.Cleanup(func() { _, _, _ = s.DeleteObject(ctx, key) })
}

// pgFormFromFlags builds a stored form carrying the fields each flag in set
// stands for, with sizes distinct enough that ObjectSize shows which it read.
func pgFormFromFlags(set core.StoredFlags) *core.StoredForm {
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

// seedMaskForms records one copy per interesting form under this test's
// prefix: plaintext, hashed, sealed with its key, encrypted with no key, and
// compressed.
func seedMaskForms(t *testing.T, s *Store) {
	t.Helper()
	pgRecordForm(t, s, uniqueKey(t, suffixPlain), formBackend, 20, nil)
	pgRecordForm(t, s, uniqueKey(t, suffixHashed), formBackend, 20, &core.StoredForm{ContentHash: formHash})
	pgRecordForm(t, s, uniqueKey(t, suffixSealed), formBackend, 20, &core.StoredForm{
		Encrypted: true, EncryptionKey: []byte(formDEK), KeyID: formKeyID, PlaintextSize: 10, ContentHash: formHash,
	})
	pgRecordForm(t, s, uniqueKey(t, suffixLost), formBackend, 20, &core.StoredForm{Encrypted: true, KeyID: formKeyID, PlaintextSize: 10})
	pgRecordForm(t, s, uniqueKey(t, suffixPacked), formBackend, 20, &core.StoredForm{
		CompressionAlgorithm: formAlgorithm, CompressionLevel: formLevel, LogicalSize: 50, ContentHash: formHash,
	})
}

// pgMaskCases pairs each With/Without mask with the seeded key suffixes it
// selects, in listing order.
var pgMaskCases = []struct {
	with, without core.StoredFlags
	want          []string
}{
	{0, 0, []string{suffixHashed, suffixLost, suffixPacked, suffixPlain, suffixSealed}},
	{core.StoredEncrypted, 0, []string{suffixLost, suffixSealed}},
	{0, core.StoredEncrypted, []string{suffixHashed, suffixPacked, suffixPlain}},
	{core.StoredHashed, core.StoredCompressed, []string{suffixHashed, suffixSealed}},
	{core.StoredEncrypted, core.StoredKeyed, []string{suffixLost}},
	{core.StoredCompressed | core.StoredHashed, 0, []string{suffixPacked}},
	{0, core.StoredHashed, []string{suffixLost, suffixPlain}},
}

// pgCountByForm reads CountCopiesByForm, failing the test on error.
func pgCountByForm(t *testing.T, s *Store, with, without core.StoredFlags) int64 {
	t.Helper()
	n, err := s.CountCopiesByForm(context.Background(), with, without)
	if err != nil {
		t.Fatalf("CountCopiesByForm: %v", err)
	}
	return n
}

// -------------------------------------------------------------------------
// GENERATED COLUMNS
// -------------------------------------------------------------------------

// TestStoreInt_StoredFormParity_ColumnsMatchGo pins the form and object_size
// columns the migration generates to the Go definitions, for every flag
// combination plus the zero and empty fields: an encrypted empty object, an
// empty key slice, and an encoding with no recorded logical size. Go reads the
// form the insert records, so a key with no envelope reads as plaintext.
func TestStoreInt_StoredFormParity_ColumnsMatchGo(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	forms := make(map[string]*core.StoredForm)
	for set := range core.StoredFlags(16) {
		forms[uniqueKey(t, fmt.Sprintf("flags-%02d", set))] = pgFormFromFlags(set)
	}
	forms[uniqueKey(t, "empty-encrypted")] = &core.StoredForm{Encrypted: true, EncryptionKey: []byte(formDEK), KeyID: formKeyID}
	forms[uniqueKey(t, "empty-key")] = &core.StoredForm{Encrypted: true, EncryptionKey: []byte{}, KeyID: formKeyID, PlaintextSize: 10}
	forms[uniqueKey(t, "no-logical-size")] = &core.StoredForm{CompressionAlgorithm: formAlgorithm, CompressionLevel: formLevel}

	for key, form := range forms {
		pgRecordForm(t, s, key, formBackend, paritySize, form)
	}
	for key, form := range forms {
		var gotForm core.StoredFlags
		var gotSize int64
		if err := s.pool.QueryRow(ctx,
			`SELECT form, object_size FROM object_locations WHERE object_key = $1`, key,
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

// -------------------------------------------------------------------------
// LISTING AND COUNTING BY FORM
// -------------------------------------------------------------------------

// TestStoreInt_ListCopiesByForm_Masks verifies a copy is listed only when it
// carries every flag of With and none of Without.
func TestStoreInt_ListCopiesByForm_Masks(t *testing.T) {
	s := adapterPgStore(t)
	seedMaskForms(t, s)
	for _, tc := range pgMaskCases {
		want := make([]string, len(tc.want))
		for i, suffix := range tc.want {
			want[i] = uniqueKey(t, suffix)
		}
		got := pgKeysByForm(t, s, &core.FormFilter{With: tc.with, Without: tc.without})
		if !slices.Equal(got, want) {
			t.Errorf("with %s without %s = %v, want %v", tc.with, tc.without, got, want)
		}
	}
}

// TestStoreInt_CountCopiesByForm_MatchesTheListing verifies the count moves by
// exactly what the listing returns for the same masks, so a dashboard figure
// and the pass it describes cannot disagree.
func TestStoreInt_CountCopiesByForm_MatchesTheListing(t *testing.T) {
	s := adapterPgStore(t)
	before := make([]int64, len(pgMaskCases))
	for i, tc := range pgMaskCases {
		before[i] = pgCountByForm(t, s, tc.with, tc.without)
	}
	seedMaskForms(t, s)
	for i, tc := range pgMaskCases {
		if delta := pgCountByForm(t, s, tc.with, tc.without) - before[i]; delta != int64(len(tc.want)) {
			t.Errorf("with %s without %s counted %d more, want %d", tc.with, tc.without, delta, len(tc.want))
		}
	}
}

// TestStoreInt_ListCopiesByForm_NarrowsByBackendKeyAndManagement verifies the
// backend, key ID, excluded key ID and managed filters each narrow the page,
// and that the empty value of each matches every row.
func TestStoreInt_ListCopiesByForm_NarrowsByBackendKeyAndManagement(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	onA, onB := uniqueKey(t, "on-a"), uniqueKey(t, "on-b")
	pgRecordForm(t, s, onA, formBackend, 10, &core.StoredForm{Encrypted: true, EncryptionKey: []byte(formDEK), KeyID: "key-1"})
	pgRecordForm(t, s, onB, formOtherBackend, 10, &core.StoredForm{Encrypted: true, EncryptionKey: []byte(formDEK), KeyID: "key-2"})
	stray := uniqueKey(t, "stray")
	if _, err := s.ImportObject(ctx, &core.ImportObjectRequest{Key: stray, Backend: formBackend, Size: 10, Unmanaged: true}); err != nil {
		t.Fatalf("ImportObject(unmanaged): %v", err)
	}
	t.Cleanup(func() { _, _, _ = s.DeleteObject(ctx, stray) })

	cases := []struct {
		name string
		f    core.FormFilter
		want []string
	}{
		{"every backend", core.FormFilter{With: core.StoredEncrypted}, []string{onA, onB}},
		{"one backend", core.FormFilter{With: core.StoredEncrypted, Backend: formOtherBackend}, []string{onB}},
		{"unknown backend", core.FormFilter{With: core.StoredEncrypted, Backend: "backend-absent"}, nil},
		{"one key", core.FormFilter{With: core.StoredEncrypted, KeyID: "key-1"}, []string{onA}},
		{"except one key", core.FormFilter{With: core.StoredEncrypted, ExceptKeyID: "key-1"}, []string{onB}},
		{"except an unused key", core.FormFilter{With: core.StoredEncrypted, ExceptKeyID: "key-absent"}, []string{onA, onB}},
		{"except keeps keyless copies", core.FormFilter{Without: core.StoredHashed | core.StoredEncrypted, ExceptKeyID: "key-1"}, []string{stray}},
		{"unmanaged included", core.FormFilter{Without: core.StoredHashed | core.StoredEncrypted}, []string{stray}},
		{"managed only", core.FormFilter{Without: core.StoredHashed | core.StoredEncrypted, ManagedOnly: true}, nil},
	}
	for _, tc := range cases {
		if got := pgKeysByForm(t, s, &tc.f); !slices.Equal(got, tc.want) {
			t.Errorf("%s: listed %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestStoreInt_ListCopiesByForm_CandidatesApplyTheThresholds verifies the
// candidate listing applies the size floor to the object size, and judges a
// recorded probe only when it was taken at the configured level: a probe over
// the ratio excludes the copy, the same probe under a loosened ratio returns
// it, and a probe from another level says nothing.
func TestStoreInt_ListCopiesByForm_CandidatesApplyTheThresholds(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	small, large := uniqueKey(t, "small"), uniqueKey(t, "large")
	probed, secret := uniqueKey(t, "probed"), uniqueKey(t, "secret")
	pgRecordForm(t, s, small, formBackend, 100, nil)
	pgRecordForm(t, s, large, formBackend, 9000, nil)
	pgRecordForm(t, s, probed, formBackend, 1000, nil)
	pgRecordForm(t, s, secret, formBackend, 9000, &core.StoredForm{
		Encrypted: true, EncryptionKey: []byte(formDEK), KeyID: formKeyID, PlaintextSize: 100,
	})
	probe := &core.CopyPatch{Probe: &core.CompressionProbe{Size: 900, Level: formLevel}}
	if err := s.UpdateCopyMetadata(ctx, storetest.LocationOn(t, s, probed, formBackend), probe); err != nil {
		t.Fatalf("UpdateCopyMetadata(probe): %v", err)
	}

	cases := []struct {
		name string
		t    core.CompressionThresholds
		want []string
	}{
		{"floor reads the object size", core.CompressionThresholds{MinSize: 4096, MinRatio: 1, Level: formLevel}, []string{large}},
		{"probe over the ratio", core.CompressionThresholds{MinRatio: 0.5, Level: formLevel}, []string{large, secret, small}},
		{"probe under a loosened ratio", core.CompressionThresholds{MinRatio: 0.95, Level: formLevel}, []string{large, probed, secret, small}},
		{"probe at another level", core.CompressionThresholds{MinRatio: 0.5, Level: "better"}, []string{large, probed, secret, small}},
	}
	for _, tc := range cases {
		if got := pgKeysByForm(t, s, pgCandidatesUnder(tc.t)); !slices.Equal(got, tc.want) {
			t.Errorf("%s: listed %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestStoreInt_ListCopiesByForm_PagesByCursor verifies each page starts after
// the cursor, so a backfill hashing as it goes still reaches every copy even
// though each hashed copy leaves the filter.
func TestStoreInt_ListCopiesByForm_PagesByCursor(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	keys := []string{uniqueKey(t, "a"), uniqueKey(t, "b"), uniqueKey(t, "c")}
	for _, key := range keys {
		pgRecordForm(t, s, key, formBackend, 10, nil)
	}

	first, err := s.ListCopiesByForm(ctx, &backfillCopies, 2, core.Cursor{ObjectKey: t.Name() + "/"})
	if err != nil || len(first) != 2 || first[0].ObjectKey != keys[0] || first[1].ObjectKey != keys[1] {
		t.Fatalf("first page = %v, %v; want %v", first, err, keys[:2])
	}
	for i := range first {
		if err := s.UpdateCopyMetadata(ctx, &first[i], &core.CopyPatch{ContentHash: formHash, Scrubbed: true}); err != nil {
			t.Fatalf("UpdateCopyMetadata: %v", err)
		}
	}
	after := core.Cursor{ObjectKey: first[1].ObjectKey, BackendName: first[1].BackendName}
	rest, err := s.ListCopiesByForm(ctx, &backfillCopies, 2, after)
	if err != nil || len(rest) == 0 || rest[0].ObjectKey != keys[2] {
		t.Errorf("second page = %v, %v; want %s first", rest, err, keys[2])
	}
}

// TestStoreInt_ListCopiesByForm_ReturnsWholeRows verifies a listed copy carries
// the probe and identity columns as well as the stored form, so a pass acting
// on it has everything the row records.
func TestStoreInt_ListCopiesByForm_ReturnsWholeRows(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "whole")
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key: key, Copies: []core.ObjectCopy{{Backend: formBackend}}, Size: 1000,
		Identity: &core.ObjectIdentity{ETag: `"etag-1"`},
	}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	t.Cleanup(func() { _, _, _ = s.DeleteObject(ctx, key) })
	probe := &core.CopyPatch{Probe: &core.CompressionProbe{Size: 990, Level: formLevel}}
	if err := s.UpdateCopyMetadata(ctx, storetest.LocationOn(t, s, key, formBackend), probe); err != nil {
		t.Fatalf("UpdateCopyMetadata(probe): %v", err)
	}

	rows := pgListByForm(t, s, &core.FormFilter{Without: core.StoredCompressed})
	if len(rows) != 1 {
		t.Fatalf("listed %+v, want the one copy", rows)
	}
	got := rows[0]
	if got.CompressionProbeSize != 990 || got.CompressionProbeLevel != formLevel {
		t.Errorf("probe = %d/%q, want 990/%s", got.CompressionProbeSize, got.CompressionProbeLevel, formLevel)
	}
	if got.Identity == nil || got.Identity.ETag != `"etag-1"` {
		t.Errorf("identity = %+v, want the recorded etag", got.Identity)
	}
}
