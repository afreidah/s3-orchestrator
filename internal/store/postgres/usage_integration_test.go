// -----------------------------------------------------------------------------
// ReconcileUsage Quota Integration Test
//
// Author: Alex Freidah
//
// Pins the contract that ReconcileUsage rewrites backend_quotas.bytes_used to
// SUM(object_locations.size_bytes) per backend. The counter is otherwise
// incrementally maintained and drifts permanently if any mutation path misses
// an adjustment; this is the operator-facing repair for that drift.
// -----------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"testing"
)

// TestStoreInt_ReconcileUsage_CorrectsDrift records two objects so the ledger
// total is known, corrupts bytes_used to simulate drift, and asserts
// ReconcileUsage restores the counter to the ledger truth and reports the
// applied delta.
func TestStoreInt_ReconcileUsage_CorrectsDrift(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	resetBytesUsed(t, s, "backend-a")
	if _, err := s.RecordObject(ctx, uniqueKey(t, "recon-1"), "backend-a", 100, nil); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if _, err := s.RecordObject(ctx, uniqueKey(t, "recon-2"), "backend-a", 250, nil); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	// Ledger truth for backend-a is now 350. Corrupt the counter to simulate
	// the drift a degraded-backend cycle leaves behind.
	if _, err := s.pool.Exec(ctx,
		`UPDATE backend_quotas SET bytes_used = 99999 WHERE backend_name = 'backend-a'`); err != nil {
		t.Fatalf("corrupt bytes_used: %v", err)
	}

	adj, err := s.ReconcileUsage(ctx)
	if err != nil {
		t.Fatalf("ReconcileUsage: %v", err)
	}

	if got := readBytesUsed(t, s, "backend-a"); got != 350 {
		t.Errorf("bytes_used = %d, want 350 (ledger truth)", got)
	}
	if adj["backend-a"] != 350-99999 {
		t.Errorf("adjustment = %d, want %d", adj["backend-a"], 350-99999)
	}
}
