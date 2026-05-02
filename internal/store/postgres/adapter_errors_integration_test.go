// -------------------------------------------------------------------------------
// Postgres TxAdapter - Error-Path Coverage
//
// Author: Alex Freidah
//
// Closes the underlying pgx.Tx before calling each adapter method so the
// next query fails deterministically and the error-wrap branch
// (if err != nil { return fmt.Errorf("...", err) }) executes. The
// happy-path tests in adapter_integration_test.go cover the success
// branches; this file pairs every method with a paired failure test
// so coverage hits both paths.
// -------------------------------------------------------------------------------

//go:build integration
// +build integration

package postgres

import (
	"context"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// closedPgAdapter opens a transaction and immediately rolls it back,
// then returns the wrapping adapter. Every subsequent query on the
// adapter fails (pgx returns "tx is closed"), exercising the
// wrap-and-return-error path on every adapter method.
func closedPgAdapter(t *testing.T, s *Store) *pgTxAdapter {
	t.Helper()
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	return &pgTxAdapter{q: s.queries.WithTx(tx)}
}

// -------------------------------------------------------------------------
// AcquireKeyLock
// -------------------------------------------------------------------------

// TestPgAdapterErr_AcquireKeyLock verifies the Exec error is wrapped.
func TestPgAdapterErr_AcquireKeyLock(t *testing.T) {
	a := closedPgAdapter(t, adapterPgStore(t))
	if err := a.AcquireKeyLock(context.Background(), "k"); err == nil {
		t.Error("expected error from closed tx")
	}
}

// -------------------------------------------------------------------------
// PENDING TX ERRORS
// -------------------------------------------------------------------------

// TestPgAdapterErr_ClaimPending verifies an underlying DB error is
// wrapped (and is not the benign pgx.ErrNoRows path).
func TestPgAdapterErr_ClaimPending(t *testing.T) {
	a := closedPgAdapter(t, adapterPgStore(t))
	if _, err := a.ClaimPending(context.Background(), "any"); err == nil {
		t.Error("expected error from closed tx")
	}
}

// TestPgAdapterErr_InsertPending verifies the Exec error is wrapped.
func TestPgAdapterErr_InsertPending(t *testing.T) {
	a := closedPgAdapter(t, adapterPgStore(t))
	err := a.InsertPending(context.Background(), &core.PendingObject{
		IntentID: "i", ObjectKey: "k", BackendName: "backend-a", SizeBytes: 1,
	})
	if err == nil {
		t.Error("expected error from closed tx")
	}
}

// TestPgAdapterErr_DeletePending verifies the Exec error is wrapped.
func TestPgAdapterErr_DeletePending(t *testing.T) {
	a := closedPgAdapter(t, adapterPgStore(t))
	if err := a.DeletePending(context.Background(), "i"); err == nil {
		t.Error("expected error from closed tx")
	}
}

// TestPgAdapterErr_DeletePendingByBackend verifies the Exec error is
// wrapped.
func TestPgAdapterErr_DeletePendingByBackend(t *testing.T) {
	a := closedPgAdapter(t, adapterPgStore(t))
	if err := a.DeletePendingByBackend(context.Background(), "backend-a"); err == nil {
		t.Error("expected error from closed tx")
	}
}

// -------------------------------------------------------------------------
// OBJECTS TX ERRORS
// -------------------------------------------------------------------------

// TestPgAdapterErr_GetExistingCopiesForUpdate verifies the Query
// error is wrapped.
func TestPgAdapterErr_GetExistingCopiesForUpdate(t *testing.T) {
	a := closedPgAdapter(t, adapterPgStore(t))
	if _, err := a.GetExistingCopiesForUpdate(context.Background(), "k"); err == nil {
		t.Error("expected error from closed tx")
	}
}

// TestPgAdapterErr_InsertObjectLocation verifies the Exec error is
// wrapped.
func TestPgAdapterErr_InsertObjectLocation(t *testing.T) {
	a := closedPgAdapter(t, adapterPgStore(t))
	err := a.InsertObjectLocation(context.Background(), &core.ObjectLocation{
		ObjectKey: "k", BackendName: "backend-a", SizeBytes: 1,
	})
	if err == nil {
		t.Error("expected error from closed tx")
	}
}

// TestPgAdapterErr_DeleteObjectCopies verifies the Exec error is
// wrapped.
func TestPgAdapterErr_DeleteObjectCopies(t *testing.T) {
	a := closedPgAdapter(t, adapterPgStore(t))
	if err := a.DeleteObjectCopies(context.Background(), "k"); err == nil {
		t.Error("expected error from closed tx")
	}
}

// TestPgAdapterErr_CheckObjectExistsOnBackend verifies the QueryRow
// error path is wrapped (not the benign ErrNoRows path).
func TestPgAdapterErr_CheckObjectExistsOnBackend(t *testing.T) {
	a := closedPgAdapter(t, adapterPgStore(t))
	if _, err := a.CheckObjectExistsOnBackend(context.Background(), "k", "backend-a"); err == nil {
		t.Error("expected error from closed tx")
	}
}

// TestPgAdapterErr_LockObjectOnBackend verifies the Scan error path
// is wrapped (not the benign ErrNoRows path).
func TestPgAdapterErr_LockObjectOnBackend(t *testing.T) {
	a := closedPgAdapter(t, adapterPgStore(t))
	if _, _, err := a.LockObjectOnBackend(context.Background(), "k", "backend-a"); err == nil {
		t.Error("expected error from closed tx")
	}
}

// TestPgAdapterErr_DeleteObjectFromBackend verifies the Exec error is
// wrapped.
func TestPgAdapterErr_DeleteObjectFromBackend(t *testing.T) {
	a := closedPgAdapter(t, adapterPgStore(t))
	if err := a.DeleteObjectFromBackend(context.Background(), "k", "backend-a"); err == nil {
		t.Error("expected error from closed tx")
	}
}

// TestPgAdapterErr_InsertObjectLocationIfNotExists verifies the Exec
// error is wrapped.
func TestPgAdapterErr_InsertObjectLocationIfNotExists(t *testing.T) {
	a := closedPgAdapter(t, adapterPgStore(t))
	_, err := a.InsertObjectLocationIfNotExists(context.Background(), &core.ObjectLocation{
		ObjectKey: "k", BackendName: "backend-a", SizeBytes: 1,
	})
	if err == nil {
		t.Error("expected error from closed tx")
	}
}

// TestPgAdapterErr_InsertReplicaConditional verifies the Exec error
// is wrapped.
func TestPgAdapterErr_InsertReplicaConditional(t *testing.T) {
	a := closedPgAdapter(t, adapterPgStore(t))
	if _, err := a.InsertReplicaConditional(context.Background(), "k", "backend-b", "backend-a"); err == nil {
		t.Error("expected error from closed tx")
	}
}

// -------------------------------------------------------------------------
// CLEANUP TX ERRORS
// -------------------------------------------------------------------------

// TestPgAdapterErr_SumAndDeleteCleanupQueueRows verifies the sum-step
// error is wrapped.
func TestPgAdapterErr_SumAndDeleteCleanupQueueRows(t *testing.T) {
	a := closedPgAdapter(t, adapterPgStore(t))
	if _, _, err := a.SumAndDeleteCleanupQueueRows(context.Background(), "k", "backend-a"); err == nil {
		t.Error("expected error from closed tx")
	}
}

// -------------------------------------------------------------------------
// QUOTA TX ERRORS
// -------------------------------------------------------------------------

// TestPgAdapterErr_IncrementBackendQuota verifies the Exec error is
// wrapped.
func TestPgAdapterErr_IncrementBackendQuota(t *testing.T) {
	a := closedPgAdapter(t, adapterPgStore(t))
	if err := a.IncrementBackendQuota(context.Background(), "backend-a", 100); err == nil {
		t.Error("expected error from closed tx")
	}
}

// TestPgAdapterErr_DecrementBackendQuota verifies the Exec error is
// wrapped.
func TestPgAdapterErr_DecrementBackendQuota(t *testing.T) {
	a := closedPgAdapter(t, adapterPgStore(t))
	if err := a.DecrementBackendQuota(context.Background(), "backend-a", 100); err == nil {
		t.Error("expected error from closed tx")
	}
}

// TestPgAdapterErr_DecrementOrphanBytes verifies the Exec error is
// wrapped.
func TestPgAdapterErr_DecrementOrphanBytes(t *testing.T) {
	a := closedPgAdapter(t, adapterPgStore(t))
	if err := a.DecrementOrphanBytes(context.Background(), "backend-a", 100); err == nil {
		t.Error("expected error from closed tx")
	}
}
