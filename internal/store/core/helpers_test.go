// -------------------------------------------------------------------------------
// Core Helpers - Object / Encryption / Displacement Tests
//
// Author: Alex Freidah
//
// Pure-function coverage for ObjectFromStoredForm, StoredForm.Described,
// removedFromExisting, removedCopy.request, PartPath, ObjectLocation.ObjectSize,
// LocationOn, and the copy listing walk of CopyPager and CopyRuns.
// Engine adapters lean on these helpers when translating between the
// canonical core domain types and the engine-specific row shapes; the
// behaviors must hold for every code path independently of the engine.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// -------------------------------------------------------------------------
// ObjectFromStoredForm
// -------------------------------------------------------------------------

// TestObjectFromStoredForm_NilForm verifies a nil StoredForm yields an
// ObjectLocation with the zero value for every encryption-related
// field.
func TestObjectFromStoredForm_NilForm(t *testing.T) {
	t.Parallel()
	loc := ObjectFromStoredForm("k", "b1", "k!w1", 100, nil, nil)
	if loc == nil {
		t.Fatal("expected non-nil ObjectLocation")
	}
	if loc.ObjectKey != "k" || loc.BackendName != "b1" || loc.StorageKey != "k!w1" || loc.SizeBytes != 100 {
		t.Errorf("required fields not preserved: %+v", loc)
	}
	if loc.Encrypted || loc.EncryptionKey != nil || loc.KeyID != "" || loc.PlaintextSize != 0 || loc.ContentHash != "" {
		t.Errorf("encryption fields not zeroed for nil form: %+v", loc)
	}
}

// TestObjectFromStoredForm_EncryptedFields verifies an encrypted location
// carries every encryption attribute end-to-end.
func TestObjectFromStoredForm_EncryptedFields(t *testing.T) {
	t.Parallel()
	form := &StoredForm{
		Encrypted:     true,
		EncryptionKey: []byte("packed"),
		KeyID:         "kid-1",
		PlaintextSize: 90,
		ContentHash:   "abc",
	}
	loc := ObjectFromStoredForm("k", "b1", "k!w1", 100, form, nil)
	if !loc.Encrypted || loc.KeyID != "kid-1" || loc.PlaintextSize != 90 || loc.ContentHash != "abc" {
		t.Errorf("encryption fields not preserved: %+v", loc)
	}
	if string(loc.EncryptionKey) != "packed" {
		t.Errorf("EncryptionKey not preserved: %v", loc.EncryptionKey)
	}
}

// TestObjectFromStoredForm_HashOnly verifies that an integrity-only PUT
// (encryption disabled, content hash present) still copies the hash across.
func TestObjectFromStoredForm_HashOnly(t *testing.T) {
	t.Parallel()
	form := &StoredForm{ContentHash: "abc123"}
	loc := ObjectFromStoredForm("k", "b1", "k!w1", 100, form, nil)
	if loc.Encrypted {
		t.Error("Encrypted = true, want false")
	}
	if loc.ContentHash != "abc123" {
		t.Errorf("ContentHash = %q, want %q", loc.ContentHash, "abc123")
	}
}

// TestObjectFromStoredForm_PlaintextFormWithoutEncryption verifies
// that a non-nil StoredForm with Encrypted=false and no hash
// produces the same shape as a nil StoredForm.
func TestObjectFromStoredForm_PlaintextFormWithoutEncryption(t *testing.T) {
	t.Parallel()
	loc := ObjectFromStoredForm("k", "b1", "k!w1", 100, &StoredForm{}, nil)
	if loc.Encrypted || loc.EncryptionKey != nil || loc.ContentHash != "" {
		t.Errorf("plaintext form did not yield zero encryption fields: %+v", loc)
	}
}

// -------------------------------------------------------------------------
// StoredForm.Described
// -------------------------------------------------------------------------

// TestStoredFormDescribed_NilWithNoLayerOrHash verifies a form with no
// compression, encryption or hash describes nothing to carry, including one
// that holds only a key.
func TestStoredFormDescribed_NilWithNoLayerOrHash(t *testing.T) {
	t.Parallel()
	for name, f := range map[string]StoredForm{
		"plain":      {},
		"keyed only": {EncryptionKey: []byte("dek"), KeyID: "kid"},
	} {
		if got := f.Described(); got != nil {
			t.Errorf("%s: Described() = %+v, want nil", name, got)
		}
	}
}

// TestStoredFormDescribed_CopiesEveryField verifies each layer, and a hash
// alone, yields a copy carrying every field rather than the original.
func TestStoredFormDescribed_CopiesEveryField(t *testing.T) {
	t.Parallel()
	full := StoredForm{
		Encrypted: true, EncryptionKey: []byte("packed"), KeyID: "kid-1", PlaintextSize: 90,
		ContentHash: "hash", CompressionAlgorithm: "zstd", CompressionLevel: "default",
		CompressionFormatVersion: 1, LogicalSize: 120,
	}
	for name, f := range map[string]StoredForm{
		"hash only":  {ContentHash: "hash"},
		"encrypted":  {Encrypted: true, KeyID: "kid-1", PlaintextSize: 90},
		"compressed": {CompressionAlgorithm: "zstd", LogicalSize: 120},
		"every":      full,
	} {
		got := f.Described()
		if got == nil {
			t.Fatalf("%s: Described() = nil", name)
		}
		if !reflect.DeepEqual(*got, f) {
			t.Errorf("%s: Described() = %+v, want %+v", name, *got, f)
		}
	}
	got := full.Described()
	got.KeyID = "changed"
	if full.KeyID != "kid-1" {
		t.Error("Described returned the original form, not a copy")
	}
}

// -------------------------------------------------------------------------
// CopyPager and CopyRuns
// -------------------------------------------------------------------------

// errListing is the failure sortedCopies returns once it runs out of pages.
var errListing = errors.New("listing failed")

// sortedCopies is a CopyLister over rows already in key order. A positive
// failAfter fails every listing after that many pages; filter records what
// the pager asked with.
type sortedCopies struct {
	rows      []ObjectLocation
	failAfter int
	pages     int
	filter    *FormFilter
}

// ListCopiesByForm returns up to limit rows after the cursor.
func (s *sortedCopies) ListCopiesByForm(_ context.Context, f *FormFilter, limit int, after Cursor) ([]ObjectLocation, error) {
	s.pages++
	s.filter = f
	if s.failAfter > 0 && s.pages > s.failAfter {
		return nil, errListing
	}
	start := 0
	for i := range s.rows {
		if after != (Cursor{}) && CursorOf(s.rows[i]) == after {
			start = i + 1
		}
	}
	return s.rows[start:min(start+limit, len(s.rows))], nil
}

// copyRows builds one copy for each "key/backend" given, in the order given.
func copyRows(copies ...string) []ObjectLocation {
	rows := make([]ObjectLocation, 0, len(copies))
	for _, c := range copies {
		key, backend, _ := strings.Cut(c, "/")
		rows = append(rows, ObjectLocation{ObjectKey: key, BackendName: backend})
	}
	return rows
}

// TestCopyRuns_GroupsEachKeyAcrossPages verifies a key's copies come out as
// one run even when a page boundary falls between them, and that the pager
// lists with the filter it was given.
func TestCopyRuns_GroupsEachKeyAcrossPages(t *testing.T) {
	t.Parallel()
	list := &sortedCopies{rows: copyRows("a/b1", "a/b2", "b/b1", "b/b2", "b/b3", "c/b1")}
	filter := &FormFilter{With: StoredEncrypted}
	var runs []string
	for run, err := range CopyRuns(CopyPager(list, filter, 2).Rows(t.Context())) {
		if err != nil {
			t.Fatalf("CopyRuns: %v", err)
		}
		runs = append(runs, KeyOf(run[0])+":"+strconv.Itoa(len(run)))
	}
	if want := []string{"a:2", "b:3", "c:1"}; !slices.Equal(runs, want) {
		t.Errorf("runs = %v, want %v", runs, want)
	}
	if list.filter != filter {
		t.Error("the pager listed with a filter other than the one it was given")
	}
}

// TestCopyRuns_ErrorDropsTheInterruptedRun verifies a listing failure ends
// the runs with its error and leaves out the run it cut short, whose other
// copies were never listed.
func TestCopyRuns_ErrorDropsTheInterruptedRun(t *testing.T) {
	t.Parallel()
	list := &sortedCopies{rows: copyRows("a/b1", "a/b2", "b/b1", "b/b2", "b/b3"), failAfter: 2}
	var runs []string
	var got error
	for run, err := range CopyRuns(CopyPager(list, &FormFilter{}, 2).Rows(t.Context())) {
		if err != nil {
			got = err
			continue
		}
		runs = append(runs, KeyOf(run[0]))
	}
	if !errors.Is(got, errListing) {
		t.Errorf("err = %v, want the listing failure", got)
	}
	if !slices.Equal(runs, []string{"a"}) {
		t.Errorf("runs = %v, want only the run completed before the failure", runs)
	}
}

// TestCopyRuns_StopsWhenTheConsumerDoes verifies a consumer that stops ranging
// stops the listing with it.
func TestCopyRuns_StopsWhenTheConsumerDoes(t *testing.T) {
	t.Parallel()
	list := &sortedCopies{rows: copyRows("a/b1", "b/b1", "c/b1", "d/b1")}
	for range CopyRuns(CopyPager(list, &FormFilter{}, 1).Rows(t.Context())) {
		break
	}
	if list.pages != 2 {
		t.Errorf("listed %d pages, want the 2 that completed the first run", list.pages)
	}
}

// -------------------------------------------------------------------------
// QUOTA AND TAG STUB
// -------------------------------------------------------------------------

// quotaTxStub is the TxAdapter implementation the quota and tag
// tests drive. Everything it does not name comes from the embedded
// noopTxAdapter; the quota mutators record call order so tests can assert the
// lock-acquisition sequence.
type quotaTxStub struct {
	noopTxAdapter

	mu      sync.Mutex
	ops     []quotaOp
	failOn  string
	failErr error

	tagsCleared    []string
	tagKeysCleared [][]string
	tagsInserted   []Tag
	tagClearErr    error
	tagInsertErr   error
	keyLockErr     error
	keyLockCalls   int
	existingCopies []ExistingCopy
	existingErr    error
	pendingCleanup bool
	pathInFlight   bool
	pendingErr     error
	importedLoc    *ObjectLocation

	adjustments []quotaOp
	adjustErr   error
}

// storedCopy is the seed a tag test uses to say the object exists, since the
// tagging operations refuse a key that holds nothing.
func storedCopy() []ExistingCopy {
	return []ExistingCopy{{BackendName: "b1", SizeBytes: 100}}
}

// quotaOp is one recorded quota mutation. The sign carries the caller's
// intent rather than the SQL direction, so a test can assert the order the
// backends were touched in without decoding which method produced each entry.
type quotaOp struct {
	backend string
	delta   int64 // positive=increment, negative=decrement (mirrors caller intent)
}

// AdjustQuotaStripes records each signed delta into ops and adjustments and
// honours the failure hooks. The stripe is not recorded so tests do not depend
// on the hash.
func (t *quotaTxStub) AdjustQuotaStripes(_ context.Context, charges ...StripeCharge) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.adjustErr != nil {
		return t.adjustErr
	}
	for _, c := range charges {
		if c.Backend == t.failOn {
			return t.failErr
		}
		op := quotaOp{backend: c.Backend, delta: c.StoredBytes}
		t.ops = append(t.ops, op)
		t.adjustments = append(t.adjustments, op)
	}
	return nil
}

// InsertObjectTag records the tag so replace-semantics tests can assert what
// was written after the preceding clear.
func (t *quotaTxStub) InsertObjectTag(_ context.Context, _, tagKey, tagValue string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tagInsertErr != nil {
		return t.tagInsertErr
	}
	t.tagsInserted = append(t.tagsInserted, Tag{Key: tagKey, Value: tagValue})
	return nil
}

// DeleteObjectTags records which keys had their tags cleared, which is what
// the cascade tests assert on, and each call as one statement so a test can
// tell a batch clear apart from a loop of single clears.
func (t *quotaTxStub) DeleteObjectTags(_ context.Context, objectKeys ...string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tagClearErr != nil {
		return t.tagClearErr
	}
	t.tagsCleared = append(t.tagsCleared, objectKeys...)
	t.tagKeysCleared = append(t.tagKeysCleared, slices.Clone(objectKeys))
	return nil
}

// AcquireKeyLock reports the seeded lock error; the tag paths take the lock
// before they write, and one test makes that fail.
func (t *quotaTxStub) AcquireKeyLock(context.Context, ...string) error {
	t.keyLockCalls++
	return t.keyLockErr
}

// GetExistingCopiesForUpdate returns the seeded copy set, which is how a test
// says whether the key holds anything.
func (t *quotaTxStub) GetExistingCopiesForUpdate(_ context.Context, keys ...string) ([]ExistingCopy, error) {
	return keyedCopies(t.existingCopies, keys), t.existingErr
}

// ListedPathStates reports every path as pending cleanup when the fixture was
// primed with one, so an import can be driven down the ordinary path, the
// suppressed path, or the error path.
func (s *quotaTxStub) ListedPathStates(_ context.Context, _ string, paths []string) (map[string]PathState, error) {
	states := map[string]PathState{}
	if s.pendingCleanup {
		for _, p := range paths {
			states[p] = PathPendingCleanup
		}
	} else if s.pathInFlight {
		for _, p := range paths {
			states[p] = PathInFlight
		}
	}
	return states, s.pendingErr
}

// InsertObjectLocationIfNotExists records the row an import built so a test can
// assert on the timestamp it carried, and reports it as newly inserted.
func (s *quotaTxStub) InsertObjectLocationIfNotExists(_ context.Context, loc *ObjectLocation) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	captured := *loc
	s.importedLoc = &captured
	return true, nil
}

// TestValidateEncryptionMetadata covers every self-consistency rule the read
// path relies on, in both directions: a row that describes bytes it cannot
// actually produce is rejected, a coherent row of either kind is accepted.
func TestValidateEncryptionMetadata(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		loc     *ObjectLocation
		wantErr bool
	}{
		{"nil location is unmanaged, not contradictory", nil, false},
		{"plain row with no key", &ObjectLocation{SizeBytes: 10}, false},
		{"plain row still carrying a key", &ObjectLocation{SizeBytes: 10, EncryptionKey: []byte("k")}, true},
		{"encrypted row with key and plaintext size",
			&ObjectLocation{SizeBytes: 100, Encrypted: true, EncryptionKey: []byte("k"), PlaintextSize: 25}, false},
		{"encrypted row with no key",
			&ObjectLocation{SizeBytes: 100, Encrypted: true, PlaintextSize: 25}, true},
		{"encrypted row with no plaintext size",
			&ObjectLocation{SizeBytes: 100, Encrypted: true, EncryptionKey: []byte("k")}, true},
		{"encrypted empty object stores a bare header and no plaintext",
			&ObjectLocation{Encrypted: true, EncryptionKey: []byte("k"), SizeBytes: 32}, false},
		{"encrypted row with chunks but no plaintext size lost it",
			&ObjectLocation{Encrypted: true, EncryptionKey: []byte("k"), SizeBytes: 4096}, true},
		{"encrypted row with a negative plaintext size",
			&ObjectLocation{Encrypted: true, EncryptionKey: []byte("k"), SizeBytes: 4096, PlaintextSize: -1}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateEncryptionMetadata(tt.loc)
			if tt.wantErr && !errors.Is(err, ErrEncryptionFlagMismatch) {
				t.Errorf("expected ErrEncryptionFlagMismatch, got %v", err)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("expected no error, got %v", err)
			}
		})
	}
}

// TestClientLocations covers the narrowing every client read applies: unmanaged
// copies drop out, a key with nothing else left reads as not found, and a
// lookup error passes through untouched.
func TestClientLocations(t *testing.T) {
	t.Parallel()
	managed := ObjectLocation{ObjectKey: "k", BackendName: "b1"}
	unmanaged := ObjectLocation{ObjectKey: "k", BackendName: "b2", Unmanaged: true}
	lookupErr := errors.New("db down")

	tests := []struct {
		name    string
		locs    []ObjectLocation
		err     error
		want    []string
		wantErr error
	}{
		{"unmanaged copy is dropped", []ObjectLocation{managed, unmanaged}, nil, []string{"b1"}, nil},
		{"only unmanaged copies read as not found", []ObjectLocation{unmanaged}, nil, nil, ErrObjectNotFound},
		{"lookup error passes through", nil, lookupErr, nil, lookupErr},
		{"not found passes through", nil, ErrObjectNotFound, nil, ErrObjectNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ClientLocations(slices.Clone(tt.locs), tt.err)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			var backends []string
			for i := range got {
				backends = append(backends, got[i].BackendName)
			}
			if !slices.Equal(backends, tt.want) {
				t.Errorf("backends = %v, want %v", backends, tt.want)
			}
		})
	}
}

// TestStoredForm_Unreadable covers the form import records for an envelope no
// key opens, which is the only form that marks an import unmanaged.
func TestStoredForm_Unreadable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		form *StoredForm
		want bool
	}{
		{"nil form is verbatim bytes", nil, false},
		{"plaintext", &StoredForm{}, false},
		{"encrypted with a key", &StoredForm{Encrypted: true, EncryptionKey: []byte("k")}, false},
		{"encrypted with no key", &StoredForm{Encrypted: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.form.Unreadable(); got != tt.want {
				t.Errorf("Unreadable() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestPartPath verifies a part's fixed path and that a part's Path prefers the
// storage key its upload wrote.
func TestPartPath(t *testing.T) {
	t.Parallel()
	if got := PartPath("up1", 3); got != "__multipart/up1/3" {
		t.Errorf("PartPath = %q, want __multipart/up1/3", got)
	}
	legacy := MultipartPart{PartNumber: 3}
	if got := legacy.Path("up1"); got != "__multipart/up1/3" {
		t.Errorf("legacy Path = %q, want the fixed path", got)
	}
	keyed := MultipartPart{PartNumber: 3, StorageKey: "__multipart/up1/3!w9"}
	if got := keyed.Path("up1"); got != "__multipart/up1/3!w9" {
		t.Errorf("keyed Path = %q, want the upload's own path", got)
	}
}

// TestObjectLocation_ObjectSize pins which size a client is told about for
// each stored form. Getting this wrong sends clients ranging against
// coordinates the object does not have.
func TestObjectLocation_ObjectSize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		loc  ObjectLocation
		want int64
	}{
		{name: "verbatim", loc: ObjectLocation{SizeBytes: 100}, want: 100},
		{name: "encrypted only", loc: ObjectLocation{SizeBytes: 160, Encrypted: true, PlaintextSize: 100}, want: 100},
		{name: "compressed only", loc: ObjectLocation{SizeBytes: 40, LogicalSize: 100, CompressionAlgorithm: "zstd"}, want: 100},
		{
			name: "compressed and encrypted",
			loc: ObjectLocation{
				SizeBytes: 70, Encrypted: true, PlaintextSize: 40,
				LogicalSize: 100, CompressionAlgorithm: "zstd",
			},
			want: 100,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.loc.ObjectSize(); got != tc.want {
				t.Errorf("ObjectSize = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestStoredForm_ClientSize verifies the client size is judged from the form
// alone, falling back to the stored size the caller passes for verbatim
// bytes.
func TestStoredForm_ClientSize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		form StoredForm
		want int64
	}{
		{name: "verbatim", form: StoredForm{}, want: 70},
		{name: "encrypted", form: StoredForm{Encrypted: true, PlaintextSize: 40}, want: 40},
		{name: "compressed", form: StoredForm{CompressionAlgorithm: "zstd", LogicalSize: 100}, want: 100},
		{name: "both", form: StoredForm{Encrypted: true, PlaintextSize: 40, CompressionAlgorithm: "zstd", LogicalSize: 100}, want: 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.form.ClientSize(70); got != tc.want {
				t.Errorf("ClientSize(70) = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestLocationOn verifies the copy on the named backend is found, and that a
// backend holding no copy yields nil.
func TestLocationOn(t *testing.T) {
	t.Parallel()
	locs := []ObjectLocation{{BackendName: "b1", StorageKey: "k!1"}, {BackendName: "b2", StorageKey: "k!2"}}
	if got := LocationOn(locs, "b2"); got == nil || got.StorageKey != "k!2" {
		t.Errorf("LocationOn(b2) = %+v, want the b2 copy", got)
	}
	if got := LocationOn(locs, "b3"); got != nil {
		t.Errorf("LocationOn(b3) = %+v, want nil", got)
	}
}

// TestLocationListingHelpers verifies the keys of a page keep its order and
// repeat a key held twice, and each cursor resumes after the copy it is
// taken from.
func TestLocationListingHelpers(t *testing.T) {
	t.Parallel()
	locs := []ObjectLocation{
		{ObjectKey: "a", BackendName: "b1", SizeBytes: 3},
		{ObjectKey: "a", BackendName: "b2", SizeBytes: 3},
		{ObjectKey: "c", BackendName: "b1", SizeBytes: 9},
	}
	if got := KeysOf(locs); !slices.Equal(got, []string{"a", "a", "c"}) {
		t.Errorf("KeysOf = %v, want [a a c]", got)
	}
	if got := CursorOf(locs[1]); got != (Cursor{ObjectKey: "a", BackendName: "b2"}) {
		t.Errorf("CursorOf = %+v, want after a on b2", got)
	}
	if got := SizeCursorOf(locs[2]); got != (SizeCursor{SizeBytes: 9, ObjectKey: "c"}) {
		t.Errorf("SizeCursorOf = %+v, want after c at 9 bytes", got)
	}
}
