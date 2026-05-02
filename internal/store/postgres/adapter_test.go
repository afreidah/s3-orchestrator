// -------------------------------------------------------------------------------
// Postgres TxAdapter - Type Translation Tests + Interface Assertions
//
// Author: Alex Freidah
//
// Direct coverage for the pure translation helpers shared by adapter.go and
// reader paths. Per-method runtime behavior of the adapter is exercised
// against a real PostgreSQL container in internal/integration/ via the
// existing PromotePending, RecordObject, DeleteObject, MoveObjectLocation,
// ImportObject, RecordReplica, RemoveExcessCopy, and
// SweepStaleCleanupQueueRows tests.
// -------------------------------------------------------------------------------

package postgres

import (
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"

	"github.com/jackc/pgx/v5/pgtype"
)

// -------------------------------------------------------------------------
// TYPE TRANSLATION HELPERS
// -------------------------------------------------------------------------

// TestStrPtr_NilWhenEmpty verifies the helper returns nil for empty
// strings so the database stores SQL NULL.
func TestStrPtr_NilWhenEmpty(t *testing.T) {
	t.Parallel()
	if got := strPtr(""); got != nil {
		t.Errorf("expected nil for empty string, got %v", got)
	}
}

// TestStrPtr_PointerWhenNonEmpty verifies the helper returns a non-nil
// pointer to the supplied string.
func TestStrPtr_PointerWhenNonEmpty(t *testing.T) {
	t.Parallel()
	got := strPtr("hello")
	if got == nil {
		t.Fatal("expected non-nil pointer")
	}
	if *got != "hello" {
		t.Errorf("expected %q, got %q", "hello", *got)
	}
}

// TestInt64Ptr_NilWhenZero verifies the helper returns nil for zero.
func TestInt64Ptr_NilWhenZero(t *testing.T) {
	t.Parallel()
	if got := int64Ptr(0); got != nil {
		t.Errorf("expected nil for zero, got %v", got)
	}
}

// TestInt64Ptr_PointerWhenNonZero verifies the helper returns a
// non-nil pointer to the supplied value.
func TestInt64Ptr_PointerWhenNonZero(t *testing.T) {
	t.Parallel()
	got := int64Ptr(42)
	if got == nil {
		t.Fatal("expected non-nil pointer")
	}
	if *got != 42 {
		t.Errorf("expected 42, got %d", *got)
	}
}

// TestDerefStr verifies safe dereference of a nullable string pointer.
func TestDerefStr(t *testing.T) {
	t.Parallel()
	if got := derefStr(nil); got != "" {
		t.Errorf("nil deref: expected empty string, got %q", got)
	}
	s := "world"
	if got := derefStr(&s); got != "world" {
		t.Errorf("expected %q, got %q", "world", got)
	}
}

// TestDerefInt64 verifies safe dereference of a nullable int64 pointer.
func TestDerefInt64(t *testing.T) {
	t.Parallel()
	if got := derefInt64(nil); got != 0 {
		t.Errorf("nil deref: expected 0, got %d", got)
	}
	n := int64(99)
	if got := derefInt64(&n); got != 99 {
		t.Errorf("expected 99, got %d", got)
	}
}

// -------------------------------------------------------------------------
// pendingInsertParams
// -------------------------------------------------------------------------

// TestPendingInsertParams_OmitsZeroOptionals verifies empty optional
// fields produce SQL NULL pointers.
func TestPendingInsertParams_OmitsZeroOptionals(t *testing.T) {
	t.Parallel()
	got := pendingInsertParams(&core.PendingObject{
		IntentID:    "abc",
		ObjectKey:   "k",
		BackendName: "b1",
		SizeBytes:   100,
	})
	if got.IntentID != "abc" || got.ObjectKey != "k" || got.BackendName != "b1" || got.SizeBytes != 100 {
		t.Errorf("required fields not propagated: %+v", got)
	}
	if got.KeyID != nil || got.PlaintextSize != nil || got.ContentHash != nil {
		t.Errorf("expected SQL NULL for optional fields, got %+v", got)
	}
}

// TestPendingInsertParams_SetsOptionalsWhenPresent verifies non-zero
// optional fields are forwarded as non-nil pointers.
func TestPendingInsertParams_SetsOptionalsWhenPresent(t *testing.T) {
	t.Parallel()
	got := pendingInsertParams(&core.PendingObject{
		IntentID:      "abc",
		ObjectKey:     "k",
		BackendName:   "b1",
		SizeBytes:     100,
		Encrypted:     true,
		EncryptionKey: []byte("packed"),
		KeyID:         "kid-1",
		PlaintextSize: 90,
		ContentHash:   "deadbeef",
	})
	if got.KeyID == nil || *got.KeyID != "kid-1" {
		t.Errorf("KeyID = %v, want pointer to %q", got.KeyID, "kid-1")
	}
	if got.PlaintextSize == nil || *got.PlaintextSize != 90 {
		t.Errorf("PlaintextSize = %v, want pointer to 90", got.PlaintextSize)
	}
	if got.ContentHash == nil || *got.ContentHash != "deadbeef" {
		t.Errorf("ContentHash = %v, want pointer to %q", got.ContentHash, "deadbeef")
	}
	if !got.Encrypted {
		t.Error("Encrypted = false, want true")
	}
}

// -------------------------------------------------------------------------
// pendingFromRow
// -------------------------------------------------------------------------

// TestPendingFromRow_NullableFieldsZeroedWhenNull verifies the row to
// core type translation maps SQL NULL to the canonical zero value.
func TestPendingFromRow_NullableFieldsZeroedWhenNull(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	row := db.PendingObject{
		IntentID:    "abc",
		ObjectKey:   "k",
		BackendName: "b1",
		SizeBytes:   100,
		CreatedAt:   pgtype.Timestamptz{Time: created, Valid: true},
	}
	got := pendingFromRow(&row)
	if got.KeyID != "" || got.PlaintextSize != 0 || got.ContentHash != "" {
		t.Errorf("nullable fields not zeroed: %+v", got)
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, created)
	}
}

// TestPendingFromRow_NullableFieldsDereferencedWhenSet verifies non-NULL
// columns land as the pointer's value on the core type.
func TestPendingFromRow_NullableFieldsDereferencedWhenSet(t *testing.T) {
	t.Parallel()
	keyID := "kid-1"
	plaintext := int64(80)
	hash := "abc123"
	row := db.PendingObject{
		IntentID:      "abc",
		ObjectKey:     "k",
		BackendName:   "b1",
		SizeBytes:     100,
		Encrypted:     true,
		EncryptionKey: []byte("packed"),
		KeyID:         &keyID,
		PlaintextSize: &plaintext,
		ContentHash:   &hash,
	}
	got := pendingFromRow(&row)
	if got.KeyID != "kid-1" || got.PlaintextSize != 80 || got.ContentHash != "abc123" {
		t.Errorf("dereferenced fields incorrect: %+v", got)
	}
}

// -------------------------------------------------------------------------
// existingCopiesFromRows
// -------------------------------------------------------------------------

// TestExistingCopiesFromRows_EmptyInput verifies the empty-slice case.
func TestExistingCopiesFromRows_EmptyInput(t *testing.T) {
	t.Parallel()
	got := existingCopiesFromRows(nil)
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %d entries", len(got))
	}
}

// TestExistingCopiesFromRows_PreservesFields verifies every row's
// columns land on the corresponding core.ExistingCopy.
func TestExistingCopiesFromRows_PreservesFields(t *testing.T) {
	t.Parallel()
	t1 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	rows := []db.GetExistingCopiesForUpdateRow{
		{BackendName: "b1", SizeBytes: 100, CreatedAt: pgtype.Timestamptz{Time: t1, Valid: true}},
		{BackendName: "b2", SizeBytes: 200, CreatedAt: pgtype.Timestamptz{Time: t2, Valid: true}},
	}
	got := existingCopiesFromRows(rows)
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if got[0].BackendName != "b1" || got[0].SizeBytes != 100 || !got[0].CreatedAt.Equal(t1) {
		t.Errorf("entry 0 mismatch: %+v", got[0])
	}
	if got[1].BackendName != "b2" || got[1].SizeBytes != 200 || !got[1].CreatedAt.Equal(t2) {
		t.Errorf("entry 1 mismatch: %+v", got[1])
	}
}

// -------------------------------------------------------------------------
// objectInsertParams
// -------------------------------------------------------------------------

// TestObjectInsertParams_NilEnc verifies nil encryption metadata leaves
// every encryption-related field at the zero value.
func TestObjectInsertParams_NilEnc(t *testing.T) {
	t.Parallel()
	got := objectInsertParams(&core.ObjectLocation{
		ObjectKey:   "k",
		BackendName: "b1",
		SizeBytes:   100,
	})
	if got.Encrypted || got.EncryptionKey != nil || got.KeyID != nil || got.PlaintextSize != nil || got.ContentHash != nil {
		t.Errorf("expected zero encryption fields with nil enc, got %+v", got)
	}
}

// TestObjectInsertParams_EncryptedFields verifies an encrypted location
// projects every encryption attribute onto the sqlc params.
func TestObjectInsertParams_EncryptedFields(t *testing.T) {
	t.Parallel()
	got := objectInsertParams(&core.ObjectLocation{
		ObjectKey:     "k",
		BackendName:   "b1",
		SizeBytes:     100,
		Encrypted:     true,
		EncryptionKey: []byte("packed"),
		KeyID:         "kid-1",
		PlaintextSize: 90,
		ContentHash:   "abc",
	})
	if !got.Encrypted {
		t.Error("Encrypted = false, want true")
	}
	if string(got.EncryptionKey) != "packed" {
		t.Errorf("EncryptionKey = %v", got.EncryptionKey)
	}
	if got.KeyID == nil || *got.KeyID != "kid-1" {
		t.Errorf("KeyID = %v", got.KeyID)
	}
	if got.PlaintextSize == nil || *got.PlaintextSize != 90 {
		t.Errorf("PlaintextSize = %v", got.PlaintextSize)
	}
	if got.ContentHash == nil || *got.ContentHash != "abc" {
		t.Errorf("ContentHash = %v", got.ContentHash)
	}
}

// TestObjectInsertParams_HashOnlyWithoutEncryption verifies an
// integrity-only PUT (no encryption, hash present) projects only the
// hash column.
func TestObjectInsertParams_HashOnlyWithoutEncryption(t *testing.T) {
	t.Parallel()
	got := objectInsertParams(&core.ObjectLocation{
		ObjectKey:   "k",
		BackendName: "b1",
		SizeBytes:   100,
		ContentHash: "abc",
	})
	if got.Encrypted {
		t.Error("Encrypted = true, want false")
	}
	if got.ContentHash == nil || *got.ContentHash != "abc" {
		t.Errorf("ContentHash = %v", got.ContentHash)
	}
}

// -------------------------------------------------------------------------
// COMPILE-TIME ASSERTIONS
// -------------------------------------------------------------------------

// TestPgTxAdapter_SatisfiesCoreInterfaces is a compile-time check that
// the adapter satisfies every per-feature interface embedded in
// core.TxAdapter. Catches accidental signature drift before runtime.
func TestPgTxAdapter_SatisfiesCoreInterfaces(t *testing.T) {
	t.Parallel()
	var a *pgTxAdapter // nil pointer is fine for type assertions
	var _ core.PendingTxAdapter = a
	var _ core.ObjectsTxAdapter = a
	var _ core.CleanupTxAdapter = a
	var _ core.QuotaTxAdapter = a
	var _ core.TxAdapter = a
}
