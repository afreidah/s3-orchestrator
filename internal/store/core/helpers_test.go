// -------------------------------------------------------------------------------
// Core Helpers - Object / Encryption / Displacement Tests
//
// Author: Alex Freidah
//
// Pure-function coverage for objectFromEnc and displacedFromExisting.
// Engine adapters lean on these helpers when translating between the
// canonical core domain types and the engine-specific row shapes; the
// behaviors must hold for every code path independently of the engine.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// -------------------------------------------------------------------------
// objectFromEnc
// -------------------------------------------------------------------------

// TestObjectFromEnc_NilEnc verifies a nil EncryptionMeta yields an
// ObjectLocation with the zero value for every encryption-related
// field.
func TestObjectFromEnc_NilEnc(t *testing.T) {
	t.Parallel()
	loc := objectFromEnc("k", "b1", 100, nil)
	if loc == nil {
		t.Fatal("expected non-nil ObjectLocation")
	}
	if loc.ObjectKey != "k" || loc.BackendName != "b1" || loc.SizeBytes != 100 {
		t.Errorf("required fields not preserved: %+v", loc)
	}
	if loc.Encrypted || loc.EncryptionKey != nil || loc.KeyID != "" || loc.PlaintextSize != 0 || loc.ContentHash != "" {
		t.Errorf("encryption fields not zeroed for nil enc: %+v", loc)
	}
}

// TestObjectFromEnc_EncryptedFields verifies an encrypted location
// carries every encryption attribute end-to-end.
func TestObjectFromEnc_EncryptedFields(t *testing.T) {
	t.Parallel()
	enc := &EncryptionMeta{
		Encrypted:     true,
		EncryptionKey: []byte("packed"),
		KeyID:         "kid-1",
		PlaintextSize: 90,
		ContentHash:   "abc",
	}
	loc := objectFromEnc("k", "b1", 100, enc)
	if !loc.Encrypted || loc.KeyID != "kid-1" || loc.PlaintextSize != 90 || loc.ContentHash != "abc" {
		t.Errorf("encryption fields not preserved: %+v", loc)
	}
	if string(loc.EncryptionKey) != "packed" {
		t.Errorf("EncryptionKey not preserved: %v", loc.EncryptionKey)
	}
}

// TestObjectFromEnc_HashOnly verifies that an integrity-only PUT
// (encryption disabled, content hash present) still copies the hash
// across without setting any encryption fields.
func TestObjectFromEnc_HashOnly(t *testing.T) {
	t.Parallel()
	enc := &EncryptionMeta{ContentHash: "abc123"}
	loc := objectFromEnc("k", "b1", 100, enc)
	if loc.Encrypted {
		t.Error("Encrypted = true, want false")
	}
	if loc.ContentHash != "abc123" {
		t.Errorf("ContentHash = %q, want %q", loc.ContentHash, "abc123")
	}
}

// TestObjectFromEnc_PlaintextOnlyEncMetaWithoutEncryption verifies
// that a non-nil EncryptionMeta with Encrypted=false and no hash
// produces the same shape as a nil EncryptionMeta.
func TestObjectFromEnc_PlaintextOnlyEncMetaWithoutEncryption(t *testing.T) {
	t.Parallel()
	loc := objectFromEnc("k", "b1", 100, &EncryptionMeta{})
	if loc.Encrypted || loc.EncryptionKey != nil || loc.ContentHash != "" {
		t.Errorf("plaintext meta did not yield zero encryption fields: %+v", loc)
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
// empty map.
func TestGroupByKey_EmptySlice(t *testing.T) {
	t.Parallel()
	got := GroupByKey(nil)
	if len(got) != 0 {
		t.Errorf("expected empty map for nil input, got %+v", got)
	}
}

// TestGroupByKey_SingleKeySingleCopy verifies the trivial case.
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

// quotaTxStub is the minimal TxAdapter implementation needed to drive
// applyQuotaDeltas. Every method other than Increment/DecrementBackendQuota
// returns the zero value or nil; only the two quota mutators are
// instrumented to record call order so tests can assert lock-acquisition
// sequence.
type quotaTxStub struct {
	mu      sync.Mutex
	ops     []quotaOp
	failOn  string
	failErr error
}

type quotaOp struct {
	backend string
	delta   int64 // positive=increment, negative=decrement (mirrors caller intent)
}

func (t *quotaTxStub) IncrementBackendQuota(_ context.Context, backend string, delta int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if backend == t.failOn {
		return t.failErr
	}
	t.ops = append(t.ops, quotaOp{backend: backend, delta: delta})
	return nil
}

func (t *quotaTxStub) DecrementBackendQuota(_ context.Context, backend string, delta int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if backend == t.failOn {
		return t.failErr
	}
	t.ops = append(t.ops, quotaOp{backend: backend, delta: -delta})
	return nil
}

// The remaining TxAdapter methods are unused by applyQuotaDeltas; stubs
// return zero values so the type satisfies the full interface.
func (*quotaTxStub) AcquireKeyLock(context.Context, string) error { return nil }

func (*quotaTxStub) ClaimPending(context.Context, string) (bool, error) { return false, nil }
func (*quotaTxStub) InsertPending(context.Context, *PendingObject) error { return nil }
func (*quotaTxStub) DeletePending(context.Context, string) error         { return nil }
func (*quotaTxStub) DeletePendingByBackend(context.Context, string) error {
	return nil
}

func (*quotaTxStub) GetExistingCopiesForUpdate(context.Context, string) ([]ExistingCopy, error) {
	return nil, nil
}
func (*quotaTxStub) InsertObjectLocation(context.Context, *ObjectLocation) error { return nil }
func (*quotaTxStub) DeleteObjectCopies(context.Context, string) error            { return nil }
func (*quotaTxStub) GetCopiesForKeysForUpdate(context.Context, []string) ([]KeyedExistingCopy, error) {
	return nil, nil
}
func (*quotaTxStub) DeleteObjectsByKeys(context.Context, []string) error { return nil }
func (*quotaTxStub) CheckObjectExistsOnBackend(context.Context, string, string) (bool, error) {
	return false, nil
}
func (*quotaTxStub) LockObjectOnBackend(context.Context, string, string) (*ObjectLocation, bool, error) {
	return nil, false, nil
}
func (*quotaTxStub) DeleteObjectFromBackend(context.Context, string, string) error { return nil }
func (*quotaTxStub) InsertObjectLocationIfNotExists(context.Context, *ObjectLocation) (bool, error) {
	return false, nil
}
func (*quotaTxStub) InsertReplicaConditional(context.Context, string, string, string) (int64, bool, error) {
	return 0, false, nil
}

func (*quotaTxStub) SumAndDeleteCleanupQueueRows(context.Context, string, string) (int64, int64, error) {
	return 0, 0, nil
}
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

