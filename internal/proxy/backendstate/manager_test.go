// -------------------------------------------------------------------------------
// Backend State Manager - Drain Record Tests
//
// Author: Alex Freidah
//
// Starting, cancelling and reporting a drain against a real SQLite store, so
// each operation is checked by the record it leaves and by what IsDraining,
// AcceptsWrites and admission make of it.
// -------------------------------------------------------------------------------

package backendstate

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
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
	if mgr.Flags("b1") != core.FlagDraining || mgr.Flags("b2") != 0 {
		t.Errorf("flags b1=%s b2=%s, want only b1 draining", mgr.Flags("b1"), mgr.Flags("b2"))
	}
	p, err := mgr.GetDrainProgress(ctx, "b1")
	if err != nil {
		t.Fatalf("GetDrainProgress: %v", err)
	}
	if !p.Active || p.State != "draining" {
		t.Errorf("progress = %+v, want an active draining record", p)
	}
	ok, err := store.InsertPendingIfFits(ctx, &core.PendingObject{IntentID: "i", ObjectKey: "k", BackendName: "b1", SizeBytes: 1}, core.AcceptForWrite)
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
	if mgr.Flags("b1") != 0 {
		t.Errorf("flags after cancel = %s, want none", mgr.Flags("b1"))
	}
	if p, err := mgr.GetDrainProgress(ctx, "b1"); err != nil || p.State != "" || p.Active {
		t.Errorf("progress after cancel = %+v, %v; want no record", p, err)
	}
	ok, err := store.InsertPendingIfFits(ctx, &core.PendingObject{IntentID: "i", ObjectKey: "k", BackendName: "b1", SizeBytes: 1}, core.AcceptForWrite)
	if err != nil || !ok {
		t.Errorf("admission after cancel = %v, %v; want accepted", ok, err)
	}
	if err := mgr.CancelDrain(ctx, "b1"); err == nil {
		t.Error("CancelDrain succeeded on a backend with no drain")
	}
}

// TestGetDrainProgress_ReportsAFailedDrain verifies a failed record reads back
// inactive with its reason, and that the backend still refuses writes.
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
	if p.Active || p.State != "failed" || p.Error != "list failed" {
		t.Errorf("progress = %+v, want inactive, failed, with its reason", p)
	}
	if mgr.Flags("b1").AcceptedBy(core.AcceptForWrite) {
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
	if mgr.Flags("b1") != 0 {
		t.Error("removing the backend left its state record behind")
	}
}

// TestCordon_HoldsWritesAndSurvivesADrain verifies a cordon refuses admission
// and keeps the backend reconcilable, survives a drain being started and
// cancelled, and that uncordoning leaves a drain running.
func TestCordon_HoldsWritesAndSurvivesADrain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr, store := newRecordFleet(t)

	if err := mgr.Cordon(ctx, "b1"); err != nil {
		t.Fatalf("Cordon: %v", err)
	}
	if f := mgr.Flags("b1"); f != core.FlagCordoned || !f.AcceptedBy(core.AcceptForReconcile) {
		t.Errorf("flags after cordon = %s, want cordoned and reconcilable", f)
	}
	ok, err := store.InsertPendingIfFits(ctx, &core.PendingObject{IntentID: "i", ObjectKey: "k", BackendName: "b1", SizeBytes: 1}, core.AcceptForWrite)
	if err != nil || ok {
		t.Errorf("admission on the cordoned backend = %v, %v; want refused", ok, err)
	}

	if err := mgr.StartDrain(ctx, "b1"); err != nil {
		t.Fatalf("StartDrain on a cordoned backend: %v", err)
	}
	if err := mgr.CancelDrain(ctx, "b1"); err != nil {
		t.Fatalf("CancelDrain: %v", err)
	}
	if f := mgr.Flags("b1"); f != core.FlagCordoned {
		t.Errorf("flags after cancelling the drain = %s, want cordoned only", f)
	}

	if err := mgr.StartDrain(ctx, "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}
	if err := mgr.Uncordon(ctx, "b1"); err != nil {
		t.Fatalf("Uncordon: %v", err)
	}
	if f := mgr.Flags("b1"); f != core.FlagDraining {
		t.Errorf("flags after uncordon = %s, want draining only", f)
	}
}

// TestCordon_RefusesNoOpsAndUnknownBackends verifies cordoning twice,
// uncordoning a backend that is not cordoned, and cordoning a backend the
// fleet does not have all fail without writing a record.
func TestCordon_RefusesNoOpsAndUnknownBackends(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr, _ := newRecordFleet(t)

	if err := mgr.Uncordon(ctx, "b1"); err == nil {
		t.Error("Uncordon succeeded on a backend that is not cordoned")
	}
	if err := mgr.Cordon(ctx, "b1"); err != nil {
		t.Fatalf("Cordon: %v", err)
	}
	if err := mgr.Cordon(ctx, "b1"); err == nil {
		t.Error("a second Cordon succeeded")
	}
	if err := mgr.Cordon(ctx, "nope"); err == nil {
		t.Error("Cordon succeeded on a backend the fleet does not have")
	}
	if mgr.Flags("nope") != 0 {
		t.Error("cordoning an unknown backend left a record")
	}
}

// TestSetStates_PublishesTheStateGauge verifies the gauge carries one series per
// flag a backend holds and drops a backend whose flags are gone. Not parallel:
// the gauge is shared by every manager in the package.
func TestSetStates_PublishesTheStateGauge(t *testing.T) {
	ctx := context.Background()
	mgr, _ := newRecordFleet(t)

	if err := mgr.Cordon(ctx, "b1"); err != nil {
		t.Fatalf("Cordon: %v", err)
	}
	if got := testutil.ToFloat64(telemetry.BackendState.WithLabelValues("b1", "cordoned")); got != 1 {
		t.Errorf("gauge for b1 cordoned = %v, want 1", got)
	}
	if err := mgr.Uncordon(ctx, "b1"); err != nil {
		t.Fatalf("Uncordon: %v", err)
	}
	if n := testutil.CollectAndCount(telemetry.BackendState); n != 0 {
		t.Errorf("gauge series after uncordon = %d, want none", n)
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
