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

import "testing"

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
