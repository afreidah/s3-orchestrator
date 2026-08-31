// -------------------------------------------------------------------------------
// Core Helpers - Object / Encryption / Displacement Tests
//
// Author: Alex Freidah
//
// Pure-function coverage for objectFromStoredForm and displacedFromExisting.
// Engine adapters lean on these helpers when translating between the
// canonical core domain types and the engine-specific row shapes; the
// behaviors must hold for every code path independently of the engine.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
)

// -------------------------------------------------------------------------
// objectFromStoredForm
// -------------------------------------------------------------------------

// TestObjectFromStoredForm_NilForm verifies a nil StoredForm yields an
// ObjectLocation with the zero value for every encryption-related
// field.
func TestObjectFromStoredForm_NilForm(t *testing.T) {
	t.Parallel()
	loc := objectFromStoredForm("k", "b1", 100, nil, nil)
	if loc == nil {
		t.Fatal("expected non-nil ObjectLocation")
	}
	if loc.ObjectKey != "k" || loc.BackendName != "b1" || loc.SizeBytes != 100 {
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
	loc := objectFromStoredForm("k", "b1", 100, form, nil)
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
	loc := objectFromStoredForm("k", "b1", 100, form, nil)
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
	loc := objectFromStoredForm("k", "b1", 100, &StoredForm{}, nil)
	if loc.Encrypted || loc.EncryptionKey != nil || loc.ContentHash != "" {
		t.Errorf("plaintext form did not yield zero encryption fields: %+v", loc)
	}
}

// -------------------------------------------------------------------------
// displacedFromExisting
// -------------------------------------------------------------------------

// TestDisplacedFromExisting_EmptyInput verifies the empty-slice case
// returns nil rather than an allocated zero-length slice.
func TestDisplacedFromExisting_EmptyInput(t *testing.T) {
	t.Parallel()
	got := displacedFromExisting(nil, "b1")
	if got != nil {
		t.Errorf("expected nil for empty input, got %+v", got)
	}
	got = displacedFromExisting([]ExistingCopy{}, "b1")
	if got != nil {
		t.Errorf("expected nil for empty slice, got %+v", got)
	}
}

// TestDisplacedFromExisting_AllOnNewBackend verifies that when every
// existing copy is on the new target backend, no copies are
// "displaced" - the in-place overwrite consumes them.
func TestDisplacedFromExisting_AllOnNewBackend(t *testing.T) {
	t.Parallel()
	existing := []ExistingCopy{
		{BackendName: "b1", SizeBytes: 100},
	}
	got := displacedFromExisting(existing, "b1")
	if got != nil {
		t.Errorf("expected nil when every copy is on the target backend, got %+v", got)
	}
}

// TestDisplacedFromExisting_OtherBackends verifies that copies on
// backends other than the new target are returned for cleanup,
// while a copy on the target is excluded.
func TestDisplacedFromExisting_OtherBackends(t *testing.T) {
	t.Parallel()
	existing := []ExistingCopy{
		{BackendName: "b1", SizeBytes: 100}, // overwritten in place
		{BackendName: "b2", SizeBytes: 200}, // becomes orphan
		{BackendName: "b3", SizeBytes: 300}, // becomes orphan
	}
	got := displacedFromExisting(existing, "b1")
	if len(got) != 2 {
		t.Fatalf("expected 2 displaced copies, got %d", len(got))
	}
	seen := map[string]int64{}
	for _, dc := range got {
		seen[dc.BackendName] = dc.SizeBytes
	}
	if seen["b2"] != 200 || seen["b3"] != 300 {
		t.Errorf("displaced copies wrong: %+v", got)
	}
	if _, ok := seen["b1"]; ok {
		t.Errorf("b1 must not be displaced (in-place overwrite); got %+v", got)
	}
}

// -------------------------------------------------------------------------
// GroupByKey
// -------------------------------------------------------------------------

// TestGroupByKey_EmptySlice verifies the empty-slice case returns an
// empty map, so callers can range over the result without a nil check.
func TestGroupByKey_EmptySlice(t *testing.T) {
	t.Parallel()
	got := GroupByKey(nil)
	if len(got) != 0 {
		t.Errorf("expected empty map for nil input, got %+v", got)
	}
}

// TestGroupByKey_SingleKeySingleCopy verifies one key holding one copy
// groups into a single bucket carrying that copy.
func TestGroupByKey_SingleKeySingleCopy(t *testing.T) {
	t.Parallel()
	got := GroupByKey([]ObjectLocation{{ObjectKey: "k", BackendName: "b1"}})
	if len(got) != 1 || len(got["k"]) != 1 || got["k"][0].BackendName != "b1" {
		t.Errorf("group result wrong: %+v", got)
	}
}

// TestGroupByKey_MultipleKeysAndReplicas verifies replicas under the
// same key are bucketed together while distinct keys remain separate.
func TestGroupByKey_MultipleKeysAndReplicas(t *testing.T) {
	t.Parallel()
	in := []ObjectLocation{
		{ObjectKey: "k1", BackendName: "b1"},
		{ObjectKey: "k1", BackendName: "b2"},
		{ObjectKey: "k2", BackendName: "b1"},
	}
	got := GroupByKey(in)
	if len(got) != 2 {
		t.Fatalf("expected 2 keys in map, got %d", len(got))
	}
	if len(got["k1"]) != 2 {
		t.Errorf("k1 should have 2 copies, got %d", len(got["k1"]))
	}
	if len(got["k2"]) != 1 {
		t.Errorf("k2 should have 1 copy, got %d", len(got["k2"]))
	}
}

// -------------------------------------------------------------------------
// applyQuotaDeltas - #687 deadlock regression
// -------------------------------------------------------------------------

// quotaTxStub is the TxAdapter implementation the quota, tag and stored-form
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
	existingCopies []ExistingCopy
	existingErr    error
	pendingCleanup bool
	pendingErr     error

	adjustments []quotaOp
	adjustErr   error
	compressed  []CompressedUpdate
	encrypted   []markedCopy
	decrypted   []markedCopy
	formErr     error
	copySize    int64
	copySizeErr error
}

// markedCopy is one recorded stored-form rewrite: which copy it touched and
// the size it left behind.
type markedCopy struct {
	objectKey   string
	backendName string
	sizeBytes   int64
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

// IncrementBackendQuota records the credit and honours the failOn hook. One
// of the two instrumented methods: the call order it captures is what the
// deadlock regression asserts on.
func (t *quotaTxStub) IncrementBackendQuota(_ context.Context, backend string, delta int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if backend == t.failOn {
		return t.failErr
	}
	t.ops = append(t.ops, quotaOp{backend: backend, delta: delta})
	return nil
}

// DecrementBackendQuota records the debit as a negative delta and honours the
// failOn hook, mirroring IncrementBackendQuota.
func (t *quotaTxStub) DecrementBackendQuota(_ context.Context, backend string, delta int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if backend == t.failOn {
		return t.failErr
	}
	t.ops = append(t.ops, quotaOp{backend: backend, delta: -delta})
	return nil
}

// AdjustBackendBytesUsed records the signed delta a stored-form rewrite asked
// for, which is what the rewrite tests assert on.
func (t *quotaTxStub) AdjustBackendBytesUsed(_ context.Context, backend string, delta int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.adjustErr != nil {
		return t.adjustErr
	}
	t.adjustments = append(t.adjustments, quotaOp{backend: backend, delta: delta})
	return nil
}

// UpdateCompressedForm records the compressed form the rewrite wrote.
func (t *quotaTxStub) UpdateCompressedForm(_ context.Context, u *CompressedUpdate) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.formErr != nil {
		return t.formErr
	}
	t.compressed = append(t.compressed, *u)
	return nil
}

// MarkCopyEncrypted records the copy the encrypt pass rewrote.
func (t *quotaTxStub) MarkCopyEncrypted(_ context.Context, u *EncryptedUpdate) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.formErr != nil {
		return t.formErr
	}
	t.encrypted = append(t.encrypted, markedCopy{u.ObjectKey, u.BackendName, u.CiphertextSize})
	return nil
}

// MarkCopyDecrypted records the copy the decrypt pass rewrote.
func (t *quotaTxStub) MarkCopyDecrypted(_ context.Context, objectKey, backendName string, plaintextSize int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.formErr != nil {
		return t.formErr
	}
	t.decrypted = append(t.decrypted, markedCopy{objectKey, backendName, plaintextSize})
	return nil
}

// GetCopySizeBytes returns the size the decrypt pass reads before it
// overwrites the row.
func (t *quotaTxStub) GetCopySizeBytes(context.Context, string, string) (int64, error) {
	return t.copySize, t.copySizeErr
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
// the cascade tests assert on.
func (t *quotaTxStub) DeleteObjectTags(_ context.Context, objectKey string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tagClearErr != nil {
		return t.tagClearErr
	}
	t.tagsCleared = append(t.tagsCleared, objectKey)
	return nil
}

// DeleteObjectTagsForKeys records the batch clear as one call so a test can
// tell it apart from a loop of single clears.
func (t *quotaTxStub) DeleteObjectTagsForKeys(_ context.Context, objectKeys []string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tagClearErr != nil {
		return t.tagClearErr
	}
	t.tagKeysCleared = append(t.tagKeysCleared, slices.Clone(objectKeys))
	return nil
}

// AcquireKeyLock reports the seeded lock error; the tag paths take the lock
// before they write, and one test makes that fail.
func (t *quotaTxStub) AcquireKeyLock(context.Context, string) error { return t.keyLockErr }

// GetExistingCopiesForUpdate returns the seeded copy set, which is how a test
// says whether the key holds anything.
func (t *quotaTxStub) GetExistingCopiesForUpdate(context.Context, string) ([]ExistingCopy, error) {
	return t.existingCopies, t.existingErr
}

// GetCleanupQueueRow is a no-op stub on quotaTxStub so the type satisfies the
// full TxAdapter interface; only the quota-touching methods carry
// real test fixtures.
func (*quotaTxStub) GetCleanupQueueRow(context.Context, int64) (CleanupQueueRow, error) {
	return CleanupQueueRow{}, nil
}

// InsertCleanupDLQ is a no-op stub on quotaTxStub so the type satisfies the
// full TxAdapter interface; only the quota-touching methods carry
// real test fixtures.
func (*quotaTxStub) InsertCleanupDLQ(context.Context, *CleanupQueueRow) error { return nil }

// DeleteCleanupItem is a no-op stub on quotaTxStub so the type satisfies the
// full TxAdapter interface; only the quota-touching methods carry
// real test fixtures.
func (*quotaTxStub) DeleteCleanupItem(context.Context, int64) error { return nil }

// HasPendingCleanup reports whatever the fixture was primed with, so an import
// can be driven down the ordinary path, the suppressed path, or the error path.
func (s *quotaTxStub) HasPendingCleanup(context.Context, string, string) (bool, error) {
	return s.pendingCleanup, s.pendingErr
}

// DecrementOrphanBytes is a no-op stub on quotaTxStub so the type satisfies the
// full TxAdapter interface; only the quota-touching methods carry
// real test fixtures.
func (*quotaTxStub) DecrementOrphanBytes(context.Context, string, int64) error { return nil }

// TestApplyQuotaDeltas_StableOrderAcrossInputs runs applyQuotaDeltas
// against the same backend set with two different map insertion orders
// (Go map iteration is non-deterministic) and asserts both calls
// produce the same sorted-by-backend-name SQL sequence. This is the
// invariant that prevents the #687 deadlock: any two transactions that
// touch the same backend set request locks in identical order.
func TestApplyQuotaDeltas_StableOrderAcrossInputs(t *testing.T) {
	t.Parallel()
	deltasA := map[string]int64{"minio-3": -100, "minio-1": -50, "minio-2": -75}
	deltasB := map[string]int64{"minio-2": -75, "minio-3": -100, "minio-1": -50}

	txA := &quotaTxStub{}
	txB := &quotaTxStub{}
	if err := applyQuotaDeltas(context.Background(), txA, deltasA); err != nil {
		t.Fatalf("applyQuotaDeltas A: %v", err)
	}
	if err := applyQuotaDeltas(context.Background(), txB, deltasB); err != nil {
		t.Fatalf("applyQuotaDeltas B: %v", err)
	}
	if len(txA.ops) != 3 || len(txB.ops) != 3 {
		t.Fatalf("expected 3 ops each, got A=%d B=%d", len(txA.ops), len(txB.ops))
	}
	for i := range txA.ops {
		if txA.ops[i] != txB.ops[i] {
			t.Errorf("op %d diverged: A=%+v B=%+v", i, txA.ops[i], txB.ops[i])
		}
	}
	want := []string{"minio-1", "minio-2", "minio-3"}
	for i, w := range want {
		if txA.ops[i].backend != w {
			t.Errorf("op[%d].backend = %q, want %q (sorted backend_name)", i, txA.ops[i].backend, w)
		}
	}
}

// TestApplyQuotaDeltas_PositiveAndNegative verifies signed deltas route
// correctly: positive -> Increment, negative -> Decrement, zero ->
// skipped (so net-zero same-backend overwrites produce no SQL call).
func TestApplyQuotaDeltas_PositiveAndNegative(t *testing.T) {
	t.Parallel()
	tx := &quotaTxStub{}
	deltas := map[string]int64{
		"a": -100,
		"b": 200,
		"c": 0,
		"d": -50,
	}
	if err := applyQuotaDeltas(context.Background(), tx, deltas); err != nil {
		t.Fatalf("applyQuotaDeltas: %v", err)
	}
	want := []quotaOp{
		{backend: "a", delta: -100},
		{backend: "b", delta: 200},
		{backend: "d", delta: -50},
	}
	if len(tx.ops) != len(want) {
		t.Fatalf("ops = %d, want %d (zero delta should be skipped)", len(tx.ops), len(want))
	}
	for i, w := range want {
		if tx.ops[i] != w {
			t.Errorf("op[%d] = %+v, want %+v", i, tx.ops[i], w)
		}
	}
}

// TestApplyQuotaDeltas_PropagatesError verifies the helper short-circuits
// on the first SQL error and surfaces it to the caller.
func TestApplyQuotaDeltas_PropagatesError(t *testing.T) {
	t.Parallel()
	want := errors.New("simulated DB error")
	tx := &quotaTxStub{failOn: "b", failErr: want}
	deltas := map[string]int64{"a": 10, "b": 20, "c": 30}
	err := applyQuotaDeltas(context.Background(), tx, deltas)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
	if len(tx.ops) != 1 || tx.ops[0].backend != "a" {
		t.Errorf("expected single op on 'a' before failure, got %+v", tx.ops)
	}
}

// TestApplyQuotaDeltas_EmptyMap verifies the no-op path: an empty map
// (and a nil map) both produce zero SQL calls and no error.
func TestApplyQuotaDeltas_EmptyMap(t *testing.T) {
	t.Parallel()
	tx := &quotaTxStub{}
	if err := applyQuotaDeltas(context.Background(), tx, nil); err != nil {
		t.Errorf("nil map err = %v, want nil", err)
	}
	if err := applyQuotaDeltas(context.Background(), tx, map[string]int64{}); err != nil {
		t.Errorf("empty map err = %v, want nil", err)
	}
	if len(tx.ops) != 0 {
		t.Errorf("expected no ops, got %+v", tx.ops)
	}
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
