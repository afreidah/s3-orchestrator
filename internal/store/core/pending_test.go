// -------------------------------------------------------------------------------
// Core Pending Helpers - Pure-Function Tests
//
// Author: Alex Freidah
//
// Engine-agnostic coverage for intentSuperseded and pendingEncryptionMeta.
// Both helpers are exercised exhaustively here; the per-engine adapter layers
// only need integration coverage that the right values reach these helpers.
// -------------------------------------------------------------------------------

package core

import (
	"testing"
	"time"
)

// -------------------------------------------------------------------------
// pendingEncryptionMeta
// -------------------------------------------------------------------------

// TestPendingEncryptionMeta_ReturnsNilWhenUnencryptedAndNoHash verifies
// the helper returns nil for plain unencrypted PUTs with no content hash,
// so the promoted object_locations row mirrors the unencrypted shape.
func TestPendingEncryptionMeta_ReturnsNilWhenUnencryptedAndNoHash(t *testing.T) {
	t.Parallel()
	if got := pendingEncryptionMeta(&PendingObject{}); got != nil {
		t.Errorf("expected nil for unencrypted no-hash pending, got %+v", got)
	}
}

// TestPendingEncryptionMeta_PreservesEncryptionFields verifies that an
// encrypted pending intent projects every encryption attribute onto the
// EncryptionMeta the promote path hands to RecordObject.
func TestPendingEncryptionMeta_PreservesEncryptionFields(t *testing.T) {
	t.Parallel()
	p := PendingObject{
		Encrypted:     true,
		EncryptionKey: []byte("packed"),
		KeyID:         "kid-1",
		PlaintextSize: 90,
		ContentHash:   "hash",
	}
	got := pendingEncryptionMeta(&p)
	if got == nil {
		t.Fatal("expected non-nil meta")
	}
	if !got.Encrypted || got.KeyID != "kid-1" || got.PlaintextSize != 90 || got.ContentHash != "hash" {
		t.Errorf("meta mismatch: %+v", got)
	}
	if string(got.EncryptionKey) != "packed" {
		t.Errorf("EncryptionKey not propagated: %v", got.EncryptionKey)
	}
}

// TestPendingEncryptionMeta_HashOnlyReturnsMeta verifies that an
// integrity-only PUT (encryption disabled, content hash present) still
// produces a non-nil EncryptionMeta so the hash survives promotion.
func TestPendingEncryptionMeta_HashOnlyReturnsMeta(t *testing.T) {
	t.Parallel()
	got := pendingEncryptionMeta(&PendingObject{ContentHash: "hash"})
	if got == nil {
		t.Fatal("expected non-nil meta when hash is set")
	}
	if got.Encrypted || got.ContentHash != "hash" {
		t.Errorf("meta mismatch: %+v", got)
	}
}

// -------------------------------------------------------------------------
// intentSuperseded - pure function, drives the timestamp-aware drop path
// -------------------------------------------------------------------------

// TestIntentSuperseded_NoExistingCopiesNeverSuperseded verifies the empty
// case: with no existing rows there is nothing newer than the intent.
func TestIntentSuperseded_NoExistingCopiesNeverSuperseded(t *testing.T) {
	t.Parallel()
	if intentSuperseded(nil, time.Now()) {
		t.Error("empty existing slice must not be superseded")
	}
}

// TestIntentSuperseded_AllOlderNeverSuperseded verifies the canonical
// promote case: the intent is at least as new as every existing copy.
func TestIntentSuperseded_AllOlderNeverSuperseded(t *testing.T) {
	t.Parallel()
	intentTime := time.Now()
	existing := []ExistingCopy{
		{BackendName: "b1", CreatedAt: intentTime.Add(-1 * time.Minute)},
		{BackendName: "b2", CreatedAt: intentTime.Add(-2 * time.Minute)},
	}
	if intentSuperseded(existing, intentTime) {
		t.Error("all-older existing rows must not trigger supersession")
	}
}

// TestIntentSuperseded_AnyNewerSupersedes verifies that even one row newer
// than the intent fires the drop path. This is the head-of-line-blocking
// fix: a successful retry on any backend authoritatively supersedes the
// stale intent.
func TestIntentSuperseded_AnyNewerSupersedes(t *testing.T) {
	t.Parallel()
	intentTime := time.Now()
	existing := []ExistingCopy{
		{BackendName: "b1", CreatedAt: intentTime.Add(-1 * time.Minute)},
		{BackendName: "b2", CreatedAt: intentTime.Add(1 * time.Minute)},
	}
	if !intentSuperseded(existing, intentTime) {
		t.Error("any newer existing row must trigger supersession")
	}
}

// TestIntentSuperseded_EqualTimePromotes verifies that ties go to promote.
// The intent is "at least as new" as the existing row; only strictly
// newer existing rows count as supersession.
func TestIntentSuperseded_EqualTimePromotes(t *testing.T) {
	t.Parallel()
	intentTime := time.Now()
	existing := []ExistingCopy{
		{BackendName: "b1", CreatedAt: intentTime},
	}
	if intentSuperseded(existing, intentTime) {
		t.Error("equal timestamp must not trigger supersession")
	}
}

// TestIntentSuperseded_ZeroTimestampSkipped verifies that rows with a
// zero CreatedAt are ignored. Engine adapters set CreatedAt to the zero
// value when the underlying database row is NULL/invalid; supersession
// must not fire on those.
func TestIntentSuperseded_ZeroTimestampSkipped(t *testing.T) {
	t.Parallel()
	intentTime := time.Now()
	existing := []ExistingCopy{
		{BackendName: "b1", CreatedAt: time.Time{}},
	}
	if intentSuperseded(existing, intentTime) {
		t.Error("zero timestamp must be ignored, not treated as newer")
	}
}
