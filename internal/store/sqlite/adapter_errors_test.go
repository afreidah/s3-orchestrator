// -------------------------------------------------------------------------------
// SQLite TxAdapter - Error-Path Coverage
//
// Author: Alex Freidah
//
// Closes the underlying *sql.Tx before calling each adapter method so the
// next query fails deterministically and the error-wrap branch
// (if err != nil { return fmt.Errorf("...", err) }) executes. The
// happy-path tests in adapter_test.go cover the success branches; this
// file pairs every method with a paired failure test so coverage hits
// both paths.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// closedAdapter opens and immediately rolls back a transaction, then
// returns the wrapping adapter. Subsequent calls on the adapter run
// queries against a closed *sql.Tx, which the SQLite driver rejects
// with an error - exercising the wrap-and-return-error path on every
// adapter method.
func closedAdapter(t *testing.T, s *Store) *sqliteTxAdapter {
	t.Helper()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	return &sqliteTxAdapter{tx: tx}
}

// -------------------------------------------------------------------------
// PENDING TX ERRORS
// -------------------------------------------------------------------------

// TestAdapterErr_ClaimPending verifies an underlying DB error is
// wrapped and propagated.
func TestAdapterErr_ClaimPending(t *testing.T) {
	t.Parallel()
	a := closedAdapter(t, newTestStore(t))
	if _, err := a.ClaimPending(context.Background(), "any"); err == nil {
		t.Error("expected error from closed tx")
	}
}

// TestAdapterErr_InsertPending verifies the Exec error is wrapped.
func TestAdapterErr_InsertPending(t *testing.T) {
	t.Parallel()
	a := closedAdapter(t, newTestStore(t))
	err := a.InsertPending(context.Background(), &core.PendingObject{
		IntentID: "i", ObjectKey: "k", BackendName: "backend-a", SizeBytes: 1,
	})
	if err == nil {
		t.Error("expected error from closed tx")
	}
}

// TestAdapterErr_DeletePending verifies the Exec error is wrapped.
func TestAdapterErr_DeletePending(t *testing.T) {
	t.Parallel()
	a := closedAdapter(t, newTestStore(t))
	if err := a.DeletePending(context.Background(), "i"); err == nil {
		t.Error("expected error from closed tx")
	}
}

// TestAdapterErr_DeletePendingByBackend verifies the Exec error is wrapped.
func TestAdapterErr_DeletePendingByBackend(t *testing.T) {
	t.Parallel()
	a := closedAdapter(t, newTestStore(t))
	if err := a.DeletePendingByBackend(context.Background(), "backend-a"); err == nil {
		t.Error("expected error from closed tx")
	}
}

// -------------------------------------------------------------------------
// OBJECTS TX ERRORS
// -------------------------------------------------------------------------

// TestAdapterErr_GetExistingCopiesForUpdate verifies the Query error
// is wrapped.
func TestAdapterErr_GetExistingCopiesForUpdate(t *testing.T) {
	t.Parallel()
	a := closedAdapter(t, newTestStore(t))
	if _, err := a.GetExistingCopiesForUpdate(context.Background(), "k"); err == nil {
		t.Error("expected error from closed tx")
	}
}

// TestAdapterErr_InsertObjectLocation verifies the Exec error is wrapped.
func TestAdapterErr_InsertObjectLocation(t *testing.T) {
	t.Parallel()
	a := closedAdapter(t, newTestStore(t))
	err := a.InsertObjectLocation(context.Background(), &core.ObjectLocation{
		ObjectKey: "k", BackendName: "backend-a", SizeBytes: 1,
	})
	if err == nil {
		t.Error("expected error from closed tx")
	}
}

// TestAdapterErr_DeleteObjectCopies verifies the Exec error is wrapped.
func TestAdapterErr_DeleteObjectCopies(t *testing.T) {
	t.Parallel()
	a := closedAdapter(t, newTestStore(t))
	if err := a.DeleteObjectCopies(context.Background(), "k"); err == nil {
		t.Error("expected error from closed tx")
	}
}

// TestAdapterErr_CheckObjectExistsOnBackend verifies the QueryRow
// scan error path is exercised (closed tx returns a non-ErrNoRows
// error that gets wrapped).
func TestAdapterErr_CheckObjectExistsOnBackend(t *testing.T) {
	t.Parallel()
	a := closedAdapter(t, newTestStore(t))
	if _, err := a.CheckObjectExistsOnBackend(context.Background(), "k", "backend-a"); err == nil {
		t.Error("expected error from closed tx")
	}
}

// TestAdapterErr_LockObjectOnBackend verifies the Scan error is
// wrapped (closed tx, not ErrNoRows).
func TestAdapterErr_LockObjectOnBackend(t *testing.T) {
	t.Parallel()
	a := closedAdapter(t, newTestStore(t))
	if _, _, err := a.LockObjectOnBackend(context.Background(), "k", "backend-a"); err == nil {
		t.Error("expected error from closed tx")
	}
}

// TestAdapterErr_DeleteObjectFromBackend verifies the Exec error is
// wrapped.
func TestAdapterErr_DeleteObjectFromBackend(t *testing.T) {
	t.Parallel()
	a := closedAdapter(t, newTestStore(t))
	if err := a.DeleteObjectFromBackend(context.Background(), "k", "backend-a"); err == nil {
		t.Error("expected error from closed tx")
	}
}

// TestAdapterErr_InsertObjectLocationIfNotExists verifies the
// existence-probe error path is wrapped.
func TestAdapterErr_InsertObjectLocationIfNotExists(t *testing.T) {
	t.Parallel()
	a := closedAdapter(t, newTestStore(t))
	_, err := a.InsertObjectLocationIfNotExists(context.Background(), &core.ObjectLocation{
		ObjectKey: "k", BackendName: "backend-a", SizeBytes: 1,
	})
	if err == nil {
		t.Error("expected error from closed tx")
	}
}

// TestAdapterErr_InsertReplicaConditional verifies the lock-source
// error path is wrapped.
func TestAdapterErr_InsertReplicaConditional(t *testing.T) {
	t.Parallel()
	a := closedAdapter(t, newTestStore(t))
	if _, _, err := a.InsertReplicaConditional(context.Background(), "k", "backend-b", "backend-a"); err == nil {
		t.Error("expected error from closed tx")
	}
}

// -------------------------------------------------------------------------
// CLEANUP TX ERRORS
// -------------------------------------------------------------------------

// TestAdapterErr_SumAndDeleteCleanupQueueRows verifies the sum-step
// error is wrapped.
func TestAdapterErr_SumAndDeleteCleanupQueueRows(t *testing.T) {
	t.Parallel()
	a := closedAdapter(t, newTestStore(t))
	if _, _, err := a.SumAndDeleteCleanupQueueRows(context.Background(), "k", "backend-a"); err == nil {
		t.Error("expected error from closed tx")
	}
}

// -------------------------------------------------------------------------
// QUOTA TX ERRORS
// -------------------------------------------------------------------------

// TestAdapterErr_IncrementBackendQuota verifies the Exec error is
// wrapped.
func TestAdapterErr_IncrementBackendQuota(t *testing.T) {
	t.Parallel()
	a := closedAdapter(t, newTestStore(t))
	if err := a.IncrementBackendQuota(context.Background(), "backend-a", 100); err == nil {
		t.Error("expected error from closed tx")
	}
}

// TestAdapterErr_DecrementBackendQuota verifies the Exec error is
// wrapped.
func TestAdapterErr_DecrementBackendQuota(t *testing.T) {
	t.Parallel()
	a := closedAdapter(t, newTestStore(t))
	if err := a.DecrementBackendQuota(context.Background(), "backend-a", 100); err == nil {
		t.Error("expected error from closed tx")
	}
}

// TestAdapterErr_DecrementOrphanBytes verifies the Exec error is
// wrapped.
func TestAdapterErr_DecrementOrphanBytes(t *testing.T) {
	t.Parallel()
	a := closedAdapter(t, newTestStore(t))
	if err := a.DecrementOrphanBytes(context.Background(), "backend-a", 100); err == nil {
		t.Error("expected error from closed tx")
	}
}
