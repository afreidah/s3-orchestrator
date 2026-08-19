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

package postgres

import (
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// pendingInsertParams
// -------------------------------------------------------------------------

// TestPendingInsertParams_NullableFieldsOmittedWhenZero verifies that
// empty/zero source fields produce SQL NULL rather than the zero value
// in the generated insert params.
func TestPendingInsertParams_NullableFieldsOmittedWhenZero(t *testing.T) {
	t.Parallel()
	p := core.PendingObject{
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
	p := core.PendingObject{
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
// pendingStoredForm
// -------------------------------------------------------------------------
