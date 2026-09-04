// -------------------------------------------------------------------------------
// ReconcileUsage Quota Integration Test
//
// Author: Alex Freidah
//
// Pins the contract that ReconcileUsage rewrites backend_quotas.bytes_used to
// SUM(object_locations.size_bytes) per backend. The counter is otherwise
// incrementally maintained and drifts permanently if any mutation path misses
// an adjustment; this is the operator-facing repair for that drift.
// -------------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestStoreInt_ReconcileUsage_CorrectsDrift records two objects so the ledger
// total is known, corrupts bytes_used to simulate drift, and asserts
// ReconcileUsage restores the counter to the ledger truth and reports the
// applied delta.
func TestStoreInt_ReconcileUsage_CorrectsDrift(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: uniqueKey(t, "recon-1"), Backend: "backend-a", Size: 100}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: uniqueKey(t, "recon-2"), Backend: "backend-a", Size: 250}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	// The shared container accumulates rows across tests, so read the actual
	// ledger truth rather than assuming only this test's objects exist.
	var truth int64
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(size_bytes), 0) FROM object_locations WHERE backend_name = 'backend-a'`,
	).Scan(&truth); err != nil {
		t.Fatalf("read ledger sum: %v", err)
	}

	// Corrupt the counter to simulate the drift a degraded-backend cycle leaves
	// behind, then reconcile back to truth.
	corrupted := truth + 99999
	if _, err := s.pool.Exec(ctx,
		`UPDATE backend_quotas SET bytes_used = $1 WHERE backend_name = 'backend-a'`, corrupted); err != nil {
		t.Fatalf("corrupt bytes_used: %v", err)
	}

	adj, err := s.ReconcileUsage(ctx)
	if err != nil {
		t.Fatalf("ReconcileUsage: %v", err)
	}

	if got := readBytesUsed(t, s, "backend-a"); got != truth {
		t.Errorf("bytes_used = %d, want %d (ledger truth)", got, truth)
	}
	if adj["backend-a"] != truth-corrupted {
		t.Errorf("adjustment = %d, want %d", adj["backend-a"], truth-corrupted)
	}
}
