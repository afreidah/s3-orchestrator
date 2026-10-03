// -------------------------------------------------------------------------------
// Drain Manager - Drain Record Tests
//
// Author: Alex Freidah
//
// Starting, cancelling and reporting a drain against a real SQLite store, so
// each operation is checked by the record it leaves and by what IsDraining and
// admission make of it.
// -------------------------------------------------------------------------------

package drain

import (
	"context"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/sqlite"
)

// newRecordFleet builds a manager over two in-memory backends and a SQLite
// store that knows them both.
func newRecordFleet(t *testing.T) (*Manager, *sqlite.Store) {
	t.Helper()
	store := newSQLiteStore(t, "b1", "b2")
	mgr, _ := newDrainFleet(t, store, map[string]backend.ObjectBackend{
		"b1": backendtest.NewInMemory(),
		"b2": backendtest.NewInMemory(),
	})
	return mgr, store
}

// TestStartDrain_RecordsAndRefusesWrites verifies starting a drain writes a
// draining record, marks the backend draining without waiting for a refresh,
// and makes admission refuse it.
func TestStartDrain_RecordsAndRefusesWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr, store := newRecordFleet(t)

	if err := mgr.StartDrain(ctx, "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}
	if !mgr.IsDraining("b1") || mgr.IsDraining("b2") {
		t.Errorf("IsDraining b1=%v b2=%v, want only b1", mgr.IsDraining("b1"), mgr.IsDraining("b2"))
	}
	p, err := mgr.GetDrainProgress(ctx, "b1")
	if err != nil {
		t.Fatalf("GetDrainProgress: %v", err)
	}
	if !p.Active || p.State != string(core.DrainStateDraining) {
		t.Errorf("progress = %+v, want an active draining record", p)
	}
	ok, err := store.InsertPendingIfFits(ctx, &core.PendingObject{IntentID: "i", ObjectKey: "k", BackendName: "b1", SizeBytes: 1})
	if err != nil || ok {
		t.Errorf("admission on the draining backend = %v, %v; want refused", ok, err)
	}
}

// TestStartDrain_Refusals verifies a drain cannot be started on a backend the
// fleet does not have, or twice on the same backend.
func TestStartDrain_Refusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr, _ := newRecordFleet(t)

	if err := mgr.StartDrain(ctx, "nope"); err == nil {
		t.Error("StartDrain accepted a backend the fleet does not have")
	}
	if err := mgr.StartDrain(ctx, "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}
	if err := mgr.StartDrain(ctx, "b1"); err == nil {
		t.Error("StartDrain accepted a second drain of the same backend")
	}
}

// TestCancelDrain_ClearsTheRecord verifies cancelling removes the record, so
// the backend is writable again and reports no drain.
func TestCancelDrain_ClearsTheRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr, store := newRecordFleet(t)

	if err := mgr.StartDrain(ctx, "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}
	if err := mgr.CancelDrain(ctx, "b1"); err != nil {
		t.Fatalf("CancelDrain: %v", err)
	}
	if mgr.IsDraining("b1") {
		t.Error("IsDraining still true after cancel")
	}
	if p, err := mgr.GetDrainProgress(ctx, "b1"); err != nil || p.State != "" || p.Active {
		t.Errorf("progress after cancel = %+v, %v; want no record", p, err)
	}
	ok, err := store.InsertPendingIfFits(ctx, &core.PendingObject{IntentID: "i", ObjectKey: "k", BackendName: "b1", SizeBytes: 1})
	if err != nil || !ok {
		t.Errorf("admission after cancel = %v, %v; want accepted", ok, err)
	}
	if err := mgr.CancelDrain(ctx, "b1"); err == nil {
		t.Error("CancelDrain succeeded on a backend with no drain")
	}
}

// TestGetDrainProgress_ReportsAFailedDrain verifies a failed record reads back
// inactive with its reason, and that IsDraining still holds the backend back.
func TestGetDrainProgress_ReportsAFailedDrain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr, store := newRecordFleet(t)

	if err := mgr.StartDrain(ctx, "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}
	if err := store.MarkDrainFailed(ctx, "b1", "list failed"); err != nil {
		t.Fatalf("MarkDrainFailed: %v", err)
	}
	if err := mgr.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	p, err := mgr.GetDrainProgress(ctx, "b1")
	if err != nil {
		t.Fatalf("GetDrainProgress: %v", err)
	}
	if p.Active || p.State != string(core.DrainStateFailed) || p.Error != "list failed" {
		t.Errorf("progress = %+v, want inactive, failed, with its reason", p)
	}
	if !mgr.IsDraining("b1") {
		t.Error("a failed drain left the backend open to writes")
	}
}

// TestRemoveBackend_RefusesADrainInProgress verifies a backend still being
// drained cannot be removed, and that removing a drained one clears its record.
func TestRemoveBackend_RefusesADrainInProgress(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr, store := newRecordFleet(t)

	if err := mgr.StartDrain(ctx, "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}
	if err := mgr.RemoveBackend(ctx, "b1", false, nil); err == nil {
		t.Fatal("RemoveBackend removed a backend mid-drain")
	}

	if done, err := store.CompleteDrain(ctx, "b1"); err != nil || !done {
		t.Fatalf("CompleteDrain = %v, %v", done, err)
	}
	if err := mgr.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if err := mgr.RemoveBackend(ctx, "b1", false, nil); err != nil {
		t.Fatalf("RemoveBackend after the drain finished: %v", err)
	}
	if mgr.IsDraining("b1") {
		t.Error("removing the backend left its drain record behind")
	}
}

// TestRemoveBackend_ReadsTheRecordNotTheCache runs two managers over one store,
// the way two instances share a database. A drain started on one must stop the
// other from removing the backend before its cache has refreshed, and a cancel
// on one must let the other remove it.
func TestRemoveBackend_ReadsTheRecordNotTheCache(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	starter, store := newRecordFleet(t)
	remover, _ := newDrainFleet(t, store, map[string]backend.ObjectBackend{
		"b1": backendtest.NewInMemory(),
		"b2": backendtest.NewInMemory(),
	})
	if err := remover.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if err := starter.StartDrain(ctx, "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}
	if err := remover.RemoveBackend(ctx, "b1", false, nil); err == nil {
		t.Fatal("RemoveBackend removed a backend another instance is draining")
	}

	if err := remover.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if err := starter.CancelDrain(ctx, "b1"); err != nil {
		t.Fatalf("CancelDrain: %v", err)
	}
	if err := remover.RemoveBackend(ctx, "b1", false, nil); err != nil {
		t.Fatalf("RemoveBackend after another instance cancelled the drain: %v", err)
	}
}
