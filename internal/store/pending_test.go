// -------------------------------------------------------------------------------
// Pending Objects Tests
//
// Author: Alex Freidah
//
// Unit coverage for the helper logic around the PendingStore role:
// param mapping, encryption-meta projection, and the circuit-breaker
// forwarder (every PendingStore method routes through breaker.CBCall*).
// The PromotePending transactional flow is exercised via integration tests
// against a real PostgreSQL container; this file pins the in-memory shape.
// -------------------------------------------------------------------------------

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/afreidah/s3-orchestrator/internal/store/sqlc"
)

// -------------------------------------------------------------------------
// pendingInsertParams
// -------------------------------------------------------------------------

// TestPendingInsertParams_NullableFieldsOmittedWhenZero verifies that
// empty/zero source fields produce SQL NULL rather than the zero value
// in the generated insert params.
func TestPendingInsertParams_NullableFieldsOmittedWhenZero(t *testing.T) {
	t.Parallel()
	p := PendingObject{
		IntentID:    "abc",
		ObjectKey:   "k",
		BackendName: "b1",
		SizeBytes:   100,
	}
	got := pendingInsertParams(&p)
	if got.IntentID != "abc" || got.ObjectKey != "k" || got.BackendName != "b1" || got.SizeBytes != 100 {
		t.Errorf("required fields not propagated: %+v", got)
	}
	if got.KeyID != nil {
		t.Error("KeyID should remain nil when source is empty (SQL NULL)")
	}
	if got.PlaintextSize != nil {
		t.Error("PlaintextSize should remain nil when source is zero (SQL NULL)")
	}
	if got.ContentHash != nil {
		t.Error("ContentHash should remain nil when source is empty (SQL NULL)")
	}
}

// TestPendingInsertParams_NullableFieldsSetWhenPresent verifies that
// non-zero encryption and hash fields are forwarded as the corresponding
// non-nil pointer types so the database stores them rather than NULL.
func TestPendingInsertParams_NullableFieldsSetWhenPresent(t *testing.T) {
	t.Parallel()
	p := PendingObject{
		IntentID:      "abc",
		ObjectKey:     "k",
		BackendName:   "b1",
		SizeBytes:     100,
		Encrypted:     true,
		EncryptionKey: []byte("packed"),
		KeyID:         "kid-1",
		PlaintextSize: 90,
		ContentHash:   "deadbeef",
	}
	got := pendingInsertParams(&p)
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
	// Integrity-only PUT: encryption disabled but content hash present.
	got := pendingEncryptionMeta(&PendingObject{ContentHash: "hash"})
	if got == nil {
		t.Fatal("expected non-nil meta when hash is set")
	}
	if got.Encrypted || got.ContentHash != "hash" {
		t.Errorf("meta mismatch: %+v", got)
	}
}

// -------------------------------------------------------------------------
// intentSuperseded — pure function, drives the timestamp-aware drop path
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
	existing := []db.GetExistingCopiesForUpdateRow{
		{BackendName: "b1", CreatedAt: pgTimestamptz(intentTime.Add(-1 * time.Minute))},
		{BackendName: "b2", CreatedAt: pgTimestamptz(intentTime.Add(-2 * time.Minute))},
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
	existing := []db.GetExistingCopiesForUpdateRow{
		{BackendName: "b1", CreatedAt: pgTimestamptz(intentTime.Add(-1 * time.Minute))},
		{BackendName: "b2", CreatedAt: pgTimestamptz(intentTime.Add(1 * time.Minute))},
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
	existing := []db.GetExistingCopiesForUpdateRow{
		{BackendName: "b1", CreatedAt: pgTimestamptz(intentTime)},
	}
	if intentSuperseded(existing, intentTime) {
		t.Error("equal timestamp must not trigger supersession")
	}
}

// TestIntentSuperseded_InvalidTimestampSkipped verifies that rows with a
// non-Valid pgtype.Timestamptz are ignored. This protects against a
// theoretical NULL slipping through and incorrectly bypassing the check.
func TestIntentSuperseded_InvalidTimestampSkipped(t *testing.T) {
	t.Parallel()
	intentTime := time.Now()
	existing := []db.GetExistingCopiesForUpdateRow{
		{BackendName: "b1", CreatedAt: pgtype.Timestamptz{Time: intentTime.Add(time.Hour), Valid: false}},
	}
	if intentSuperseded(existing, intentTime) {
		t.Error("invalid timestamp must be ignored, not treated as newer")
	}
}

// -------------------------------------------------------------------------
// CB forwarder
// -------------------------------------------------------------------------

// TestCBForwarders_PendingStore verifies every PendingStore method on the
// circuit-breaker decorator forwards arguments to the inner store and
// returns the inner store's result unchanged when the breaker is closed.
func TestCBForwarders_PendingStore(t *testing.T) {
	t.Parallel()
	mock := &mockStore{
		pendingDepthResp:        7,
		getStalePendingResp:     []PendingObject{{IntentID: "x"}},
		promotePendingResult:    PendingPromoteCommitted,
		promotePendingDisplaced: []DeletedCopy{{BackendName: "old", SizeBytes: 5}},
	}
	cb := newTestCB(mock, 3, time.Minute)
	ctx := context.Background()

	if err := cb.InsertPending(ctx, &PendingObject{IntentID: "x"}); err != nil {
		t.Errorf("InsertPending: %v", err)
	}
	if err := cb.DeletePending(ctx, "x"); err != nil {
		t.Errorf("DeletePending: %v", err)
	}
	rows, err := cb.GetStalePending(ctx, time.Now(), 10)
	if err != nil {
		t.Fatalf("GetStalePending: %v", err)
	}
	if len(rows) != 1 || rows[0].IntentID != "x" {
		t.Errorf("GetStalePending = %+v, want one row with IntentID x", rows)
	}
	depth, err := cb.PendingDepth(ctx)
	if err != nil {
		t.Fatalf("PendingDepth: %v", err)
	}
	if depth != 7 {
		t.Errorf("PendingDepth = %d, want 7", depth)
	}
	result, displaced, err := cb.PromotePending(ctx, &PendingObject{IntentID: "x"})
	if err != nil {
		t.Fatalf("PromotePending: %v", err)
	}
	if result != PendingPromoteCommitted {
		t.Errorf("PromotePending result = %v, want Committed", result)
	}
	if len(displaced) != 1 || displaced[0].BackendName != "old" {
		t.Errorf("PromotePending displaced = %+v", displaced)
	}
	if err := cb.DeletePendingByBackend(ctx, "b1"); err != nil {
		t.Errorf("DeletePendingByBackend: %v", err)
	}
}

// TestCBForwarders_PendingStore_OpenCircuitReturnsSentinel verifies that
// once the shared breaker is open, every PendingStore method returns
// ErrDBUnavailable without touching the inner store.
func TestCBForwarders_PendingStore_OpenCircuitReturnsSentinel(t *testing.T) {
	t.Parallel()
	dbErr := errors.New("connection refused")
	// Trip the breaker via getAllLocations, then verify pending methods
	// short-circuit with ErrDBUnavailable instead of hitting the inner mock.
	mock := &mockStore{getAllLocationsErr: dbErr}
	cb := newTestCB(mock, 1, time.Minute)
	ctx := context.Background()

	_, _ = cb.GetAllObjectLocations(ctx, "k")

	if err := cb.InsertPending(ctx, &PendingObject{IntentID: "x"}); !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("InsertPending: got %v, want ErrDBUnavailable", err)
	}
	if err := cb.DeletePending(ctx, "x"); !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("DeletePending: got %v, want ErrDBUnavailable", err)
	}
	if _, err := cb.GetStalePending(ctx, time.Now(), 10); !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("GetStalePending: got %v, want ErrDBUnavailable", err)
	}
	if _, err := cb.PendingDepth(ctx); !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("PendingDepth: got %v, want ErrDBUnavailable", err)
	}
	if _, _, err := cb.PromotePending(ctx, &PendingObject{IntentID: "x"}); !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("PromotePending: got %v, want ErrDBUnavailable", err)
	}
	if err := cb.DeletePendingByBackend(ctx, "b1"); !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("DeletePendingByBackend: got %v, want ErrDBUnavailable", err)
	}
}
